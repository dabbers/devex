package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
)

// newTestStore opens an isolated in-memory store for a single test.
func newTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, ctx
}

// seed creates the user/repo/project chain most tests need.
func seed(t *testing.T, s *Store, ctx context.Context) (*domain.User, *domain.Repo, *domain.Project) {
	t.Helper()
	u, err := s.EnsureUser(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	r := &domain.Repo{UserID: u.ID, Name: "devex", RemoteURL: "git@github.com:dabbers/devex.git"}
	if err := s.CreateRepo(ctx, r); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	p := &domain.Project{RepoID: r.ID, Name: "web", Path: "apps/web", Toolchain: "node", PreviewPort: 5173}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return u, r, p
}

func TestOpenIsIdempotentAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "dabberz.db")

	s1, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	u := &domain.User{Email: "owner@example.com"}
	if err := s1.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-opening must replay no migrations and must preserve the data.
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	got, err := s2.GetUserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail after reopen: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("user id = %q, want %q", got.ID, u.ID)
	}
}

func TestInMemoryStoresAreIsolated(t *testing.T) {
	a, ctx := newTestStore(t)
	b, _ := newTestStore(t)

	if err := a.CreateUser(ctx, &domain.User{Email: "a@example.com"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := b.GetUserByEmail(ctx, "a@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second in-memory store saw the first store's data (err = %v)", err)
	}
}

func TestEnsureUserIsIdempotent(t *testing.T) {
	s, ctx := newTestStore(t)
	first, err := s.EnsureUser(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	second, err := s.EnsureUser(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("EnsureUser (repeat): %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("EnsureUser created a second user: %q vs %q", first.ID, second.ID)
	}
}

func TestNotFoundAndConflictAreDistinguishable(t *testing.T) {
	s, ctx := newTestStore(t)
	if _, err := s.GetRepo(ctx, "repo_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRepo(missing) = %v, want ErrNotFound", err)
	}

	u, _, _ := seed(t, s, ctx)
	dup := &domain.Repo{UserID: u.ID, Name: "devex", RemoteURL: "x"}
	if err := s.CreateRepo(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate repo name = %v, want ErrConflict", err)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	s, ctx := newTestStore(t)
	orphan := &domain.Repo{UserID: "usr_missing", Name: "orphan", RemoteURL: "x"}
	if err := s.CreateRepo(ctx, orphan); err == nil {
		t.Fatal("expected a foreign key violation for a repo with no owner")
	}
}

func TestUpdateOnMissingRowReportsNotFound(t *testing.T) {
	s, ctx := newTestStore(t)
	ghost := &domain.Repo{ID: "repo_missing", Name: "x", DefaultBranch: "main"}
	if err := s.UpdateRepo(ctx, ghost); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateRepo(missing) = %v, want ErrNotFound", err)
	}
}

func TestUpsertProjectReplacesBySamePath(t *testing.T) {
	s, ctx := newTestStore(t)
	_, r, p := seed(t, s, ctx)

	// A later discovery pass re-reports the same path with better information.
	revised := &domain.Project{
		RepoID: r.ID, Name: "web-app", Path: "apps/web",
		Toolchain: "node", PreviewCommand: "npm run dev", PreviewPort: 3000, Confirmed: true,
	}
	if err := s.UpsertProject(ctx, revised); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if revised.ID != p.ID {
		t.Fatalf("upsert created a new project (%q) instead of updating %q", revised.ID, p.ID)
	}

	projects, err := s.ListProjects(ctx, r.ID)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("got %d projects, want 1", len(projects))
	}
	if projects[0].PreviewCommand != "npm run dev" || !projects[0].Confirmed {
		t.Fatalf("upsert did not apply the revision: %+v", projects[0])
	}
}

func TestTaskRoundTripsPlanAndPreferences(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, _ := seed(t, s, ctx)

	task := &domain.Task{
		UserID: u.ID, RepoID: r.ID, Title: "Ratings and photos",
		Request:     "add ratings and photo upload",
		MergeTarget: domain.MergeTargetIntegrationBranch, MergeTiming: domain.MergeTimingBatch,
		IntegrationBranch: "feat/batch-1",
		Plan: &domain.Plan{
			Summary: "two workstreams",
			Round:   1,
			Workstreams: []domain.PlannedWorkstream{
				{Name: "ratings", ProjectPath: "apps/web"},
				{Name: "photos", ProjectPath: "apps/web", SerializeGroup: "media", OverlapRationale: "both touch image handling"},
			},
			Questions: []domain.Question{{ID: "qst_1", Text: "Star or thumbs?"}},
		},
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != domain.TaskDraft {
		t.Fatalf("default state = %q, want %q", got.State, domain.TaskDraft)
	}
	if got.MergeTarget != domain.MergeTargetIntegrationBranch || got.MergeTiming != domain.MergeTimingBatch {
		t.Fatalf("merge preferences not preserved: %+v", got)
	}
	if got.Plan == nil || len(got.Plan.Workstreams) != 2 {
		t.Fatalf("plan not round-tripped: %+v", got.Plan)
	}
	if got.Plan.Workstreams[1].SerializeGroup != "media" {
		t.Fatalf("serialize group lost: %+v", got.Plan.Workstreams[1])
	}
	if !got.Plan.NeedsInput() {
		t.Fatal("plan with an unanswered question should need input")
	}
}

func TestCreateTaskDefaultsMergePreferences(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, _ := seed(t, s, ctx)

	task := &domain.Task{UserID: u.ID, RepoID: r.ID, Title: "t", Request: "r"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if !task.MergeTarget.Valid() || !task.MergeTiming.Valid() {
		t.Fatalf("defaults are not valid preferences: %+v", task)
	}
}

func TestForkLifecyclePersistsTimestampsAndUsage(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)

	task := &domain.Task{UserID: u.ID, RepoID: r.ID, Title: "t", Request: "r"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	fork := &domain.Fork{
		TaskID: task.ID, UserID: u.ID, RepoID: r.ID, ProjectID: p.ID,
		Name: "ratings", Branch: "dabberz/ratings",
	}
	if err := s.CreateFork(ctx, fork); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}
	if fork.State != domain.ForkQueued {
		t.Fatalf("new fork state = %q, want %q", fork.State, domain.ForkQueued)
	}

	if err := s.TransitionFork(ctx, fork, domain.ForkProvisioning, "capacity available"); err != nil {
		t.Fatalf("TransitionFork -> provisioning: %v", err)
	}
	if fork.StartedAt == nil {
		t.Fatal("StartedAt should be stamped on entering provisioning")
	}

	fork.Usage.Add(domain.Usage{Cycles: 2, InputTokens: 1000, OutputTokens: 250, CostUSD: 0.42, Wall: 90 * time.Second})
	if err := s.UpdateFork(ctx, fork); err != nil {
		t.Fatalf("UpdateFork: %v", err)
	}

	for _, next := range []domain.ForkState{domain.ForkCoding, domain.ForkVerifying, domain.ForkAwaitingMerge, domain.ForkMerging, domain.ForkMerged} {
		if err := s.TransitionFork(ctx, fork, next, ""); err != nil {
			t.Fatalf("TransitionFork -> %q: %v", next, err)
		}
	}
	if fork.EndedAt == nil {
		t.Fatal("EndedAt should be stamped on reaching a terminal state")
	}

	got, err := s.GetFork(ctx, fork.ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	if got.State != domain.ForkMerged {
		t.Fatalf("state = %q, want %q", got.State, domain.ForkMerged)
	}
	if got.Usage.Cycles != 2 || got.Usage.CostUSD != 0.42 || got.Usage.Wall != 90*time.Second {
		t.Fatalf("usage not round-tripped: %+v", got.Usage)
	}
	if got.StartedAt == nil || got.EndedAt == nil {
		t.Fatalf("timestamps not round-tripped: %+v", got)
	}
}

func TestTransitionForkRejectsIllegalMoveWithoutWriting(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	task := &domain.Task{UserID: u.ID, RepoID: r.ID, Title: "t", Request: "r"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	fork := &domain.Fork{TaskID: task.ID, UserID: u.ID, RepoID: r.ID, ProjectID: p.ID, Name: "f", Branch: "b"}
	if err := s.CreateFork(ctx, fork); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}

	if err := s.TransitionFork(ctx, fork, domain.ForkMerged, ""); err == nil {
		t.Fatal("expected queued -> merged to be rejected")
	}
	got, err := s.GetFork(ctx, fork.ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	if got.State != domain.ForkQueued {
		t.Fatalf("rejected transition was persisted: state = %q", got.State)
	}
}

func TestCountActiveForksIgnoresQueuedAndTerminal(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	task := &domain.Task{UserID: u.ID, RepoID: r.ID, Title: "t", Request: "r"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	states := []domain.ForkState{domain.ForkQueued, domain.ForkCoding, domain.ForkVerifying, domain.ForkEscalated}
	for i, want := range states {
		f := &domain.Fork{TaskID: task.ID, UserID: u.ID, RepoID: r.ID, ProjectID: p.ID, Name: "f", Branch: "b"}
		if err := s.CreateFork(ctx, f); err != nil {
			t.Fatalf("CreateFork %d: %v", i, err)
		}
		if want == domain.ForkQueued {
			continue
		}
		if err := s.TransitionFork(ctx, f, domain.ForkProvisioning, ""); err != nil {
			t.Fatalf("to provisioning: %v", err)
		}
		if err := s.TransitionFork(ctx, f, domain.ForkCoding, ""); err != nil {
			t.Fatalf("to coding: %v", err)
		}
		if want != domain.ForkCoding {
			if err := s.TransitionFork(ctx, f, want, ""); err != nil {
				t.Fatalf("to %q: %v", want, err)
			}
		}
	}

	n, err := s.CountActiveForks(ctx)
	if err != nil {
		t.Fatalf("CountActiveForks: %v", err)
	}
	// coding + verifying are active; queued and escalated are not.
	if n != 2 {
		t.Fatalf("CountActiveForks = %d, want 2", n)
	}
}

func TestListForksFiltersByTaskAndState(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	task := &domain.Task{UserID: u.ID, RepoID: r.ID, Title: "t", Request: "r"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	for range 3 {
		f := &domain.Fork{TaskID: task.ID, UserID: u.ID, RepoID: r.ID, ProjectID: p.ID, Name: "f", Branch: "b"}
		if err := s.CreateFork(ctx, f); err != nil {
			t.Fatalf("CreateFork: %v", err)
		}
	}

	queued, err := s.ListForks(ctx, ForkFilter{TaskID: task.ID, States: []domain.ForkState{domain.ForkQueued}})
	if err != nil {
		t.Fatalf("ListForks: %v", err)
	}
	if len(queued) != 3 {
		t.Fatalf("got %d queued forks, want 3", len(queued))
	}
	// Queue ordering must be oldest-first so admission is fair, and stable so
	// that repeated polls of the queue do not reshuffle it.
	for i := 1; i < len(queued); i++ {
		if queued[i].CreatedAt.Before(queued[i-1].CreatedAt) {
			t.Fatal("fork listing is not in chronological order")
		}
	}
	again, err := s.ListForks(ctx, ForkFilter{TaskID: task.ID, States: []domain.ForkState{domain.ForkQueued}})
	if err != nil {
		t.Fatalf("ListForks (repeat): %v", err)
	}
	for i := range queued {
		if queued[i].ID != again[i].ID {
			t.Fatalf("queue ordering is not stable across calls at position %d", i)
		}
	}

	active, err := s.ListForks(ctx, ForkFilter{TaskID: task.ID, Active: true})
	if err != nil {
		t.Fatalf("ListForks(active): %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("got %d active forks, want 0", len(active))
	}
}

func TestEventsStreamByAfterID(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, _ := seed(t, s, ctx)
	task := &domain.Task{UserID: u.ID, RepoID: r.ID, Title: "t", Request: "r"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var seqs []int64
	for i := range 5 {
		e := &domain.Event{
			UserID: u.ID, TaskID: task.ID, Type: domain.EventAgentMessage,
			Message: "step", Data: map[string]any{"n": float64(i)},
		}
		if err := s.AppendEvent(ctx, e); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		if e.Seq == 0 {
			t.Fatal("AppendEvent did not assign a sequence number")
		}
		seqs = append(seqs, e.Seq)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("event sequence is not monotonic: %v", seqs)
		}
	}

	rest, err := s.ListEvents(ctx, EventFilter{TaskID: task.ID, AfterSeq: seqs[2]})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(rest) != 2 {
		t.Fatalf("got %d events after cursor, want 2", len(rest))
	}
	if rest[0].Seq != seqs[3] {
		t.Fatalf("first event after cursor = %d, want %d", rest[0].Seq, seqs[3])
	}
	if rest[0].Data["n"] != float64(3) {
		t.Fatalf("event data not round-tripped: %+v", rest[0].Data)
	}
}

func TestPreviewRoutesUpsertAndReportUsedPorts(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	task := &domain.Task{UserID: u.ID, RepoID: r.ID, Title: "t", Request: "r"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	fork := &domain.Fork{TaskID: task.ID, UserID: u.ID, RepoID: r.ID, ProjectID: p.ID, Name: "f", Branch: "b"}
	if err := s.CreateFork(ctx, fork); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}

	route := &PreviewRoute{ForkID: fork.ID, Hostname: "preview-web-ratings.dab.im", UpstreamHost: "10.0.0.5", UpstreamPort: 41000}
	if err := s.PutPreviewRoute(ctx, route); err != nil {
		t.Fatalf("PutPreviewRoute: %v", err)
	}
	// Re-publishing the same fork updates in place rather than conflicting.
	route.UpstreamPort = 41001
	if err := s.PutPreviewRoute(ctx, route); err != nil {
		t.Fatalf("PutPreviewRoute (update): %v", err)
	}

	used, err := s.UsedPreviewPorts(ctx)
	if err != nil {
		t.Fatalf("UsedPreviewPorts: %v", err)
	}
	if used[41000] || !used[41001] {
		t.Fatalf("used ports = %v, want only 41001", used)
	}

	routes, err := s.ListPreviewRoutes(ctx)
	if err != nil {
		t.Fatalf("ListPreviewRoutes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
}

func TestSecretsAreUpsertedPerRepoAndName(t *testing.T) {
	s, ctx := newTestStore(t)
	_, r, _ := seed(t, s, ctx)

	if err := s.PutSecret(ctx, &SecretRow{RepoID: r.ID, Name: "DATABASE_URL", Nonce: []byte("n1"), Ciphertext: []byte("c1")}); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if err := s.PutSecret(ctx, &SecretRow{RepoID: r.ID, Name: "DATABASE_URL", Nonce: []byte("n2"), Ciphertext: []byte("c2")}); err != nil {
		t.Fatalf("PutSecret (rotate): %v", err)
	}

	got, err := s.GetSecret(ctx, r.ID, "DATABASE_URL")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(got.Ciphertext) != "c2" {
		t.Fatalf("ciphertext = %q, want the rotated value", got.Ciphertext)
	}

	all, err := s.ListSecrets(ctx, r.ID)
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d secrets, want 1", len(all))
	}

	if err := s.DeleteSecret(ctx, r.ID, "DATABASE_URL"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if err := s.DeleteSecret(ctx, r.ID, "DATABASE_URL"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestCascadeDeleteRemovesDependents(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	task := &domain.Task{UserID: u.ID, RepoID: r.ID, Title: "t", Request: "r"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	fork := &domain.Fork{TaskID: task.ID, UserID: u.ID, RepoID: r.ID, ProjectID: p.ID, Name: "f", Branch: "b"}
	if err := s.CreateFork(ctx, fork); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}

	if _, err := s.DB().ExecContext(ctx, "DELETE FROM repos WHERE id = ?", r.ID); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	if _, err := s.GetFork(ctx, fork.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fork survived repo deletion (err = %v)", err)
	}
	if _, err := s.GetTask(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("task survived repo deletion (err = %v)", err)
	}
}
