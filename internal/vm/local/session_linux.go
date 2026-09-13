//go:build linux

package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/dabbers/devex/internal/vm"
)

// OpenSession implements vm.Interactive by running a command on a real
// pseudo-terminal.
//
// A terminal rather than pipes is the point: the web shell exists so a person
// can work inside a machine the way they would over SSH, and editors, pagers
// and progress output all behave differently without one.
//
// The same caveat as the rest of this driver applies and is worse here: the
// session runs as the control-plane user on the host, with no isolation. It is
// for development only.
func (d *Driver) OpenSession(_ context.Context, instanceID string, spec vm.SessionSpec) (vm.Session, error) {
	d.mu.Lock()
	inst, ok := d.instances[instanceID]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("local: %s: %w", instanceID, vm.ErrNotFound)
	}
	if inst.State != vm.StateRunning {
		return nil, fmt.Errorf("local: %s is %s, not running", instanceID, inst.State)
	}

	argv := spec.Argv
	if len(argv) == 0 {
		argv = []string{loginShell()}
	}

	dir := spec.Dir
	if dir == "" {
		dir = inst.Workspace
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(inst.Workspace, dir)
	}

	primary, replica, err := openPTY()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the caller supplies the argv by design
	cmd.Dir = dir
	cmd.Env = append(mergeEnv(inst.Spec.Env, spec.Env), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = replica, replica, replica
	// A new session with the replica as its controlling terminal is what makes
	// job control, signals and window size work at all.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if err := cmd.Start(); err != nil {
		primary.Close()
		replica.Close()
		return nil, fmt.Errorf("local: start session in %s: %w", instanceID, err)
	}
	// The child holds its own copy; keeping ours open would stop the primary
	// ever reporting end-of-file.
	replica.Close()

	session := &ptySession{primary: primary, cmd: cmd}
	if spec.Cols > 0 && spec.Rows > 0 {
		// A size the caller asked for is worth applying, but failing to set it
		// is not worth failing the session over.
		_ = session.Resize(spec.Cols, spec.Rows)
	}
	return session, nil
}

// openPTY allocates a pseudo-terminal pair.
func openPTY() (primary, replica *os.File, err error) {
	primary, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("local: open /dev/ptmx: %w", err)
	}
	defer func() {
		if err != nil {
			primary.Close()
		}
	}()

	// Unlocking has to happen before the replica can be opened.
	if err = unix.IoctlSetPointerInt(int(primary.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		return nil, nil, fmt.Errorf("local: unlock pty: %w", err)
	}
	number, err := unix.IoctlGetInt(int(primary.Fd()), unix.TIOCGPTN)
	if err != nil {
		return nil, nil, fmt.Errorf("local: read pty number: %w", err)
	}

	name := fmt.Sprintf("/dev/pts/%d", number)
	replica, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("local: open %s: %w", name, err)
	}
	return primary, replica, nil
}

// loginShell picks the shell a session opens by default.
func loginShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

// ptySession is one live terminal.
type ptySession struct {
	primary *os.File
	cmd     *exec.Cmd

	once     sync.Once
	waitErr  error
	exitCode int
	done     chan struct{}
	closed   bool
	mu       sync.Mutex
}

// Read implements vm.Session.
func (s *ptySession) Read(p []byte) (int, error) {
	n, err := s.primary.Read(p)
	// A terminal whose child has exited reports EIO rather than EOF. That is
	// the session ending, not a failure, and callers copying from it should
	// see a clean end.
	if err != nil && errors.Is(err, syscall.EIO) {
		return n, io.EOF
	}
	return n, err
}

// Write implements vm.Session.
func (s *ptySession) Write(p []byte) (int, error) { return s.primary.Write(p) }

// Resize implements vm.Session.
func (s *ptySession) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return errors.New("local: terminal size must be positive")
	}
	err := unix.IoctlSetWinsize(int(s.primary.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Col: cols, Row: rows,
	})
	if err != nil {
		return fmt.Errorf("local: resize terminal: %w", err)
	}
	return nil
}

// Wait implements vm.Session.
func (s *ptySession) Wait() (int, error) {
	s.once.Do(func() {
		s.done = make(chan struct{})
		err := s.cmd.Wait()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			s.exitCode = 0
		case errors.As(err, &exitErr):
			s.exitCode = exitErr.ExitCode()
		default:
			s.waitErr = err
		}
		close(s.done)
	})
	<-s.done
	return s.exitCode, s.waitErr
}

// Close implements vm.Session, ending whatever is running.
func (s *ptySession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	if s.cmd.Process != nil {
		// Signal the whole process group: a shell's children should go with
		// it, or a closed tab leaves work running on the machine.
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGHUP)
		_ = s.cmd.Process.Kill()
	}
	return s.primary.Close()
}

// Driver satisfies the interactive capability.
var _ vm.Interactive = (*Driver)(nil)
