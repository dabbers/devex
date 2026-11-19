package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/dabbers/devex/internal/vm"
	"github.com/dabbers/devex/internal/vm/firecracker/guestproto"
)

// dialAgent opens a connection to a guest's agent through Firecracker's
// host-side vsock socket.
func dialAgent(ctx context.Context, socketPath string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", guestproto.Port); err != nil {
		conn.Close()
		return nil, err
	}
	// The reply is read a byte at a time: anything buffered past its newline
	// would be the agent's first frame, lost to a reader thrown away here.
	var reply []byte
	one := make([]byte, 1)
	for len(reply) < 64 {
		if _, err := conn.Read(one); err != nil {
			conn.Close()
			// Firecracker hangs up when nothing listens on the port yet,
			// which is the normal state of a guest still booting.
			return nil, fmt.Errorf("vsock handshake: %w", err)
		}
		if one[0] == '\n' {
			break
		}
		reply = append(reply, one[0])
	}
	if !strings.HasPrefix(string(reply), "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: unexpected reply %q", reply)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// ping reports whether the guest's agent answers.
func ping(ctx context.Context, socketPath string) error {
	conn, err := dialAgent(ctx, socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := guestproto.NewEncoder(conn).Encode(guestproto.Request{Kind: guestproto.KindPing}); err != nil {
		return err
	}
	var frame guestproto.Frame
	if err := guestproto.NewDecoder(conn).Decode(&frame); err != nil {
		return err
	}
	if frame.Kind != guestproto.FrameExit {
		return fmt.Errorf("ping: unexpected %q frame", frame.Kind)
	}
	return nil
}

// execGrace is how long the host waits past a command's own deadline before
// giving up on the agent reporting it. The agent enforces the deadline; this
// only covers a guest that has stopped answering altogether.
const execGrace = 15 * time.Second

func (d *Driver) exec(ctx context.Context, instanceID string, cmd vm.Command, onLine func([]byte)) (*vm.ExecResult, error) {
	if len(cmd.Argv) == 0 {
		return nil, errors.New("firecracker: exec requires a command")
	}
	m, err := d.running(instanceID)
	if err != nil {
		return nil, err
	}
	inst, err := d.Get(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	timeout := cmd.Timeout
	if timeout <= 0 {
		timeout = d.cfg.ExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+execGrace)
	defer cancel()

	started := time.Now()
	conn, err := dialAgent(ctx, m.vsockPath())
	if err != nil {
		return nil, fmt.Errorf("firecracker: reach agent in %s: %w", instanceID, err)
	}
	defer conn.Close()
	// Hanging up is how a cancelled caller tells the agent to kill the
	// command; the agent treats a closed connection exactly that way.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	req := guestproto.Request{
		Kind:          guestproto.KindExec,
		Argv:          cmd.Argv,
		Dir:           cmd.Dir,
		Env:           mergeEnv(inst.Spec.Env, cmd.Env),
		Stdin:         cmd.Stdin,
		TimeoutMillis: timeout.Milliseconds(),
	}
	if err := guestproto.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("firecracker: send command to %s: %w", instanceID, err)
	}

	var stdout, stderr bytes.Buffer
	tee := &lineTee{sink: &stdout, onLine: onLine}
	dec := guestproto.NewDecoder(conn)
	for {
		var frame guestproto.Frame
		if err := dec.Decode(&frame); err != nil {
			tee.flush()
			if ctx.Err() != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return &vm.ExecResult{
						ExitCode: -1, TimedOut: true,
						Stdout: stdout.String(), Stderr: stderr.String(),
						Duration: time.Since(started),
					}, nil
				}
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("firecracker: agent in %s hung up mid-command: %w", instanceID, err)
		}
		switch frame.Kind {
		case guestproto.FrameStdout:
			_, _ = tee.Write(frame.Data)
		case guestproto.FrameStderr:
			stderr.Write(frame.Data)
		case guestproto.FrameExit:
			tee.flush()
			if frame.Error != "" {
				// The command never ran, which is the driver failing rather
				// than the workload, the same split the local driver makes.
				return nil, fmt.Errorf("firecracker: run %q in %s: %s", cmd.Argv[0], instanceID, frame.Error)
			}
			return &vm.ExecResult{
				ExitCode: frame.Code,
				TimedOut: frame.TimedOut,
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				Duration: time.Since(started),
			}, nil
		}
	}
}

func mergeEnv(base, overlay map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

func (d *Driver) openSession(ctx context.Context, instanceID string, spec vm.SessionSpec) (vm.Session, error) {
	m, err := d.running(instanceID)
	if err != nil {
		return nil, err
	}
	inst, err := d.Get(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	conn, err := dialAgent(ctx, m.vsockPath())
	if err != nil {
		return nil, fmt.Errorf("firecracker: reach agent in %s: %w", instanceID, err)
	}
	enc := guestproto.NewEncoder(conn)
	req := guestproto.Request{
		Kind: guestproto.KindSession,
		Argv: spec.Argv,
		Dir:  spec.Dir,
		Env:  mergeEnv(inst.Spec.Env, spec.Env),
		Cols: spec.Cols,
		Rows: spec.Rows,
	}
	if err := enc.Encode(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("firecracker: open session in %s: %w", instanceID, err)
	}
	output, feed := io.Pipe()
	s := &session{conn: conn, enc: enc, output: output, done: make(chan struct{})}
	go s.pump(guestproto.NewDecoder(conn), feed)
	return s, nil
}

// session is a terminal inside a guest, carried over one agent connection.
type session struct {
	conn   net.Conn
	enc    *guestproto.Encoder
	output *io.PipeReader

	done     chan struct{}
	exitCode int
	err      error
	closing  sync.Once
}

// pump moves the agent's frames into the pipe Read drains, until the exit
// frame or the connection ends.
func (s *session) pump(dec *guestproto.Decoder, feed *io.PipeWriter) {
	defer close(s.done)
	for {
		var frame guestproto.Frame
		if err := dec.Decode(&frame); err != nil {
			s.exitCode, s.err = -1, fmt.Errorf("firecracker: session ended without an exit status: %w", err)
			feed.Close()
			return
		}
		switch frame.Kind {
		case guestproto.FrameStdout, guestproto.FrameStderr:
			if _, err := feed.Write(frame.Data); err != nil {
				// Nobody reads any more; keep draining so the exit status
				// still arrives.
				continue
			}
		case guestproto.FrameExit:
			s.exitCode = frame.Code
			if frame.Error != "" {
				s.err = errors.New("firecracker: session: " + frame.Error)
			}
			feed.Close()
			return
		}
	}
}

func (s *session) Read(p []byte) (int, error) { return s.output.Read(p) }

func (s *session) Write(p []byte) (int, error) {
	if err := s.enc.Encode(guestproto.Frame{Kind: guestproto.FrameStdin, Data: p}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *session) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return errors.New("firecracker: terminal size must be positive")
	}
	return s.enc.Encode(guestproto.Frame{Kind: guestproto.FrameResize, Cols: cols, Rows: rows})
}

func (s *session) Wait() (int, error) {
	<-s.done
	return s.exitCode, s.err
}

// Close ends the session. Dropping the connection is enough: the agent hangs
// up the terminal's process group when its host goes away.
func (s *session) Close() error {
	s.closing.Do(func() {
		s.conn.Close()
		s.output.Close()
	})
	return nil
}

// lineTee captures output in full while handing each complete line to onLine.
type lineTee struct {
	sink    *bytes.Buffer
	onLine  func([]byte)
	partial []byte
}

func (w *lineTee) Write(p []byte) (int, error) {
	w.sink.Write(p)
	if w.onLine == nil {
		return len(p), nil
	}
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.onLine(append([]byte(nil), w.partial[:i]...))
		w.partial = w.partial[i+1:]
	}
	return len(p), nil
}

// flush hands over output that never ended in a newline; it is still a line.
func (w *lineTee) flush() {
	if w.onLine != nil && len(w.partial) > 0 {
		w.onLine(append([]byte(nil), w.partial...))
		w.partial = nil
	}
}
