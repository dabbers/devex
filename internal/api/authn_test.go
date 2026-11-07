package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/auth"
)

// rejecting is an Authenticator that refuses everything, standing in for a
// browser that has not signed in.
type rejecting struct{ err error }

func (rejecting) Name() string { return "rejecting" }
func (r rejecting) Identify(context.Context, *http.Request) (*auth.Identity, error) {
	return nil, r.err
}
func (rejecting) PublicConfig() map[string]any { return map[string]any{"provider": "rejecting"} }

// TestEveryDataRouteRequiresAUser walks the real route table and checks that
// nothing under /v1 answers an unidentified request.
//
// It enumerates routes rather than naming a few, because the risk is not the
// routes that exist today: it is the one somebody adds next to a protected
// neighbour and assumes is covered.
func TestEveryDataRouteRequiresAUser(t *testing.T) {
	f := newFixture(t)
	f.api.authn = rejecting{err: auth.ErrNoCredentials}
	handler := f.api.Handler()

	// Method and path for every registered data route, including the ones that
	// hand out real capability: a shell on the machine and the secrets vault.
	routes := []struct{ method, path string }{
		{"GET", "/v1/capacity"},
		{"GET", "/v1/overview"},
		{"GET", "/v1/audit"},
		{"GET", "/v1/repos"},
		{"POST", "/v1/repos"},
		{"GET", "/v1/repos/repo_1"},
		{"GET", "/v1/repos/repo_1/projects"},
		{"POST", "/v1/repos/repo_1/discover"},
		{"POST", "/v1/repos/repo_1/projects/confirm"},
		{"GET", "/v1/repos/repo_1/secrets"},
		{"PUT", "/v1/repos/repo_1/secrets/TOKEN"},
		{"DELETE", "/v1/repos/repo_1/secrets/TOKEN"},
		{"GET", "/v1/repos/repo_1/memory"},
		{"PUT", "/v1/repos/repo_1/memory/slug"},
		{"GET", "/v1/tasks"},
		{"POST", "/v1/tasks"},
		{"GET", "/v1/tasks/task_1"},
		{"POST", "/v1/tasks/task_1/plan"},
		{"POST", "/v1/tasks/task_1/answers"},
		{"POST", "/v1/tasks/task_1/approve"},
		{"POST", "/v1/tasks/task_1/cancel"},
		{"GET", "/v1/tasks/task_1/forks"},
		{"GET", "/v1/forks/fork_1"},
		{"POST", "/v1/forks/fork_1/resolve"},
		{"POST", "/v1/forks/fork_1/validate"},
		{"GET", "/v1/forks/fork_1/shell"},
		{"GET", "/v1/events"},
		{"GET", "/v1/events/stream"},
		{"GET", "/v1/auth/whoami"},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("answered an unidentified request with %d, want 401", rec.Code)
			}
		})
	}
}

// TestOpenPathsStayOpen: a health check cannot hold a credential, and the
// sign-in configuration is read before anyone is signed in.
func TestOpenPathsStayOpen(t *testing.T) {
	f := newFixture(t)
	f.api.authn = rejecting{err: auth.ErrNoCredentials}
	handler := f.api.Handler()

	for _, path := range []string{"/healthz", "/v1/auth/config"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d, want 200", path, rec.Code)
		}
	}
}

// TestAuthConfigLeaksNothingSecret guards the one endpoint that is open by
// design and comes from the authenticator.
func TestAuthConfigLeaksNothingSecret(t *testing.T) {
	clerk, err := auth.NewClerk(auth.ClerkConfig{
		PublishableKey: "pk_test_ZXhhbXBsZS5jbGVyay5hY2NvdW50cy5kZXYk",
		SecretKey:      "sk_test_this_must_never_be_served",
	})
	if err != nil {
		t.Fatalf("NewClerk: %v", err)
	}
	f := newFixture(t)
	f.api.authn = clerk

	rec := httptest.NewRecorder()
	f.api.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/auth/config", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "pk_test_") {
		t.Fatalf("the browser was not told which instance to sign in to: %s", body)
	}
	if strings.Contains(body, "sk_test") || strings.Contains(body, "must_never_be_served") {
		t.Fatalf("the secret key was served to the browser: %s", body)
	}
}

// TestWorkIsAttributedToTheRequestingUser: with several users, an action must
// land on the one who took it. Scoping everything to a fixed owner was fine
// with one user and is a data leak with two.
func TestWorkIsAttributedToTheRequestingUser(t *testing.T) {
	f := newFixture(t)

	// Two people, each creating a repo through the same server.
	for _, person := range []struct{ subject, email, repo string }{
		{"prov|alice", "alice@example.com", "alice-repo"},
		{"prov|bob", "bob@example.com", "bob-repo"},
	} {
		f.api.authn = auth.Single{Subject: person.subject, Email: person.email}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/repos",
			strings.NewReader(`{"name":"`+person.repo+`","remote_url":"git@example.com:x.git"}`))
		req.Header.Set("Content-Type", "application/json")
		f.api.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("%s could not create a repo: %d %s", person.email, rec.Code, rec.Body.String())
		}
	}

	// Alice must not see Bob's repository.
	f.api.authn = auth.Single{Subject: "prov|alice", Email: "alice@example.com"}
	rec := httptest.NewRecorder()
	f.api.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/repos", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "alice-repo") {
		t.Fatalf("alice cannot see her own repository: %s", body)
	}
	if strings.Contains(body, "bob-repo") {
		t.Fatalf("alice can see bob's repository: %s", body)
	}
}

// TestARejectedCredentialIsDistinguishedFromNone keeps the sign-in flow
// sensible: a browser with no session should be prompted, not told it failed.
func TestARejectedCredentialIsDistinguishedFromNone(t *testing.T) {
	f := newFixture(t)

	f.api.authn = rejecting{err: auth.ErrNoCredentials}
	rec := httptest.NewRecorder()
	f.api.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/overview", nil))
	if !strings.Contains(rec.Body.String(), "sign in to continue") {
		t.Fatalf("a request with no credentials was not asked to sign in: %s", rec.Body.String())
	}

	f.api.authn = rejecting{err: auth.ErrUnauthenticated}
	rec = httptest.NewRecorder()
	f.api.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/overview", nil))
	if !strings.Contains(rec.Body.String(), "not valid") {
		t.Fatalf("a rejected credential was not reported as such: %s", rec.Body.String())
	}
}

func TestBearerTokenPrefersTheHeaderThenTheCookie(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer from-header")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "from-cookie"})
	if got := auth.BearerToken(req); got != "from-header" {
		t.Fatalf("token = %q, want the header to win", got)
	}

	// A browser cannot set headers on an EventSource or a WebSocket handshake,
	// and the activity feed is server-sent events while the shell is a
	// WebSocket. The cookie is what protects those.
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "from-cookie"})
	if got := auth.BearerToken(req); got != "from-cookie" {
		t.Fatalf("token = %q, want the cookie", got)
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if got := auth.BearerToken(req); got != "" {
		t.Fatalf("token = %q, want nothing for a non-bearer scheme", got)
	}
}
