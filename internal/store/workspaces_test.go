package store

import (
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
)

func seedWorkspace(t *testing.T, s *Store, u *domain.User, r *domain.Repo, p *domain.Project, name string) *domain.Workspace {
	t.Helper()
	w := &domain.Workspace{
		UserID: u.ID, RepoID: r.ID, ProjectID: p.ID,
		Name: name, Branch: "dabberz/" + name,
	}
	if err := s.CreateWorkspace(t.Context(), w); err != nil {
		t.Fatalf("CreateWorkspace(%s): %v", name, err)
	}
	return w
}

func TestWorkspaceStartsStopped(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	w := seedWorkspace(t, s, u, r, p, "cart")

	// Recording it as running before a machine exists is how a UI ends up
	// showing a preview that was never there.
	if w.State != domain.WorkspaceStopped {
		t.Fatalf("a new workspace is %s, want stopped", w.State)
	}
	got, err := s.GetWorkspace(ctx, w.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.Branch != "dabberz/cart" || got.State != domain.WorkspaceStopped {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if !strings.HasPrefix(got.ID, "ws_") {
		t.Fatalf("workspace id %q is not a dabberz identifier", got.ID)
	}
}

func TestWorkspaceTransitionsFollowTheStateMachine(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	w := seedWorkspace(t, s, u, r, p, "cart")

	started, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceStopped, domain.WorkspaceStarting, "user started it")
	if err != nil {
		t.Fatalf("stopped -> starting: %v", err)
	}
	if started.StartedAt == nil || started.LastUsedAt == nil {
		t.Fatalf("starting did not record when: %+v", started)
	}

	if _, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceStarting, domain.WorkspaceRunning, "services up"); err != nil {
		t.Fatalf("starting -> running: %v", err)
	}

	// Running straight to stopped skips the shutdown, so it is refused.
	if _, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceRunning, domain.WorkspaceStopped, ""); err == nil {
		t.Fatal("running jumped straight to stopped")
	}

	if _, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceRunning, domain.WorkspaceStopping, "user stopped it"); err != nil {
		t.Fatalf("running -> stopping: %v", err)
	}
	stopped, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceStopping, domain.WorkspaceStopped, "")
	if err != nil {
		t.Fatalf("stopping -> stopped: %v", err)
	}
	if stopped.StoppedAt == nil {
		t.Fatalf("stopping did not record when: %+v", stopped)
	}
}

// TestOnlyOneStartWins is why transitions are compare-and-set. Two requests to
// start the same workspace would otherwise both see it stopped and both boot a
// machine, and nothing is reclaimed automatically, so the loser is stranded.
func TestOnlyOneStartWins(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	w := seedWorkspace(t, s, u, r, p, "cart")

	if _, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceStopped, domain.WorkspaceStarting, "first"); err != nil {
		t.Fatalf("first start: %v", err)
	}
	_, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceStopped, domain.WorkspaceStarting, "second")
	if err == nil {
		t.Fatal("a second start was accepted; two machines would be booted")
	}
	if !strings.Contains(err.Error(), "is starting, not stopped") {
		t.Fatalf("error %q does not say what actually happened", err)
	}
}

// TestFailedCanBeStartedAgain: the usual cause of a failed start is a service
// command that needs fixing, and the point of a workspace is that you fix it
// and start it again.
func TestFailedCanBeStartedAgain(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	w := seedWorkspace(t, s, u, r, p, "cart")

	if _, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceStopped, domain.WorkspaceStarting, ""); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceStarting, domain.WorkspaceFailed, "npm run dev exited"); err != nil {
		t.Fatalf("starting -> failed: %v", err)
	}
	if domain.WorkspaceFailed.Terminal() {
		t.Fatal("failed is terminal; a workspace you cannot retry is a job, not a place")
	}
	if _, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceFailed, domain.WorkspaceStarting, "retry"); err != nil {
		t.Fatalf("failed -> starting: %v", err)
	}
}

func TestArchivedIsTerminal(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	w := seedWorkspace(t, s, u, r, p, "cart")

	if _, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceStopped, domain.WorkspaceArchived, "done with it"); err != nil {
		t.Fatalf("stopped -> archived: %v", err)
	}
	if !domain.WorkspaceArchived.Terminal() {
		t.Fatal("archived should be terminal")
	}
	if _, err := s.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceArchived, domain.WorkspaceStarting, ""); err == nil {
		t.Fatal("an archived workspace was started")
	}
}

// TestListingHidesScratchWorkspaces: a person's list of places to work should
// not fill up with machines a batch run made for itself.
func TestListingHidesScratchWorkspaces(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)

	mine := seedWorkspace(t, s, u, r, p, "mine")
	scratch := seedWorkspace(t, s, u, r, p, "scratch")
	scratch.Ephemeral = true
	if err := s.UpdateWorkspace(ctx, scratch); err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}

	listed, err := s.ListWorkspaces(ctx, WorkspaceFilter{UserID: u.ID})
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != mine.ID {
		t.Fatalf("listing = %+v, want only the non-ephemeral workspace", listed)
	}

	all, err := s.ListWorkspaces(ctx, WorkspaceFilter{UserID: u.ID, IncludeEphemeral: true})
	if err != nil {
		t.Fatalf("ListWorkspaces(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d workspaces including scratch, want 2", len(all))
	}
}

// TestListingOrdersByLastUsed makes the list read as "where you were working".
func TestListingOrdersByLastUsed(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)

	first := seedWorkspace(t, s, u, r, p, "first")
	second := seedWorkspace(t, s, u, r, p, "second")

	// Never-used workspaces still sort, by creation, rather than falling to
	// the bottom under a NULL.
	listed, err := s.ListWorkspaces(ctx, WorkspaceFilter{UserID: u.ID})
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("got %d workspaces, want 2", len(listed))
	}

	time.Sleep(2 * time.Millisecond)
	if err := s.TouchWorkspace(ctx, first.ID); err != nil {
		t.Fatalf("TouchWorkspace: %v", err)
	}
	listed, err = s.ListWorkspaces(ctx, WorkspaceFilter{UserID: u.ID})
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if listed[0].ID != first.ID {
		t.Fatalf("most recently used is %s, want %s (%s)", listed[0].Name, first.Name, second.Name)
	}
}

func TestWorkspacesAreScopedByUser(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	seedWorkspace(t, s, u, r, p, "mine")

	other, err := s.EnsureUser(ctx, "someone-else@example.com")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	listed, err := s.ListWorkspaces(ctx, WorkspaceFilter{UserID: other.ID})
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("another user can see %d workspaces: %+v", len(listed), listed)
	}
}

func TestUnknownWorkspaceStateIsRefused(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	w := &domain.Workspace{
		UserID: u.ID, RepoID: r.ID, ProjectID: p.ID,
		Name: "bad", Branch: "b", State: domain.WorkspaceState("sideways"),
	}
	if err := s.CreateWorkspace(ctx, w); err == nil {
		t.Fatal("a workspace with an unknown state was stored")
	}
}
