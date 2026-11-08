package workspace

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
)

// These tests start real processes that really listen, because the point of
// supervision is whether the port ends up open. A stubbed exec would only
// prove the shell script was assembled.

// freePort reserves a port and releases it, so a service can bind it.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

// webService returns a service that serves HTTP on the given port.
func webService(projectID string, port int, extra string) *domain.Service {
	return &domain.Service{
		ProjectID: projectID, Name: "web", Kind: domain.ServiceWeb,
		Command: fmt.Sprintf("python3 -u -m http.server %d --bind 127.0.0.1 %s", port, extra),
		Port:    port, Exposed: true, Primary: true,
	}
}

func TestStartingAWorkspaceStartsItsServices(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)
	port := freePort(t)

	if err := f.store.ReplaceServices(f.ctx, f.project.ID,
		[]*domain.Service{webService(f.project.ID, port, "")}); err != nil {
		t.Fatalf("ReplaceServices: %v", err)
	}

	ws, _ := f.start(t, "cart", "main")

	// The thing that matters: something is actually listening, so a preview
	// route to this port reaches a server rather than a refused connection.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("nothing is listening on the service's port: %v", err)
	}
	conn.Close()

	services, err := f.store.ListServices(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	status, err := f.manager.Status(f.ctx, ws, services)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(status) != 1 || !status[0].Running || !status[0].Reachable {
		t.Fatalf("status = %+v, want one running and reachable service", status)
	}

	t.Cleanup(func() { f.manager.Stop(f.ctx, ws) }) //nolint:errcheck // best effort
}

// TestStoppingAWorkspaceStopsItsServices is the half that is easy to forget:
// a workspace that stops but leaves its dev server holding the port cannot be
// started again, and nothing reclaims it.
func TestStoppingAWorkspaceStopsItsServices(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)
	port := freePort(t)

	if err := f.store.ReplaceServices(f.ctx, f.project.ID,
		[]*domain.Service{webService(f.project.ID, port, "")}); err != nil {
		t.Fatalf("ReplaceServices: %v", err)
	}
	ws, _ := f.start(t, "cart", "main")

	if err := f.manager.Stop(f.ctx, ws); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Give the signal a moment to take effect.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
		if err != nil {
			return // nothing listening any more, which is the point
		}
		conn.Close()
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the service is still holding its port after the workspace stopped")
}

// TestAServiceThatDiesIsReportedQuickly: a command that exits immediately is
// the commonest mistake in a service definition, and waiting out the full
// health timeout for it would make a typo look like a slow boot.
func TestAServiceThatDiesIsReportedQuickly(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)

	if err := f.store.ReplaceServices(f.ctx, f.project.ID, []*domain.Service{{
		ProjectID: f.project.ID, Name: "web", Kind: domain.ServiceWeb,
		Command: "echo 'cannot find module' >&2; exit 1",
		Port:    freePort(t), Exposed: true, Primary: true,
	}}); err != nil {
		t.Fatalf("ReplaceServices: %v", err)
	}

	ws := &domain.Workspace{
		UserID: f.user.ID, RepoID: f.repo.ID, ProjectID: f.project.ID,
		Name: "cart", Branch: "main",
	}
	if err := f.store.CreateWorkspace(f.ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	started := time.Now()
	_, err := f.manager.Start(f.ctx, ws)
	if err == nil {
		t.Fatal("a workspace whose service died started successfully")
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("took %s to notice a service that exited; the health timeout was waited out", elapsed)
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Fatalf("error %q does not say the service exited", err)
	}

	// And the reason reaches the workspace, not just a log.
	after, err := f.store.GetWorkspace(f.ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if after.State != domain.WorkspaceFailed {
		t.Fatalf("workspace is %q, want failed", after.State)
	}

	// The log is what says why, so it has to be readable afterwards.
	logs, err := f.manager.Logs(f.ctx, after, "web", 50)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(logs, "cannot find module") {
		t.Fatalf("the service log does not carry what it printed: %q", logs)
	}
}

// TestServicesStartInDependencyOrder: a web app brought up before the database
// it reads produces a broken preview for reasons unrelated to the change being
// made.
func TestServicesStartInDependencyOrder(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)
	dbPort, webPort := freePort(t), freePort(t)

	if err := f.store.ReplaceServices(f.ctx, f.project.ID, []*domain.Service{
		{ProjectID: f.project.ID, Name: "web", Kind: domain.ServiceWeb,
			// Refuses to start unless the database is already listening.
			Command: fmt.Sprintf(
				`python3 -c "import socket,sys; s=socket.create_connection(('127.0.0.1',%d),2)" || exit 1; `+
					`python3 -u -m http.server %d --bind 127.0.0.1`, dbPort, webPort),
			Port: webPort, Exposed: true, Primary: true, DependsOn: []string{"db"}},
		{ProjectID: f.project.ID, Name: "db", Kind: domain.ServiceDatastore,
			Command: fmt.Sprintf("python3 -u -m http.server %d --bind 127.0.0.1", dbPort),
			Port:    dbPort},
	}); err != nil {
		t.Fatalf("ReplaceServices: %v", err)
	}

	ws, _ := f.start(t, "cart", "main")
	t.Cleanup(func() { f.manager.Stop(f.ctx, ws) }) //nolint:errcheck // best effort

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", webPort), 2*time.Second)
	if err != nil {
		t.Fatalf("the web service did not come up, so it started before its database: %v", err)
	}
	conn.Close()
}

// TestARestartReplacesTheRunningService: starting a workspace twice must not
// leave two copies of a dev server fighting over one port.
func TestARestartReplacesTheRunningService(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)
	port := freePort(t)

	if err := f.store.ReplaceServices(f.ctx, f.project.ID,
		[]*domain.Service{webService(f.project.ID, port, "")}); err != nil {
		t.Fatalf("ReplaceServices: %v", err)
	}
	ws, _ := f.start(t, "cart", "main")
	t.Cleanup(func() { f.manager.Stop(f.ctx, ws) }) //nolint:errcheck // best effort

	if err := f.manager.Stop(f.ctx, ws); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := f.manager.Start(f.ctx, ws); err != nil {
		t.Fatalf("Start (again): %v", err)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("the service did not come back after a restart: %v", err)
	}
	conn.Close()
}

// TestACommandCarryingQuotesSurvives is the quoting bug in miniature. Go's %q
// yields a double-quoted string, so a command with its own quotes loses them
// when the outer shell re-parses it -- turning one argument into two, and a
// dev server into a confusing failure.
func TestACommandCarryingQuotesSurvives(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)
	port := freePort(t)

	// The quoted argument contains a space. Mangled quoting splits it, and
	// the marker never reaches the log in one piece.
	if err := f.store.ReplaceServices(f.ctx, f.project.ID, []*domain.Service{{
		ProjectID: f.project.ID, Name: "web", Kind: domain.ServiceWeb,
		Command: fmt.Sprintf(
			`python3 -c "import sys; print(' '.join(sys.argv[1:]))" "two words"; `+
				`python3 -u -m http.server %d --bind 127.0.0.1`, port),
		Port: port, Exposed: true, Primary: true,
	}}); err != nil {
		t.Fatalf("ReplaceServices: %v", err)
	}

	ws, _ := f.start(t, "cart", "main")
	t.Cleanup(func() { f.manager.Stop(f.ctx, ws) }) //nolint:errcheck // best effort

	logs, err := f.manager.Logs(f.ctx, ws, "web", 50)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(logs, "two words") {
		t.Fatalf("the quoted argument did not survive: %q", logs)
	}
}

// TestAVariableInACommandStillExpands: the command is handed to the inner
// shell byte for byte, and that shell is what expands. A service command
// referring to an injected secret therefore works as written, which is how
// anyone would expect to write one.
func TestAVariableInACommandStillExpands(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)
	port := freePort(t)

	// HOME is set on every machine by the driver, so it stands in for any
	// variable without needing a vault in this fixture.
	if err := f.store.ReplaceServices(f.ctx, f.project.ID, []*domain.Service{{
		ProjectID: f.project.ID, Name: "web", Kind: domain.ServiceWeb,
		Command: fmt.Sprintf(`echo "home is [$HOME]"; python3 -u -m http.server %d --bind 127.0.0.1`, port),
		Port:    port, Exposed: true, Primary: true,
	}}); err != nil {
		t.Fatalf("ReplaceServices: %v", err)
	}

	ws, _ := f.start(t, "cart", "main")
	t.Cleanup(func() { f.manager.Stop(f.ctx, ws) }) //nolint:errcheck // best effort

	logs, err := f.manager.Logs(f.ctx, ws, "web", 50)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if strings.Contains(logs, "$HOME") {
		t.Fatalf("the variable was passed through literally instead of expanding: %q", logs)
	}
	if !strings.Contains(logs, "home is [/") {
		t.Fatalf("the variable did not expand to a path: %q", logs)
	}
}
