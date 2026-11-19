//go:build linux

// Command dabberz-guest is the agent inside every Firecracker guest. It
// listens on vsock and runs what the driver on the host asks it to: plain
// commands for vm.Driver.Exec and terminals for vm.Interactive.
//
// It runs as root inside the guest on purpose. The guest is the isolation
// boundary -- its own kernel, its own disk, its own uid on the host via the
// jailer -- so there is nothing inside it to protect from the workload, and an
// agent that edits a customer's machine needs to be able to install packages
// on it.
//
// It is built static (CGO_ENABLED=0) so the golden image needs nothing for it.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dabbers/devex/internal/vm/firecracker/guestproto"
)

// Workspace is where commands run when the caller names no directory. The
// driver reports the same path as vm.Instance.Workspace.
const Workspace = "/workspace"

func main() {
	log.SetFlags(0)
	log.SetPrefix("dabberz-guest: ")
	listener, err := listenVsock(guestproto.Port)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("listening on vsock port %d", guestproto.Port)
	for {
		conn, err := listener.accept()
		if err != nil {
			log.Printf("accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go serve(conn)
	}
}

// vsockListener accepts vsock connections. The net package has no AF_VSOCK
// support (net.FileListener refuses the socket), so connections are plain
// files; made non-blocking, they go through the runtime poller like any
// socket, and closing one wakes a goroutine blocked reading it.
type vsockListener struct{ fd int }

// listenVsock binds the agent's port on every CID the guest answers to.
func listenVsock(port uint32) (*vsockListener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("bind: %w", err)
	}
	if err := unix.Listen(fd, 64); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("listen: %w", err)
	}
	return &vsockListener{fd: fd}, nil
}

func (l *vsockListener) accept() (io.ReadWriteCloser, error) {
	nfd, _, err := unix.Accept4(l.fd, unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(nfd), "vsock"), nil
}

func serve(conn io.ReadWriteCloser) {
	defer conn.Close()
	dec := guestproto.NewDecoder(conn)
	enc := guestproto.NewEncoder(conn)

	var req guestproto.Request
	if err := dec.Decode(&req); err != nil {
		return
	}
	switch req.Kind {
	case guestproto.KindPing:
		_ = enc.Encode(guestproto.Frame{Kind: guestproto.FrameExit})
	case guestproto.KindExec:
		runExec(req, dec, enc)
	case guestproto.KindSession:
		runSession(req, dec, enc)
	default:
		_ = enc.Encode(guestproto.Frame{Kind: guestproto.FrameExit, Code: -1, Error: fmt.Sprintf("unknown request kind %q", req.Kind)})
	}
}

// command builds the process every request kind shares: its own process
// group, so a timeout or a dropped connection takes its children with it.
func command(ctx context.Context, req guestproto.Request) (*exec.Cmd, error) {
	if len(req.Argv) == 0 {
		return nil, errors.New("a command is required")
	}
	dir := req.Dir
	switch {
	case dir == "":
		dir = Workspace
	case !filepath.IsAbs(dir):
		dir = filepath.Join(Workspace, dir)
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...) //nolint:gosec // running the caller's argv is the agent's whole job
	cmd.Dir = dir
	cmd.Env = environ(req.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// A background child that keeps stdout open must not hold the reply
	// hostage once the command itself has exited.
	cmd.WaitDelay = 2 * time.Second
	return cmd, nil
}

func environ(overlay map[string]string) []string {
	env := map[string]string{
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME": "/root",
		"USER": "root",
		"LANG": "C.UTF-8",
	}
	for k, v := range overlay {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func runExec(req guestproto.Request, dec *guestproto.Decoder, enc *guestproto.Encoder) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if req.TimeoutMillis > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMillis)*time.Millisecond)
		defer cancel()
	}
	// The host hanging up means nobody wants the result any more: a cancelled
	// context on the host side must not leave the command running in here.
	go func() {
		var ignored guestproto.Frame
		for dec.Decode(&ignored) == nil {
		}
		cancel()
	}()

	cmd, err := command(ctx, req)
	if err != nil {
		_ = enc.Encode(guestproto.Frame{Kind: guestproto.FrameExit, Code: -1, Error: err.Error()})
		return
	}
	cmd.Stdin = bytes.NewReader(req.Stdin)
	cmd.Stdout = guestproto.ChunkWriter{Enc: enc, Kind: guestproto.FrameStdout}
	cmd.Stderr = guestproto.ChunkWriter{Enc: enc, Kind: guestproto.FrameStderr}

	err = cmd.Run()
	_ = enc.Encode(exitFrame(ctx, err))
}

func exitFrame(ctx context.Context, err error) guestproto.Frame {
	frame := guestproto.Frame{Kind: guestproto.FrameExit}
	var exitErr *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		frame.TimedOut, frame.Code = true, -1
	case err == nil:
	case errors.As(err, &exitErr):
		frame.Code = exitErr.ExitCode()
	case errors.Is(err, exec.ErrWaitDelay):
		// The command exited cleanly; only a straggler held its output open.
	default:
		frame.Code, frame.Error = -1, err.Error()
	}
	return frame
}

func runSession(req guestproto.Request, dec *guestproto.Decoder, enc *guestproto.Encoder) {
	if len(req.Argv) == 0 {
		req.Argv = []string{"/bin/bash", "-l"}
	}
	primary, replica, err := openPTY()
	if err != nil {
		_ = enc.Encode(guestproto.Frame{Kind: guestproto.FrameExit, Code: -1, Error: err.Error()})
		return
	}
	defer primary.Close()
	if req.Cols > 0 && req.Rows > 0 {
		_ = resize(primary, req.Cols, req.Rows)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, err := command(ctx, req)
	if err != nil {
		replica.Close()
		_ = enc.Encode(guestproto.Frame{Kind: guestproto.FrameExit, Code: -1, Error: err.Error()})
		return
	}
	if req.Env["TERM"] == "" {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = replica, replica, replica
	// A new session with the terminal as its controlling tty is what makes job
	// control and window-size signals work at all.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP) }
	if err := cmd.Start(); err != nil {
		replica.Close()
		_ = enc.Encode(guestproto.Frame{Kind: guestproto.FrameExit, Code: -1, Error: err.Error()})
		return
	}
	replica.Close()

	go func() {
		defer cancel()
		for {
			var frame guestproto.Frame
			if err := dec.Decode(&frame); err != nil {
				return
			}
			switch frame.Kind {
			case guestproto.FrameStdin:
				if _, err := primary.Write(frame.Data); err != nil {
					return
				}
			case guestproto.FrameResize:
				_ = resize(primary, frame.Cols, frame.Rows)
			}
		}
	}()

	var output sync.WaitGroup
	output.Add(1)
	go func() {
		defer output.Done()
		// A terminal whose child has gone reports EIO rather than EOF; either
		// way the output is over.
		_, _ = io.Copy(guestproto.ChunkWriter{Enc: enc, Kind: guestproto.FrameStdout}, primary)
	}()

	err = cmd.Wait()
	output.Wait()
	_ = enc.Encode(exitFrame(context.Background(), err))
}

func openPTY() (primary, replica *os.File, err error) {
	primary, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	if err = unix.IoctlSetPointerInt(int(primary.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		primary.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}
	number, err := unix.IoctlGetInt(int(primary.Fd()), unix.TIOCGPTN)
	if err != nil {
		primary.Close()
		return nil, nil, fmt.Errorf("read pty number: %w", err)
	}
	replica, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		primary.Close()
		return nil, nil, fmt.Errorf("open pty replica: %w", err)
	}
	return primary, replica, nil
}

func resize(primary *os.File, cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	return unix.IoctlSetWinsize(int(primary.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
}
