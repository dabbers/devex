package workspace

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
)

// TestRebuildBringsBackWhatTheAgentLeft: on dbz-alpha an agent asked to
// restyle Forms killed the change's web service to restart it, and could not
// bring it back -- the service runs with a database credential and a port only
// the platform holds. Rebuild is what the platform does after every agent turn
// instead, so it has to work from exactly that state: the service gone, and
// the checkout holding work the running binary never saw.
func TestRebuildBringsBackWhatTheAgentLeft(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)
	if err := f.store.ReplaceServices(f.ctx, f.project.ID,
		[]*domain.Service{webService(f.project.ID, freePort(t), "")}); err != nil {
		t.Fatalf("ReplaceServices: %v", err)
	}

	ws, dir := f.start(t, "change", "main")
	t.Cleanup(func() { f.manager.Stop(f.ctx, ws) }) //nolint:errcheck // best effort
	port := allocatedPort(t, f, ws, "web")

	// What the agent did: new work in the checkout, then the service killed.
	if err := os.WriteFile(filepath.Join(dir, "geocities.html"), []byte("under construction"), 0o644); err != nil {
		t.Fatal(err)
	}
	instance, err := f.driver.Get(f.ctx, ws.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	services, err := f.store.ListServices(f.ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.stopServices(f.ctx, instance, services)
	if listening(port) {
		t.Fatal("the service survived being killed, so this proves nothing")
	}

	if err := f.manager.Rebuild(f.ctx, ws); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if !listening(port) {
		t.Fatalf("after a rebuild nothing answers on %d, the port the preview routes to", port)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/geocities.html", port))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "under construction" {
		t.Fatalf("the rebuilt service does not serve what is in the checkout: %d %q", resp.StatusCode, body)
	}
}
