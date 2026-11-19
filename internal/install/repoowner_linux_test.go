//go:build linux

package install

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/dabbers/devex/internal/store"
)

// TestTheWorkspaceAccountCanReachTheRepositories: workspaces clone from and
// promote by pushing to these repositories, as the account the driver runs
// them as. On dbz-alpha the repositories had been handed to that account but
// their directory was still 0700 root, so the first promote failed with "does
// not appear to be a git repository". Needs root, as dropping to another
// account does.
func TestTheWorkspaceAccountCanReachTheRepositories(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("running git as another account needs root; run this package under sudo")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("no 'nobody' account: %v", err)
	}
	uid, _ := strconv.Atoi(account.Uid)
	gid, _ := strconv.Atoi(account.Gid)

	parent, err := os.MkdirTemp("/tmp", "dabberz-repos-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	// As a host set up before RepoOwner existed has it: root's, and closed.
	repoDir := filepath.Join(parent, "repos")
	if err := os.Mkdir(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(repoDir, "repo_x.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "dabberz.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	catalog, err := OpenCatalog("../../apps")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Deps{Store: db, Catalog: catalog}, Options{
		RepoDir: repoDir, RepoOwner: &Owner{UID: uid, GID: gid},
	}); err != nil {
		t.Fatalf("New: %v", err)
	}

	asWorkspace := exec.Command("git", "ls-remote", bare)
	asWorkspace.Dir = "/tmp" // the test's own directory is not the account's to enter
	asWorkspace.Env = []string{"HOME=/tmp", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=" + os.Getenv("PATH")}
	asWorkspace.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{},
	}}
	if out, err := asWorkspace.CombinedOutput(); err != nil {
		t.Fatalf("the workspace account cannot read the repository: %v: %s", err, out)
	}
}
