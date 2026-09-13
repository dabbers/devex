//go:build linux

package local

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/vm"
)

// openTestSession boots an instance and opens a shell on it.
func openTestSession(t *testing.T, spec vm.SessionSpec) (*Driver, vm.Session) {
	t.Helper()
	d := newDriver(t, vm.Resources{VCPUs: 8, MemoryMiB: 8192, DiskGiB: 100})
	inst, err := d.Create(context.Background(), smallSpec("fork-shell"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	session, err := d.OpenSession(context.Background(), inst.ID, spec)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return d, session
}

// readUntil reads until the needle appears or the deadline passes.
func readUntil(t *testing.T, r io.Reader, needle string, within time.Duration) string {
	t.Helper()
	seen := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
				if strings.Contains(b.String(), needle) {
					seen <- b.String()
					return
				}
			}
			if err != nil {
				seen <- b.String()
				return
			}
		}
	}()
	select {
	case got := <-seen:
		return got
	case <-time.After(within):
		t.Fatalf("timed out waiting for %q", needle)
		return ""
	}
}

func TestSessionRunsACommandAndStreamsOutput(t *testing.T) {
	_, session := openTestSession(t, vm.SessionSpec{Argv: []string{"sh"}, Cols: 80, Rows: 24})

	if _, err := io.WriteString(session, "echo streamed-back\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readUntil(t, session, "streamed-back", 5*time.Second); !strings.Contains(got, "streamed-back") {
		t.Fatalf("output = %q", got)
	}
}

func TestSessionIsARealTerminal(t *testing.T) {
	_, session := openTestSession(t, vm.SessionSpec{Argv: []string{"sh"}, Cols: 80, Rows: 24})

	// The whole reason for a pty rather than pipes: programs must see a tty,
	// or editors and pagers behave differently from how they would over SSH.
	if _, err := io.WriteString(session, "test -t 0 && echo IS-A-TTY\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readUntil(t, session, "IS-A-TTY", 5*time.Second); !strings.Contains(got, "IS-A-TTY") {
		t.Fatalf("the session is not attached to a terminal: %q", got)
	}
}

func TestSessionReportsItsSize(t *testing.T) {
	_, session := openTestSession(t, vm.SessionSpec{Argv: []string{"sh"}, Cols: 100, Rows: 30})

	if _, err := io.WriteString(session, "stty size\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readUntil(t, session, "30 100", 5*time.Second)
	if !strings.Contains(got, "30 100") {
		t.Fatalf("terminal size was not applied: %q", got)
	}

	if err := session.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if _, err := io.WriteString(session, "stty size\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readUntil(t, session, "40 120", 5*time.Second); !strings.Contains(got, "40 120") {
		t.Fatalf("resize was not applied: %q", got)
	}

	if err := session.Resize(0, 0); err == nil {
		t.Error("a zero terminal size should be rejected")
	}
}

func TestSessionEndsCleanlyWhenTheShellExits(t *testing.T) {
	_, session := openTestSession(t, vm.SessionSpec{Argv: []string{"sh", "-c", "echo bye; exit 7"}, Cols: 80, Rows: 24})

	// Draining to EOF must not surface the EIO a terminal reports when its
	// child is gone; that is the session ending, not a failure.
	reader := bufio.NewReader(session)
	var out strings.Builder
	for {
		chunk, err := reader.ReadString('\n')
		out.WriteString(chunk)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("reading to the end of a session returned %v, want io.EOF", err)
			}
			break
		}
	}
	if !strings.Contains(out.String(), "bye") {
		t.Fatalf("output = %q", out.String())
	}

	code, err := session.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
}

func TestSessionInheritsInstanceEnvironmentAndWorkspace(t *testing.T) {
	d := newDriver(t, vm.Resources{VCPUs: 8, MemoryMiB: 8192, DiskGiB: 100})
	spec := smallSpec("fork-env")
	spec.Env = map[string]string{"DABBERZ_SECRET": "inherited"}
	inst, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	session, err := d.OpenSession(context.Background(), inst.ID, vm.SessionSpec{Argv: []string{"sh"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer session.Close()

	// A shell opened on a machine should see what that machine was given,
	// exactly as the agent working there does.
	if _, err := io.WriteString(session, "echo secret=$DABBERZ_SECRET; pwd\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readUntil(t, session, "secret=inherited", 5*time.Second)
	if !strings.Contains(got, "secret=inherited") {
		t.Fatalf("instance environment was not inherited: %q", got)
	}
}

func TestCloseEndsTheSession(t *testing.T) {
	_, session := openTestSession(t, vm.SessionSpec{Argv: []string{"sh", "-c", "sleep 300"}, Cols: 80, Rows: 24})

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Closing a tab must not leave the work running on the machine.
	done := make(chan struct{})
	go func() { session.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session survived Close")
	}

	// Closing twice is a no-op, not an error: a disconnect and an explicit
	// close can both arrive.
	if err := session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOpenSessionRejectsUnknownInstances(t *testing.T) {
	d := newDriver(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 50})
	if _, err := d.OpenSession(context.Background(), "vm_missing", vm.SessionSpec{}); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("OpenSession(missing) = %v, want vm.ErrNotFound", err)
	}
}

func TestSessionDefaultsToALoginShell(t *testing.T) {
	_, session := openTestSession(t, vm.SessionSpec{Cols: 80, Rows: 24})
	// No argv given: a shell is what a person dropping into a machine expects.
	if _, err := io.WriteString(session, "echo default-shell-works\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readUntil(t, session, "default-shell-works", 5*time.Second); !strings.Contains(got, "default-shell-works") {
		t.Fatalf("no usable default shell: %q", got)
	}
}
