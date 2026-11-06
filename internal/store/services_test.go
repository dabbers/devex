package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
)

func TestServiceRoundTripsEveryField(t *testing.T) {
	s, ctx := newTestStore(t)
	_, _, p := seed(t, s, ctx)

	db := &domain.Service{
		ProjectID: p.ID, Name: "postgres", Kind: domain.ServiceDatastore,
		Command: "postgres -D /var/lib/pg", Port: 5432,
	}
	if err := s.CreateService(ctx, db); err != nil {
		t.Fatalf("CreateService(postgres): %v", err)
	}
	web := &domain.Service{
		ProjectID: p.ID, Name: "web", Kind: domain.ServiceWeb,
		Command: "npm run dev", Port: 5173, HealthPath: "/healthz",
		Exposed: true, Primary: true, DependsOn: []string{"postgres"},
	}
	if err := s.CreateService(ctx, web); err != nil {
		t.Fatalf("CreateService(web): %v", err)
	}

	got, err := s.GetService(ctx, web.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.Name != "web" || got.Kind != domain.ServiceWeb || got.Port != 5173 ||
		got.HealthPath != "/healthz" || !got.Exposed || !got.Primary {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if len(got.DependsOn) != 1 || got.DependsOn[0] != "postgres" {
		t.Fatalf("dependencies lost: %+v", got.DependsOn)
	}
}

func TestListServicesReturnsDependencyOrder(t *testing.T) {
	s, ctx := newTestStore(t)
	_, _, p := seed(t, s, ctx)

	// Inserted front-to-back, so insertion order is the wrong order. A caller
	// starting them as listed must still never start one before what it needs.
	for _, sv := range []*domain.Service{
		{ProjectID: p.ID, Name: "web", Kind: domain.ServiceWeb, Port: 5173,
			Exposed: true, Primary: true, DependsOn: []string{"api"}},
		{ProjectID: p.ID, Name: "api", Kind: domain.ServiceAPI, Port: 8080,
			Exposed: true, DependsOn: []string{"db"}},
		{ProjectID: p.ID, Name: "db", Kind: domain.ServiceDatastore, Port: 5432},
	} {
		if err := s.CreateService(ctx, sv); err != nil {
			t.Fatalf("CreateService(%s): %v", sv.Name, err)
		}
	}

	services, err := s.ListServices(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	var order []string
	for _, sv := range services {
		order = append(order, sv.Name)
	}
	if strings.Join(order, ",") != "db,api,web" {
		t.Fatalf("start order = %v, want db before api before web", order)
	}
}

func TestOnlyOneServiceMayBeTheFrontDoor(t *testing.T) {
	s, ctx := newTestStore(t)
	_, _, p := seed(t, s, ctx)

	first := &domain.Service{ProjectID: p.ID, Name: "web", Kind: domain.ServiceWeb,
		Port: 3000, Exposed: true, Primary: true}
	if err := s.CreateService(ctx, first); err != nil {
		t.Fatalf("CreateService(web): %v", err)
	}
	second := &domain.Service{ProjectID: p.ID, Name: "admin", Kind: domain.ServiceWeb,
		Port: 3001, Exposed: true, Primary: true}
	if err := s.CreateService(ctx, second); err == nil {
		t.Fatal("a second front door was accepted; the schema should refuse it")
	}
}

func TestServiceValidationRefusesImpossibleCombinations(t *testing.T) {
	cases := []struct {
		name    string
		service domain.Service
		wantErr string
	}{
		{"exposed with no port",
			domain.Service{Name: "web", Kind: domain.ServiceWeb, Exposed: true},
			"listens on no port"},
		{"exposed datastore",
			domain.Service{Name: "db", Kind: domain.ServiceDatastore, Port: 5432, Exposed: true},
			"never exposed publicly"},
		{"exposed worker",
			domain.Service{Name: "jobs", Kind: domain.ServiceWorker, Port: 9000, Exposed: true},
			"never exposed publicly"},
		{"primary but not exposed",
			domain.Service{Name: "web", Kind: domain.ServiceWeb, Port: 3000, Primary: true},
			"not exposed"},
		{"primary api",
			domain.Service{Name: "api", Kind: domain.ServiceAPI, Port: 8080, Exposed: true, Primary: true},
			"must be a web service"},
		{"unknown kind",
			domain.Service{Name: "thing", Kind: domain.ServiceKind("daemon"), Port: 1},
			"unknown kind"},
		{"self dependency",
			domain.Service{Name: "web", Kind: domain.ServiceWeb, Port: 3000, DependsOn: []string{"web"}},
			"depends on itself"},
		{"no name",
			domain.Service{Kind: domain.ServiceWeb, Port: 3000},
			"needs a name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.service.Validate()
			if err == nil {
				t.Fatalf("accepted an impossible service: %+v", tc.service)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not explain the problem (want %q)", err, tc.wantErr)
			}
		})
	}
}

func TestReplaceServicesIsAtomicAndKeepsIdentities(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)

	original := []*domain.Service{
		{ProjectID: p.ID, Name: "web", Kind: domain.ServiceWeb, Port: 3000, Exposed: true, Primary: true},
		{ProjectID: p.ID, Name: "doomed", Kind: domain.ServiceWorker, Command: "old"},
	}
	if err := s.ReplaceServices(ctx, p.ID, original); err != nil {
		t.Fatalf("ReplaceServices: %v", err)
	}
	webID := original[0].ID

	// A route attached to the surviving service must not be cascaded away by
	// the replacement, because the URL may be bookmarked.
	task := &domain.Task{UserID: u.ID, RepoID: r.ID, Title: "t", Request: "r"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	fork := &domain.Fork{TaskID: task.ID, UserID: u.ID, RepoID: r.ID, ProjectID: p.ID, Name: "f", Branch: "b"}
	if err := s.CreateFork(ctx, fork); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}
	if err := s.PutPreviewRoute(ctx, &PreviewRoute{
		ForkID: fork.ID, ServiceID: webID,
		Hostname: "web-web-f.dab.im", UpstreamHost: "10.0.0.2", UpstreamPort: 41000,
	}); err != nil {
		t.Fatalf("PutPreviewRoute: %v", err)
	}

	// A rerun of discovery: "doomed" is gone, the front door moves to a new
	// service, and "web" is demoted. Applied row by row this would transiently
	// have two primaries and be refused.
	revised := []*domain.Service{
		{ProjectID: p.ID, Name: "web", Kind: domain.ServiceWeb, Port: 3000, Exposed: true},
		{ProjectID: p.ID, Name: "shell", Kind: domain.ServiceWeb, Port: 4000, Exposed: true, Primary: true},
	}
	if err := s.ReplaceServices(ctx, p.ID, revised); err != nil {
		t.Fatalf("ReplaceServices (revised): %v", err)
	}

	if revised[0].ID != webID {
		t.Fatalf("web was recreated (%s) instead of updated (%s); its preview route would be lost",
			revised[0].ID, webID)
	}
	services, err := s.ListServices(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("got %d services, want 2: %+v", len(services), services)
	}
	primary := domain.PrimaryService(services)
	if primary == nil || primary.Name != "shell" {
		t.Fatalf("front door did not move to shell: %+v", primary)
	}
	if _, err := s.GetPreviewRoute(ctx, fork.ID, webID); err != nil {
		t.Fatalf("the surviving service lost its preview route: %v", err)
	}
}

func TestReplaceServicesRefusesTwoFrontDoors(t *testing.T) {
	s, ctx := newTestStore(t)
	_, _, p := seed(t, s, ctx)

	err := s.ReplaceServices(ctx, p.ID, []*domain.Service{
		{ProjectID: p.ID, Name: "web", Kind: domain.ServiceWeb, Port: 3000, Exposed: true, Primary: true},
		{ProjectID: p.ID, Name: "admin", Kind: domain.ServiceWeb, Port: 3001, Exposed: true, Primary: true},
	})
	if err == nil {
		t.Fatal("two front doors were accepted")
	}
	if !strings.Contains(err.Error(), "at most one service may be primary") {
		t.Fatalf("error %q does not name the problem", err)
	}
}

func TestReplaceServicesRefusesACycle(t *testing.T) {
	s, ctx := newTestStore(t)
	_, _, p := seed(t, s, ctx)

	err := s.ReplaceServices(ctx, p.ID, []*domain.Service{
		{ProjectID: p.ID, Name: "a", Kind: domain.ServiceAPI, Port: 1, Exposed: true, DependsOn: []string{"b"}},
		{ProjectID: p.ID, Name: "b", Kind: domain.ServiceAPI, Port: 2, Exposed: true, DependsOn: []string{"a"}},
	})
	if err == nil {
		t.Fatal("a dependency cycle was accepted; nothing could ever start")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error %q does not name the cycle", err)
	}
}

func TestDeletingAServiceWithdrawsItsPreviewRoutes(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	sv := seedService(t, s, ctx, p.ID)

	task := &domain.Task{UserID: u.ID, RepoID: r.ID, Title: "t", Request: "r"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	fork := &domain.Fork{TaskID: task.ID, UserID: u.ID, RepoID: r.ID, ProjectID: p.ID, Name: "f", Branch: "b"}
	if err := s.CreateFork(ctx, fork); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}
	if err := s.PutPreviewRoute(ctx, &PreviewRoute{
		ForkID: fork.ID, ServiceID: sv.ID,
		Hostname: "web-web-f.dab.im", UpstreamHost: "10.0.0.2", UpstreamPort: 41000,
	}); err != nil {
		t.Fatalf("PutPreviewRoute: %v", err)
	}

	if err := s.DeleteService(ctx, sv.ID); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	// A hostname must never outlive the thing it pointed at: the proxy would
	// otherwise publish a route nothing can explain.
	if _, err := s.GetPreviewRoute(ctx, fork.ID, sv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the route survived its service: %v", err)
	}
}

func TestForkRoutesAreListedPerService(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)

	web := &domain.Service{ProjectID: p.ID, Name: "web", Kind: domain.ServiceWeb,
		Port: 3000, Exposed: true, Primary: true}
	api := &domain.Service{ProjectID: p.ID, Name: "api", Kind: domain.ServiceAPI,
		Port: 8080, Exposed: true}
	for _, sv := range []*domain.Service{web, api} {
		if err := s.CreateService(ctx, sv); err != nil {
			t.Fatalf("CreateService(%s): %v", sv.Name, err)
		}
	}

	task := &domain.Task{UserID: u.ID, RepoID: r.ID, Title: "t", Request: "r"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	fork := &domain.Fork{TaskID: task.ID, UserID: u.ID, RepoID: r.ID, ProjectID: p.ID, Name: "f", Branch: "b"}
	if err := s.CreateFork(ctx, fork); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}

	for i, sv := range []*domain.Service{web, api} {
		if err := s.PutPreviewRoute(ctx, &PreviewRoute{
			ForkID: fork.ID, ServiceID: sv.ID,
			Hostname:     fmt.Sprintf("%s-web-f.dab.im", sv.Name),
			UpstreamHost: "10.0.0.2", UpstreamPort: 41000 + i,
		}); err != nil {
			t.Fatalf("PutPreviewRoute(%s): %v", sv.Name, err)
		}
	}

	routes, err := s.ListForkPreviewRoutes(ctx, fork.ID)
	if err != nil {
		t.Fatalf("ListForkPreviewRoutes: %v", err)
	}
	// One fork, two addresses. This is the whole point of the service model:
	// the old schema keyed routes by fork alone and could hold only one.
	if len(routes) != 2 {
		t.Fatalf("got %d routes for one fork, want 2: %+v", len(routes), routes)
	}
}

// TestBackfillMigratesLegacyPreviewColumns builds a database at the schema that
// existed before services, puts real rows in it, and then opens it normally so
// the outstanding migrations run.
//
// A data migration without a test is a liability: it runs once, on a database
// that matters, and a mistake is not recoverable from the code.
func TestBackfillMigratesLegacyPreviewColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}

	// Apply only the migrations that predate services, and record them, so a
	// normal Open afterwards runs exactly the ones under test.
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE schema_migrations (
		name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create migration table: %v", err)
	}
	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	applied := 0
	for _, entry := range entries {
		name := filepath.Base(entry)
		if name >= "0003" {
			continue
		}
		body, err := migrationFS.ReadFile(entry)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := legacy.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := legacy.ExecContext(ctx,
			"INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)",
			name, formatTime(time.Now())); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
		applied++
	}
	if applied == 0 {
		t.Fatal("no pre-service migrations were found; this test is not exercising the backfill")
	}

	now := formatTime(time.Now())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := legacy.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed legacy row: %v\nquery: %s", err, query)
		}
	}
	exec(`INSERT INTO users (id, email, created_at) VALUES ('usr_1', 'o@example.com', ?)`, now)
	exec(`INSERT INTO repos (id, user_id, name, remote_url, default_branch, discovered, created_at, updated_at)
	      VALUES ('repo_1', 'usr_1', 'devex', 'git@example.com:devex.git', 'main', 1, ?, ?)`, now, now)
	// One project with a runnable preview, one with nothing to run.
	exec(`INSERT INTO projects (id, repo_id, name, path, toolchain, preview_command, preview_port, confirmed, created_at, updated_at)
	      VALUES ('proj_web', 'repo_1', 'web', 'apps/web', 'node', 'npm run dev', 5173, 1, ?, ?)`, now, now)
	exec(`INSERT INTO projects (id, repo_id, name, path, toolchain, preview_command, preview_port, confirmed, created_at, updated_at)
	      VALUES ('proj_lib', 'repo_1', 'lib', 'packages/lib', 'node', '', 0, 1, ?, ?)`, now, now)
	exec(`INSERT INTO tasks (id, user_id, repo_id, title, request, state, merge_target, merge_timing, created_at, updated_at)
	      VALUES ('task_1', 'usr_1', 'repo_1', 't', 'r', 'running', 'default_branch', 'immediate', ?, ?)`, now, now)
	exec(`INSERT INTO forks (id, task_id, user_id, repo_id, project_id, name, description, branch, state, usage_json, created_at, updated_at)
	      VALUES ('fork_1', 'task_1', 'usr_1', 'repo_1', 'proj_web', 'ratings', '', 'b', 'coding', '{}', ?, ?)`, now, now)
	exec(`INSERT INTO preview_routes (fork_id, hostname, upstream_host, upstream_port, created_at)
	      VALUES ('fork_1', 'preview-web-ratings.dab.im', '172.30.0.2', 41000, ?)`, now)
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (running the backfill): %v", err)
	}
	defer s.Close()

	// The old dev server became the project's exposed web front door.
	services, err := s.ListServices(ctx, "proj_web")
	if err != nil {
		t.Fatalf("ListServices(proj_web): %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("got %d services for the web project, want 1: %+v", len(services), services)
	}
	sv := services[0]
	if sv.Command != "npm run dev" || sv.Port != 5173 {
		t.Fatalf("the preview command or port was lost: %+v", sv)
	}
	if sv.Kind != domain.ServiceWeb || !sv.Exposed || !sv.Primary {
		t.Fatalf("the migrated service should be the exposed web front door: %+v", sv)
	}
	if !sv.Confirmed {
		t.Fatalf("a confirmed project's service should stay confirmed: %+v", sv)
	}
	if !strings.HasPrefix(sv.ID, "svc_") {
		t.Fatalf("service id %q is not a real dabberz identifier", sv.ID)
	}

	// A project that had nothing to run gets no invented service.
	libServices, err := s.ListServices(ctx, "proj_lib")
	if err != nil {
		t.Fatalf("ListServices(proj_lib): %v", err)
	}
	if len(libServices) != 0 {
		t.Fatalf("a project with no preview command was given %d services: %+v",
			len(libServices), libServices)
	}

	// The existing route kept its hostname and port, and now belongs to the
	// service rather than to the fork as a whole.
	routes, err := s.ListForkPreviewRoutes(ctx, "fork_1")
	if err != nil {
		t.Fatalf("ListForkPreviewRoutes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want the one carried over: %+v", len(routes), routes)
	}
	if routes[0].Hostname != "preview-web-ratings.dab.im" || routes[0].UpstreamPort != 41000 {
		t.Fatalf("the bookmarked preview URL moved: %+v", routes[0])
	}
	if routes[0].ServiceID != sv.ID {
		t.Fatalf("route was re-keyed to %q, want the backfilled service %q",
			routes[0].ServiceID, sv.ID)
	}

	// The legacy columns are gone, so there is no second source of truth for
	// how a project runs.
	for _, column := range []string{"preview_command", "preview_port"} {
		var count int
		if err := s.db.QueryRowContext(ctx,
			"SELECT count(*) FROM pragma_table_info('projects') WHERE name = ?", column,
		).Scan(&count); err != nil {
			t.Fatalf("inspect projects schema: %v", err)
		}
		if count != 0 {
			t.Fatalf("projects.%s survived the migration", column)
		}
	}
}
