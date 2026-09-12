package preview

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/proxy"
	"github.com/dabbers/devex/internal/store"
)

// recordingRouter captures the last published route set.
type recordingRouter struct {
	mu       sync.Mutex
	applied  [][]proxy.Route
	failWith error
}

func (r *recordingRouter) Name() string { return "recording" }

func (r *recordingRouter) Apply(_ context.Context, routes []proxy.Route) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failWith != nil {
		return r.failWith
	}
	r.applied = append(r.applied, append([]proxy.Route(nil), routes...))
	return nil
}

func (r *recordingRouter) last() []proxy.Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.applied) == 0 {
		return nil
	}
	return r.applied[len(r.applied)-1]
}

type fixture struct {
	store   *store.Store
	router  *recordingRouter
	alloc   *Allocator
	user    *domain.User
	repo    *domain.Repo
	project *domain.Project
	task    *domain.Task
	ctx     context.Context
}

func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	user, err := st.EnsureUser(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	repo := &domain.Repo{UserID: user.ID, Name: "devex", RemoteURL: "git@example.com:devex.git"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	project := &domain.Project{RepoID: repo.ID, Name: "Web App", Path: "apps/web", PreviewPort: 5173}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	task := &domain.Task{UserID: user.ID, RepoID: repo.ID, Title: "t", Request: "r", State: domain.TaskRunning}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	router := &recordingRouter{}
	alloc, err := New(st, router, cfg)
	if err != nil {
		t.Fatalf("preview.New: %v", err)
	}
	return &fixture{store: st, router: router, alloc: alloc, user: user, repo: repo, project: project, task: task, ctx: ctx}
}

func (f *fixture) fork(t *testing.T, name string) *domain.Fork {
	t.Helper()
	fork := &domain.Fork{
		TaskID: f.task.ID, UserID: f.user.ID, RepoID: f.repo.ID, ProjectID: f.project.ID,
		Name: name, Branch: "dabberz/" + name,
	}
	if err := f.store.CreateFork(f.ctx, fork); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}
	return fork
}

func TestConfigValidation(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Error("a config without a domain should be rejected")
	}
	if err := (Config{Domain: "dab.im", PortStart: 500, PortEnd: 400}).Validate(); err == nil {
		t.Error("an inverted port range should be rejected")
	}
	if err := (Config{Domain: "dab.im"}).Validate(); err != nil {
		t.Errorf("a domain alone should be enough: %v", err)
	}
}

func TestAllocateBuildsAPerForkHostname(t *testing.T) {
	f := newFixture(t, Config{Domain: "dab.im"})
	fork := f.fork(t, "Photo Upload")

	route, err := f.alloc.Allocate(f.ctx, fork, f.project, "172.30.0.2")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if route.Hostname != "preview-web-app-photo-upload.dab.im" {
		t.Fatalf("hostname = %q", route.Hostname)
	}
	if route.UpstreamPort < DefaultPortStart || route.UpstreamPort > DefaultPortEnd {
		t.Fatalf("port %d is outside the configured range", route.UpstreamPort)
	}
	if got := f.alloc.URL(route); got != "https://preview-web-app-photo-upload.dab.im" {
		t.Fatalf("URL = %q", got)
	}
}

func TestAllocateIsIdempotentSoBookmarkedURLsSurvive(t *testing.T) {
	f := newFixture(t, Config{Domain: "dab.im"})
	fork := f.fork(t, "ratings")

	first, err := f.alloc.Allocate(f.ctx, fork, f.project, "172.30.0.2")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	// A fork's VM can come back on a different address, but its public URL
	// must not move: the user may have it open.
	second, err := f.alloc.Allocate(f.ctx, fork, f.project, "172.30.0.99")
	if err != nil {
		t.Fatalf("Allocate (repeat): %v", err)
	}
	if second.Hostname != first.Hostname || second.UpstreamPort != first.UpstreamPort {
		t.Fatalf("preview moved on reallocation: %+v -> %+v", first, second)
	}
	if second.UpstreamHost != "172.30.0.99" {
		t.Fatalf("upstream not updated: %q", second.UpstreamHost)
	}
}

func TestAllocateGivesEveryForkADistinctHostnameAndPort(t *testing.T) {
	f := newFixture(t, Config{Domain: "dab.im"})

	hostnames := map[string]bool{}
	ports := map[int]bool{}
	for range 25 {
		// Same name every time: distinct workstreams in different tasks can
		// legitimately collide on a label.
		fork := f.fork(t, "ratings")
		route, err := f.alloc.Allocate(f.ctx, fork, f.project, "172.30.0.2")
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		if hostnames[route.Hostname] {
			t.Fatalf("hostname %q was handed out twice", route.Hostname)
		}
		if ports[route.UpstreamPort] {
			t.Fatalf("port %d was handed out twice", route.UpstreamPort)
		}
		hostnames[route.Hostname] = true
		ports[route.UpstreamPort] = true
	}
}

func TestAllocateRequiresAnUpstreamAddress(t *testing.T) {
	f := newFixture(t, Config{Domain: "dab.im"})
	fork := f.fork(t, "ratings")
	if _, err := f.alloc.Allocate(f.ctx, fork, f.project, ""); err == nil {
		t.Fatal("expected allocation to fail before the VM has an address")
	}
}

func TestAllocateReportsPortExhaustion(t *testing.T) {
	// A range with exactly one port.
	f := newFixture(t, Config{Domain: "dab.im", PortStart: 41000, PortEnd: 41000})

	if _, err := f.alloc.Allocate(f.ctx, f.fork(t, "first"), f.project, "172.30.0.2"); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	_, err := f.alloc.Allocate(f.ctx, f.fork(t, "second"), f.project, "172.30.0.6")
	if err == nil {
		t.Fatal("expected port exhaustion to be reported")
	}
	// Nothing is reclaimed automatically, so the message must tell the
	// operator what to do.
	if !strings.Contains(err.Error(), "clean up") {
		t.Fatalf("exhaustion error should point at manual cleanup: %v", err)
	}
}

func TestPublishSendsEveryRecordedRoute(t *testing.T) {
	f := newFixture(t, Config{Domain: "dab.im"})
	for _, name := range []string{"ratings", "photos"} {
		if _, err := f.alloc.Allocate(f.ctx, f.fork(t, name), f.project, "172.30.0.2"); err != nil {
			t.Fatalf("Allocate(%s): %v", name, err)
		}
	}

	if err := f.alloc.Publish(f.ctx); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// The whole set is republished, so the proxy converges on the database
	// rather than on an incremental diff.
	if got := len(f.router.last()); got != 2 {
		t.Fatalf("published %d routes, want 2", got)
	}
}

func TestPublishSurfacesRouterFailures(t *testing.T) {
	f := newFixture(t, Config{Domain: "dab.im"})
	f.router.failWith = context.DeadlineExceeded
	err := f.alloc.Publish(f.ctx)
	if err == nil {
		t.Fatal("expected a router failure to propagate")
	}
	if !strings.Contains(err.Error(), "recording") {
		t.Fatalf("error should name the router: %v", err)
	}
}

func TestWithdrawRemovesARouteAndRepublishes(t *testing.T) {
	f := newFixture(t, Config{Domain: "dab.im"})
	keep := f.fork(t, "ratings")
	drop := f.fork(t, "photos")
	for _, fork := range []*domain.Fork{keep, drop} {
		if _, err := f.alloc.Allocate(f.ctx, fork, f.project, "172.30.0.2"); err != nil {
			t.Fatalf("Allocate: %v", err)
		}
	}

	if err := f.alloc.Withdraw(f.ctx, drop.ID); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	published := f.router.last()
	if len(published) != 1 {
		t.Fatalf("published %d routes after withdrawal, want 1", len(published))
	}
	if !strings.Contains(published[0].Hostname, "ratings") {
		t.Fatalf("the wrong route survived: %q", published[0].Hostname)
	}
}

func TestNewDefaultsToADiscardRouter(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// A machine with no proxy in front of it must still start.
	alloc, err := New(st, nil, Config{Domain: "dab.im"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := alloc.Publish(ctx); err != nil {
		t.Fatalf("Publish with no router: %v", err)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Photo Upload":      "photo-upload",
		"apps/web":          "apps-web",
		"  leading spaces":  "leading-spaces",
		"UPPER_snake.case":  "upper-snake-case",
		"---":               "",
		"add ratings (v2)!": "add-ratings-v2",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostnameLabelStaysWithinTheDNSLimit(t *testing.T) {
	f := newFixture(t, Config{Domain: "dab.im"})
	long := strings.Repeat("extremely-long-workstream-name-", 6)
	fork := f.fork(t, long)

	route, err := f.alloc.Allocate(f.ctx, fork, f.project, "172.30.0.2")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	label, _, _ := strings.Cut(route.Hostname, ".")
	if len(label) > 63 {
		t.Fatalf("hostname label is %d characters, over the DNS limit: %q", len(label), label)
	}
	if strings.HasSuffix(label, "-") {
		t.Fatalf("truncation left a trailing hyphen: %q", label)
	}
}

func TestHostnameFallsBackWhenTheProjectIsUnknown(t *testing.T) {
	f := newFixture(t, Config{Domain: "dab.im"})
	fork := f.fork(t, "ratings")
	route, err := f.alloc.Allocate(f.ctx, fork, nil, "172.30.0.2")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if route.Hostname != "preview-app-ratings.dab.im" {
		t.Fatalf("hostname = %q, want the generic project label", route.Hostname)
	}
}
