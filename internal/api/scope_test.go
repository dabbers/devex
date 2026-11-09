package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/auth"
	"github.com/dabbers/devex/internal/domain"
)

// TestEveryRouteNamingSomebodysResourceRefusesEveryoneElse walks the route
// table the server actually registered, rather than a list written here that
// would drift from it, and checks each route that names an owned resource.
//
// A missed check on one of these is not a cosmetic leak: /v1/forks/{fork}/shell
// is a terminal on the machine holding that user's checkout.
func TestEveryRouteNamingSomebodysResourceRefusesEveryoneElse(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))
	withProject(t, f)

	task, forks := f.startTask(t, "add ratings")
	var workspace struct {
		Workspace domain.Workspace `json:"workspace"`
	}
	if status := f.call(t, "POST", "/v1/workspaces", map[string]any{
		"repo_id": f.repo.ID, "name": "mine",
	}, &workspace); status != http.StatusCreated {
		t.Fatalf("creating a workspace: %d", status)
	}

	ids := map[string]string{
		"repo":      f.repo.ID,
		"task":      task.ID,
		"fork":      forks[0].ID,
		"workspace": workspace.Workspace.ID,
	}
	// Stand-ins for the path values that do not name an owned resource. The
	// gate runs before the handler, so these are never reached on a refusal.
	placeholders := map[string]string{
		"name":    "SOME_SECRET",
		"slug":    "some-finding",
		"service": "web",
	}

	scoped := 0
	for _, pattern := range f.api.routes {
		method, path, found := strings.Cut(pattern, " ")
		if !found || len(scopesIn(pattern)) == 0 {
			continue
		}
		scoped++

		target, open := path, false
		for param, value := range ids {
			target = strings.ReplaceAll(target, "{"+param+"}", value)
		}
		for param, value := range placeholders {
			if strings.Contains(target, "{"+param+"}") {
				target = strings.ReplaceAll(target, "{"+param+"}", value)
				// A stand-in id may legitimately not exist, so this route's
				// owner cannot be expected to get anything but a 404.
				open = true
			}
		}
		if strings.Contains(target, "{") {
			t.Fatalf("%s names a path value this test does not know how to fill", pattern)
		}

		// The owner reaches it. Without this the refusal below could pass for
		// the wrong reason -- a mistyped id refuses everybody.
		if method == http.MethodGet && !open {
			f.api.authn = testAuthn(f.owner)
			if status := f.call(t, method, target, nil, nil); status == http.StatusNotFound {
				t.Errorf("%s: the owner was told their own resource does not exist", pattern)
			}
		}

		f.api.authn = auth.Single{Subject: "prov|intruder", Email: "intruder@example.com"}
		status := f.call(t, method, target, map[string]any{}, nil)
		if status != http.StatusNotFound {
			t.Errorf("%s: another user got %d, want 404", pattern, status)
		}
	}

	if scoped < 20 {
		t.Fatalf("only %d scoped routes were checked; the route table did not load", scoped)
	}
}

// TestAResourceIdInTheBodyIsCheckedToo covers what the route pattern cannot
// see: creating a task or a workspace names its repo in JSON.
func TestAResourceIdInTheBodyIsCheckedToo(t *testing.T) {
	f := newFixture(t)
	withProject(t, f)
	victimRepo := f.repo.ID

	f.api.authn = auth.Single{Subject: "prov|intruder", Email: "intruder@example.com"}

	for _, attempt := range []struct {
		name string
		path string
		body map[string]any
	}{
		{"a task", "/v1/tasks", map[string]any{
			"repo_id": victimRepo, "title": "mine now", "request": "do something",
		}},
		{"a workspace", "/v1/workspaces", map[string]any{
			"repo_id": victimRepo, "name": "mine now",
		}},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			if status := f.call(t, "POST", attempt.path, attempt.body, nil); status != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: another user started work on a repo that is not theirs", status)
			}
		})
	}
}

// TestRefusingSaysMissingRatherThanForbidden: a 403 confirms the id exists,
// which turns a guessed id into a way to learn what other people have.
func TestRefusingSaysMissingRatherThanForbidden(t *testing.T) {
	f := newFixture(t)
	withProject(t, f)

	f.api.authn = auth.Single{Subject: "prov|intruder", Email: "intruder@example.com"}

	var body map[string]any
	status := f.call(t, "GET", "/v1/repos/"+f.repo.ID, nil, &body)
	if status == http.StatusForbidden {
		t.Fatal("the API confirmed somebody else's repo exists")
	}
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}

	// An id that genuinely does not exist is answered the same way, so the
	// two cases cannot be told apart.
	missing := f.call(t, "GET", "/v1/repos/repo_doesnotexist", nil, nil)
	if missing != status {
		t.Fatalf("a nonexistent repo gave %d but somebody else's gave %d", missing, status)
	}
}

func TestScopesInReadsThePattern(t *testing.T) {
	t.Parallel()

	for pattern, want := range map[string]int{
		"GET /v1/repos":                       0,
		"GET /healthz":                        0,
		"GET /v1/repos/{repo}":                1,
		"PUT /v1/repos/{repo}/secrets/{name}": 1,
		"GET /v1/workspaces/{workspace}/services/{service}/logs": 1,
		"GET /v1/tasks/{task}/forks":                             1,
	} {
		if got := len(scopesIn(pattern)); got != want {
			t.Errorf("scopesIn(%q) found %d scopes, want %d", pattern, got, want)
		}
	}
}
