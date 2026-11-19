package workspace

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
)

// listening reports whether something accepts connections on port within a
// few seconds.
func listening(port int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestARestartBringsBackWhatWasRunning: stopping the daemon stops every
// service it started while the records still say running, and on dbz-alpha
// every deploy left each installation answering 502 until somebody restarted
// each workspace by hand.
func TestARestartBringsBackWhatWasRunning(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)
	port := freePort(t)
	if err := f.store.ReplaceServices(f.ctx, f.project.ID,
		[]*domain.Service{webService(f.project.ID, port, "")}); err != nil {
		t.Fatalf("ReplaceServices: %v", err)
	}

	live, dir := f.start(t, "live", "main")
	t.Cleanup(func() { f.manager.Stop(f.ctx, live) }) //nolint:errcheck // best effort
	allocated := allocatedPort(t, f, live, "web")

	// What a rolled-back installation and a change in progress have on their
	// disks: a revision that is not the branch tip on the remote, and work
	// nobody has committed. A resume that fetched over or reset either would
	// silently undo a rollback or throw an agent's work away.
	gitRun(t, dir, "commit", "--quiet", "--allow-empty", "-m", "only here")
	here := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(dir, "draft.txt"), []byte("not committed"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A workspace somebody stopped on purpose stays stopped.
	parked, _ := f.start(t, "parked", "main")
	parkedPort := allocatedPort(t, f, parked, "web")
	if err := f.manager.Stop(f.ctx, parked); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The daemon dies: its services go with it, and nothing updates a record.
	instance, err := f.driver.Get(f.ctx, live.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	services, err := f.store.ListServices(f.ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.stopServices(f.ctx, instance, services)
	if listening(allocated) {
		t.Fatal("the service survived the simulated restart, so this proves nothing")
	}

	if err := f.manager.Resume(f.ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if !listening(allocated) {
		t.Fatal("after a restart the workspace is recorded as running and nothing is listening")
	}
	if got := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD")); got != here {
		t.Fatalf("resuming moved the checkout from %s to %s", here, got)
	}
	if _, err := os.Stat(filepath.Join(dir, "draft.txt")); err != nil {
		t.Fatalf("resuming threw away uncommitted work: %v", err)
	}
	again, err := f.store.GetWorkspace(f.ctx, parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.State != domain.WorkspaceStopped {
		t.Fatalf("a workspace stopped on purpose is %s after a restart", again.State)
	}
	if parkedPort != allocated {
		if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", parkedPort), 300*time.Millisecond); err == nil {
			conn.Close()
			t.Fatal("a workspace stopped on purpose was started by the restart")
		}
	}
}
