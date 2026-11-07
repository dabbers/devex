package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/auth"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
)

func TestCreatingAWorkspaceDefaultsToTheRepoBranchAndStaysStopped(t *testing.T) {
	f := newFixture(t)
	withProject(t, f)

	var created struct {
		Workspace domain.Workspace `json:"workspace"`
	}
	status := f.call(t, "POST", "/v1/workspaces", map[string]any{
		"repo_id": f.repo.ID, "name": "cart work",
	}, &created)
	if status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}

	// A workspace is somewhere you work, and working on the default branch is
	// a legitimate thing to want -- unlike for a fork.
	if created.Workspace.Branch != f.repo.DefaultBranch {
		t.Fatalf("branch = %q, want the repo default %q",
			created.Workspace.Branch, f.repo.DefaultBranch)
	}
	// Nothing was asked to start it, so nothing is running.
	if created.Workspace.State != domain.WorkspaceStopped {
		t.Fatalf("state = %q, want stopped", created.Workspace.State)
	}
	if created.Workspace.Ephemeral {
		t.Fatal("a workspace a person opened should not be marked ephemeral")
	}
}

func TestCreatingAWorkspaceNeedsAName(t *testing.T) {
	f := newFixture(t)
	withProject(t, f)
	status := f.call(t, "POST", "/v1/workspaces", map[string]any{
		"repo_id": f.repo.ID, "name": "   ",
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

// TestAmbiguousProjectIsRefused: picking one would be a guess, and the wrong
// guess means a workspace running the wrong services.
func TestAmbiguousProjectIsRefused(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"apps/web", "services/api"} {
		if err := f.store.CreateProject(f.ctx, &domain.Project{
			RepoID: f.repo.ID, Name: path, Path: path,
		}); err != nil {
			t.Fatalf("CreateProject(%s): %v", path, err)
		}
	}

	var body map[string]any
	status := f.call(t, "POST", "/v1/workspaces", map[string]any{
		"repo_id": f.repo.ID, "name": "ambiguous",
	}, &body)
	if status < 400 {
		t.Fatalf("status = %d, want a refusal", status)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "say which one") {
		t.Fatalf("refusal %v does not ask which project", body)
	}
}

func TestListingHidesAnotherUsersWorkspaces(t *testing.T) {
	f := newFixture(t)
	withProject(t, f)
	if status := f.call(t, "POST", "/v1/workspaces", map[string]any{
		"repo_id": f.repo.ID, "name": "mine",
	}, nil); status != http.StatusCreated {
		t.Fatalf("create: %d", status)
	}

	// A different person, same server.
	f.api.authn = auth.Single{Subject: "prov|intruder", Email: "intruder@example.com"}
	var listed struct {
		Workspaces []domain.Workspace `json:"workspaces"`
	}
	if status := f.call(t, "GET", "/v1/workspaces", nil, &listed); status != http.StatusOK {
		t.Fatalf("list: %d", status)
	}
	if len(listed.Workspaces) != 0 {
		t.Fatalf("another user sees %d workspaces: %+v", len(listed.Workspaces), listed.Workspaces)
	}
}

// TestAnotherUsersWorkspaceReadsAsMissing: reported as not found rather than
// forbidden, so the API cannot be used to learn that somebody else's
// workspace exists.
func TestAnotherUsersWorkspaceReadsAsMissing(t *testing.T) {
	f := newFixture(t)
	withProject(t, f)
	var created struct {
		Workspace domain.Workspace `json:"workspace"`
	}
	if status := f.call(t, "POST", "/v1/workspaces", map[string]any{
		"repo_id": f.repo.ID, "name": "mine",
	}, &created); status != http.StatusCreated {
		t.Fatalf("create: %d", status)
	}

	f.api.authn = auth.Single{Subject: "prov|intruder", Email: "intruder@example.com"}
	for _, call := range []struct{ method, path string }{
		{"GET", "/v1/workspaces/" + created.Workspace.ID},
		{"POST", "/v1/workspaces/" + created.Workspace.ID + "/start"},
		{"POST", "/v1/workspaces/" + created.Workspace.ID + "/stop"},
		{"POST", "/v1/workspaces/" + created.Workspace.ID + "/archive"},
	} {
		status := f.call(t, call.method, call.path, map[string]any{}, nil)
		if status != http.StatusNotFound {
			t.Fatalf("%s %s returned %d, want 404", call.method, call.path, status)
		}
	}
}

func TestArchivingRequiresStopping(t *testing.T) {
	f := newFixture(t)
	withProject(t, f)
	var created struct {
		Workspace domain.Workspace `json:"workspace"`
	}
	if status := f.call(t, "POST", "/v1/workspaces", map[string]any{
		"repo_id": f.repo.ID, "name": "cart",
	}, &created); status != http.StatusCreated {
		t.Fatalf("create: %d", status)
	}

	// Put it in a state that holds a machine.
	if _, err := f.store.TransitionWorkspace(f.ctx, created.Workspace.ID,
		domain.WorkspaceStopped, domain.WorkspaceStarting, ""); err != nil {
		t.Fatalf("TransitionWorkspace: %v", err)
	}
	status := f.call(t, "POST", "/v1/workspaces/"+created.Workspace.ID+"/archive", map[string]any{}, nil)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while it still holds a machine", status)
	}
}

func TestWorkspaceLifecycleIsAudited(t *testing.T) {
	f := newFixture(t)
	withProject(t, f)
	var created struct {
		Workspace domain.Workspace `json:"workspace"`
	}
	if status := f.call(t, "POST", "/v1/workspaces", map[string]any{
		"repo_id": f.repo.ID, "name": "cart",
	}, &created); status != http.StatusCreated {
		t.Fatalf("create: %d", status)
	}

	events, err := f.store.ListEvents(f.ctx, store.EventFilter{WorkspaceID: created.Workspace.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("creating a workspace was not recorded")
	}
	// Scoped to the workspace, so "what happened in here" is answerable
	// without reading every event and guessing from the message.
	if events[0].Type != domain.EventWorkspaceCreated {
		t.Fatalf("event type = %q, want workspace.created", events[0].Type)
	}
	if events[0].Actor != domain.ActorUser {
		t.Fatalf("actor = %q, want the user", events[0].Actor)
	}
}

func TestShellRefusesAWorkspaceWithNoMachine(t *testing.T) {
	f := newFixture(t)
	withProject(t, f)
	var created struct {
		Workspace domain.Workspace `json:"workspace"`
	}
	if status := f.call(t, "POST", "/v1/workspaces", map[string]any{
		"repo_id": f.repo.ID, "name": "cart",
	}, &created); status != http.StatusCreated {
		t.Fatalf("create: %d", status)
	}

	var body map[string]any
	status := f.call(t, "GET", "/v1/workspaces/"+created.Workspace.ID+"/shell", nil, &body)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	// The message differs from a sub-task's on purpose: a workspace is not
	// running, it has not failed to provision.
	if message, _ := body["error"].(string); !strings.Contains(message, "not running") {
		t.Fatalf("message %v does not say the workspace is stopped", body)
	}
}

// withProject gives the fixture's repo a project, since a workspace runs a
// project's services and a repo with none cannot have one.
func withProject(t *testing.T, f *fixture) *domain.Project {
	t.Helper()
	project := &domain.Project{RepoID: f.repo.ID, Name: "web", Path: ".", Confirmed: true}
	if err := f.store.CreateProject(f.ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return project
}

// TestRepoWithNoProjectsCannotHaveAWorkspace: a workspace runs a project's
// services, so one without a project would have nothing to run.
func TestRepoWithNoProjectsCannotHaveAWorkspace(t *testing.T) {
	f := newFixture(t)
	var body map[string]any
	status := f.call(t, "POST", "/v1/workspaces", map[string]any{
		"repo_id": f.repo.ID, "name": "cart",
	}, &body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "discovery") {
		t.Fatalf("refusal %v does not say what to do about it", body)
	}
}
