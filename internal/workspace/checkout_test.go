package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/preview"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/vm"
	"github.com/dabbers/devex/internal/vm/local"
)

// These tests use real git against a real repository on disk, through the
// local driver. A stubbed git would only prove that the right strings were
// assembled; what is worth knowing is whether a second start preserves
// uncommitted work and whether a new branch actually gets created.

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=dabberz", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=dabberz", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// originRepo builds a repository with a main branch and one commit.
func originRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "--initial-branch=main", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("shoppe\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "--quiet", "-m", "first")
	return dir
}

type checkoutFixture struct {
	manager *Manager
	store   *store.Store
	driver  *local.Driver
	repo    *domain.Repo
	project *domain.Project
	user    *domain.User
	ctx     context.Context
}

func newCheckoutFixture(t *testing.T, remote string) *checkoutFixture {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	driver, err := local.New(local.Options{
		Root:  t.TempDir(),
		Total: vm.Resources{VCPUs: 8, MemoryMiB: 8192, DiskGiB: 100},
	})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	user, err := st.EnsureUser(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	repo := &domain.Repo{
		UserID: user.ID, Name: "shoppe", RemoteURL: remote, DefaultBranch: "main",
	}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	project := &domain.Project{RepoID: repo.ID, Name: "app", Path: "."}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	alloc, err := preview.New(st, nil, preview.Config{Domain: "ondabberz.link", Router: preview.RouterNone})
	if err != nil {
		t.Fatalf("preview.New: %v", err)
	}
	manager, err := New(Deps{Store: st, Driver: driver, Preview: alloc}, Options{
		Resources: vm.Resources{VCPUs: 1, MemoryMiB: 512, DiskGiB: 2},
		Image:     "dabberz-golden",
	})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	return &checkoutFixture{
		manager: manager, store: st, driver: driver,
		repo: repo, project: project, user: user, ctx: ctx,
	}
}

func (f *checkoutFixture) start(t *testing.T, name, branch string) (*domain.Workspace, string) {
	t.Helper()
	ws := &domain.Workspace{
		UserID: f.user.ID, RepoID: f.repo.ID, ProjectID: f.project.ID,
		Name: name, Branch: branch,
	}
	if err := f.store.CreateWorkspace(f.ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	started, err := f.manager.Start(f.ctx, ws)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return ws, started.Instance.Workspace
}

func TestStartingAWorkspaceClonesTheRepository(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)

	_, dir := f.start(t, "cart", "main")

	// The repository lands at the workspace root, which is what every path in
	// a service command assumes.
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatalf("the repository was not checked out at the workspace root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("no git directory: %v", err)
	}
	if branch := strings.TrimSpace(gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD")); branch != "main" {
		t.Fatalf("on branch %q, want main", branch)
	}
}

// TestANewBranchIsStartedFromTheDefault: a workspace on a branch nobody has
// pushed yet is the ordinary case, and starting it from whatever happened to
// be checked out would put unrelated work on it.
func TestANewBranchIsStartedFromTheDefault(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)

	_, dir := f.start(t, "cart", "dabberz/cart-work")

	if branch := strings.TrimSpace(gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD")); branch != "dabberz/cart-work" {
		t.Fatalf("on branch %q, want the workspace's", branch)
	}
	// Started from the default, so it carries its commit.
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatalf("the new branch does not carry the default branch's work: %v", err)
	}
}

func TestAnExistingRemoteBranchIsTracked(t *testing.T) {
	origin := originRepo(t)
	gitRun(t, origin, "checkout", "--quiet", "-b", "feature/ratings")
	if err := os.WriteFile(filepath.Join(origin, "ratings.txt"), []byte("stars\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, origin, "add", ".")
	gitRun(t, origin, "commit", "--quiet", "-m", "ratings")
	gitRun(t, origin, "checkout", "--quiet", "main")

	f := newCheckoutFixture(t, origin)
	_, dir := f.start(t, "ratings", "feature/ratings")

	if _, err := os.Stat(filepath.Join(dir, "ratings.txt")); err != nil {
		t.Fatalf("the remote branch's work is missing: %v", err)
	}
}

// TestRestartingPreservesUncommittedWork is the property that makes a
// workspace a place rather than a job. The disk is the work: a second start
// that reset, re-cloned or cleaned would throw away whatever was in progress.
func TestRestartingPreservesUncommittedWork(t *testing.T) {
	origin := originRepo(t)
	f := newCheckoutFixture(t, origin)

	ws, dir := f.start(t, "cart", "main")

	scratch := filepath.Join(dir, "in-progress.txt")
	if err := os.WriteFile(scratch, []byte("half an idea\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("edited\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := f.manager.Stop(f.ctx, ws); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := f.manager.Start(f.ctx, ws); err != nil {
		t.Fatalf("Start (again): %v", err)
	}

	untracked, err := os.ReadFile(scratch)
	if err != nil {
		t.Fatalf("an untracked file was destroyed by restarting: %v", err)
	}
	if string(untracked) != "half an idea\n" {
		t.Fatalf("untracked file = %q", untracked)
	}
	edited, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil || string(edited) != "edited\n" {
		t.Fatalf("an uncommitted edit was reverted by restarting: %q (%v)", edited, err)
	}
}

func TestARepositoryWithNoRemoteIsRefusedClearly(t *testing.T) {
	f := newCheckoutFixture(t, "")

	ws := &domain.Workspace{
		UserID: f.user.ID, RepoID: f.repo.ID, ProjectID: f.project.ID,
		Name: "cart", Branch: "main",
	}
	if err := f.store.CreateWorkspace(f.ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	_, err := f.manager.Start(f.ctx, ws)
	if err == nil {
		t.Fatal("a repository with no remote was cloned")
	}
	if !strings.Contains(err.Error(), "no remote") {
		t.Fatalf("error %q does not name the problem", err)
	}
	// And the failure is on the workspace, not only in a log.
	after, err := f.store.GetWorkspace(f.ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if after.State != domain.WorkspaceFailed {
		t.Fatalf("workspace is %q, want failed", after.State)
	}
	if !strings.Contains(after.StateReason, "remote") {
		t.Fatalf("state reason %q does not say what went wrong", after.StateReason)
	}
}
