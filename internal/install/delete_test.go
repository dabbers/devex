package install

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
)

// TestAFailedInstallationCanBeDeletedAndTriedAgain: on dbz-alpha two installs
// failed before the fixes that would have let them succeed, and each kept its
// name, its repository and in one case a provisioned database, with no way to
// remove any of it short of editing the control plane by hand.
func TestAFailedInstallationCanBeDeletedAndTriedAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("provisions a database")
	}
	dsn := os.Getenv(clusterEnv)
	if dsn == "" {
		t.Skip("set " + clusterEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	installer, databases, owner, _, _ := newInstaller(t, dsn)

	installation, err := installer.Install(ctx, Request{UserID: owner.ID, App: "forms", Name: "hiring"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// One that is running holds a customer's data, and is not this.
	if err := installer.Delete(ctx, installation); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a running installation was not refused: %v", err)
	}
	if _, err := databases.store.GetInstallation(ctx, installation.ID); err != nil {
		t.Fatalf("refusing to delete it removed it anyway: %v", err)
	}

	// Made failed by hand. A real failure would take an app in the catalog
	// that breaks partway, and what matters here is the most a failure can
	// leave behind: a database, a repository, and a machine with something
	// still listening on it.
	service, err := url.Parse(serviceAddress(t, databases, installation))
	if err != nil {
		t.Fatal(err)
	}
	installation.State = domain.InstallationFailed
	if err := databases.store.UpdateInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(installer.opts.RepoDir, installation.RepoID+".git")
	if _, err := os.Stat(repository); err != nil {
		t.Fatalf("no repository to remove, so this proves nothing: %v", err)
	}

	if err := installer.Delete(ctx, installation); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := databases.store.GetInstallation(ctx, installation.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the installation is still recorded: %v", err)
	}
	if _, err := databases.store.GetWorkspace(ctx, installation.WorkspaceID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("its workspace is still recorded: %v", err)
	}
	if _, err := os.Stat(repository); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("its repository is still on disk: %v", err)
	}
	var one string
	if err := databases.QueryRow(ctx, installation.Database, "SELECT 'still here'", &one); err == nil {
		t.Fatalf("its database %s is still there", installation.Database)
	}
	if conn, err := net.DialTimeout("tcp", service.Host, time.Second); err == nil {
		conn.Close()
		t.Fatalf("its service is still listening on %s", service.Host)
	}
	// The machine is the one that matters for capacity, and its service dies
	// with its database whether or not anything released it, so it is looked
	// for on disk. This installation was the only one with a machine.
	machines, err := os.ReadDir(filepath.Join(filepath.Dir(installer.opts.RepoDir), "vms"))
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 0 {
		t.Fatalf("its machine is still on disk, holding its share of the host: %s", machines[0].Name())
	}

	// The retry: the name is free again.
	again, err := installer.Install(ctx, Request{UserID: owner.ID, App: "forms", Name: "hiring"})
	if err != nil {
		t.Fatalf("installing again under the same name: %v", err)
	}
	if again.State != domain.InstallationRunning {
		t.Fatalf("the second install is %s", again.State)
	}
}
