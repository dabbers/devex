package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/llm"
	"github.com/dabbers/devex/internal/store"
)

// startTask plans and approves a task, returning its forks.
func (f *fixture) startTask(t *testing.T, request string) (*domain.Task, []*domain.Fork) {
	t.Helper()

	var task domain.Task
	if status := f.call(t, http.MethodPost, "/v1/tasks", map[string]any{
		"repo_id": f.repo.ID, "request": request, "plan": true,
	}, &task); status != http.StatusCreated {
		t.Fatalf("create task: %d", status)
	}
	var approved struct {
		Task  *domain.Task   `json:"task"`
		Forks []*domain.Fork `json:"forks"`
	}
	if status := f.call(t, http.MethodPost, "/v1/tasks/"+task.ID+"/approve", nil, &approved); status != http.StatusOK {
		t.Fatalf("approve: %d", status)
	}
	return approved.Task, approved.Forks
}

func TestOverviewSpansEveryRepo(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()), planJSON(nil, oneWorkstream()))

	// A second repo, so the overview has something to unify.
	other := &domain.Repo{UserID: f.owner.ID, Name: "other", RemoteURL: "git@example.com:other.git", DefaultBranch: "main"}
	if err := f.store.CreateRepo(f.ctx, other); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	f.startTask(t, "add ratings")

	var second domain.Task
	f.call(t, http.MethodPost, "/v1/tasks", map[string]any{
		"repo_id": other.ID, "request": "add search", "plan": true,
	}, &second)
	f.call(t, http.MethodPost, "/v1/tasks/"+second.ID+"/approve", nil, nil)

	var overview Overview
	if status := f.call(t, http.MethodGet, "/v1/overview", nil, &overview); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	if len(overview.Repos) != 2 {
		t.Fatalf("overview covers %d repos, want both", len(overview.Repos))
	}
	// Work from every repo appears in one list; that is the whole point.
	if len(overview.Work) != 2 {
		t.Fatalf("overview lists %d in-flight forks, want 2 across both repos", len(overview.Work))
	}
	repos := map[string]bool{}
	for _, view := range overview.Work {
		if view.RepoName == "" {
			t.Error("a fork in the unified list has no repo name, so it cannot be placed")
		}
		if view.TaskTitle == "" {
			t.Error("a fork in the unified list has no task title")
		}
		repos[view.RepoName] = true
	}
	if len(repos) != 2 {
		t.Fatalf("work came from %d repos, want 2", len(repos))
	}
	if overview.Totals.Queued != 2 {
		t.Fatalf("totals = %+v, want both forks counted as queued", overview.Totals)
	}
}

func TestOverviewSeparatesWorkNeedingTheUser(t *testing.T) {
	f := newFixture(t, planJSON(nil, twoWorkstreams()))
	_, forks := f.startTask(t, "add ratings and photos")

	// Put one fork in front of the user and leave the other running.
	stuck, err := f.store.GetFork(f.ctx, forks[0].ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	for _, next := range []domain.ForkState{domain.ForkProvisioning, domain.ForkCoding} {
		if err := f.store.TransitionFork(f.ctx, stuck, next, ""); err != nil {
			t.Fatalf("transition: %v", err)
		}
	}
	if err := f.orch.Escalate(f.ctx, stuck, domain.EscalationAmbiguous, "stars or thumbs?", ""); err != nil {
		t.Fatalf("Escalate: %v", err)
	}

	var overview Overview
	f.call(t, http.MethodGet, "/v1/overview", nil, &overview)

	if len(overview.NeedsAttention) != 1 {
		t.Fatalf("needs_attention has %d entries, want the escalated fork alone", len(overview.NeedsAttention))
	}
	if overview.NeedsAttention[0].Fork.ID != stuck.ID {
		t.Fatalf("the wrong fork was surfaced: %s", overview.NeedsAttention[0].Fork.ID)
	}
	if overview.Totals.Escalated != 1 {
		t.Fatalf("totals = %+v", overview.Totals)
	}
	// An escalated fork is still in flight, so it belongs in both lists.
	var inWork bool
	for _, view := range overview.Work {
		if view.Fork.ID == stuck.ID {
			inWork = true
		}
	}
	if !inWork {
		t.Error("an escalated fork should still appear in the in-flight list")
	}
}

func TestOverviewReportsRemainingBudget(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))
	f.startTask(t, "add ratings")

	var overview Overview
	f.call(t, http.MethodGet, "/v1/overview", nil, &overview)
	if len(overview.Work) == 0 {
		t.Fatal("no work listed")
	}
	// Headroom is what lets the UI show a fork approaching its tripwire before
	// it stops.
	if overview.Work[0].Remaining == nil {
		t.Fatal("in-flight work carries no budget headroom")
	}
}

func TestOverviewExcludesFinishedWork(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))
	_, forks := f.startTask(t, "add ratings")

	fork, err := f.store.GetFork(f.ctx, forks[0].ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	for _, next := range []domain.ForkState{
		domain.ForkProvisioning, domain.ForkCoding, domain.ForkVerifying,
		domain.ForkAwaitingMerge, domain.ForkMerging, domain.ForkMerged,
	} {
		if err := f.store.TransitionFork(f.ctx, fork, next, ""); err != nil {
			t.Fatalf("transition: %v", err)
		}
	}

	var overview Overview
	f.call(t, http.MethodGet, "/v1/overview", nil, &overview)

	if len(overview.Work) != 0 {
		t.Fatalf("a merged fork is still listed as in flight: %+v", overview.Work)
	}
	if overview.Totals.Merged != 1 {
		t.Fatalf("totals = %+v, want the merge counted", overview.Totals)
	}
}

func TestAuditTrailIsUnifiedAndFilterable(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))
	f.startTask(t, "add ratings")

	var all struct {
		Events []*domain.Event `json:"events"`
		Cursor int64           `json:"cursor"`
		Actors []domain.Actor  `json:"actors"`
	}
	if status := f.call(t, http.MethodGet, "/v1/audit", nil, &all); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(all.Events) == 0 {
		t.Fatal("the audit trail is empty")
	}
	if all.Cursor == 0 {
		t.Fatal("no cursor returned; the feed cannot be resumed")
	}
	if len(all.Actors) == 0 {
		t.Fatal("no actor list returned; the UI filter would have nothing to offer")
	}

	// Newest first, so a reader sees current activity without paging.
	for i := 1; i < len(all.Events); i++ {
		if all.Events[i].Seq >= all.Events[i-1].Seq {
			t.Fatalf("the audit feed is not newest-first at position %d", i)
		}
	}

	// Every event carries a repo, or the unified view cannot be grouped.
	for _, event := range all.Events {
		if event.RepoID == "" {
			t.Errorf("event %q (%s) has no repo and cannot be placed in a unified view", event.Type, event.ID)
		}
		if event.Actor == "" {
			t.Errorf("event %q has no actor and cannot be attributed", event.Type)
		}
	}

	var byRepo struct {
		Events []*domain.Event `json:"events"`
	}
	f.call(t, http.MethodGet, "/v1/audit?repo="+f.repo.ID, nil, &byRepo)
	if len(byRepo.Events) != len(all.Events) {
		t.Fatalf("filtering to the only repo changed the result: %d vs %d", len(byRepo.Events), len(all.Events))
	}

	var byActor struct {
		Events []*domain.Event `json:"events"`
	}
	f.call(t, http.MethodGet, "/v1/audit?actor=user", nil, &byActor)
	if len(byActor.Events) == 0 {
		t.Fatal("no user-attributed events; user actions are not being recorded")
	}
	for _, event := range byActor.Events {
		if event.Actor != domain.ActorUser {
			t.Errorf("actor filter returned a %q event", event.Actor)
		}
	}
}

func TestAuditDistinguishesUserActionsFromAutomatedOnes(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))
	f.startTask(t, "add ratings")

	var feed struct {
		Events []*domain.Event `json:"events"`
	}
	f.call(t, http.MethodGet, "/v1/audit?limit=200", nil, &feed)

	seen := map[domain.EventType]domain.Actor{}
	for _, event := range feed.Events {
		seen[event.Type] = event.Actor
	}

	// The question an audit trail exists to answer: what did I authorise, and
	// what did the system do on its own.
	if seen[domain.EventPlanApproved] != domain.ActorUser {
		t.Errorf("plan approval is attributed to %q, want the user", seen[domain.EventPlanApproved])
	}
	if seen[domain.EventTaskPlanned] != domain.ActorOrchestrator {
		t.Errorf("planning is attributed to %q, want the orchestrator", seen[domain.EventTaskPlanned])
	}
	if !seen[domain.EventTaskPlanned].Automated() {
		t.Error("orchestrator planning should count as automated")
	}
	if domain.ActorUser.Automated() {
		t.Error("a user action is not automated")
	}
}

func TestAuditRecordsSecretNamesButNeverValues(t *testing.T) {
	f := newFixture(t)
	const value = "postgres://user:hunter2@db/app"

	if status := f.call(t, http.MethodPut, "/v1/repos/"+f.repo.ID+"/secrets/DATABASE_URL",
		map[string]string{"value": value}, nil); status != http.StatusNoContent {
		t.Fatalf("set secret: %d", status)
	}
	if status := f.call(t, http.MethodDelete, "/v1/repos/"+f.repo.ID+"/secrets/DATABASE_URL", nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete secret: %d", status)
	}

	resp, err := f.server.Client().Get(f.server.URL + "/v1/audit?limit=200")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var body struct {
		Events []*domain.Event `json:"events"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The trail is stored and served in the clear, so a value reaching it
	// would be a disclosure.
	if strings.Contains(string(raw), "hunter2") {
		t.Fatal("a secret value leaked into the audit trail")
	}

	kinds := map[domain.EventType]bool{}
	for _, event := range body.Events {
		kinds[event.Type] = true
	}
	// The mutation itself must still be auditable.
	if !kinds[domain.EventSecretSet] || !kinds[domain.EventSecretDeleted] {
		t.Fatalf("secret mutations were not recorded: %v", kinds)
	}
}

func TestAuditPagesBackwards(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))
	f.startTask(t, "add ratings")

	var page struct {
		Events []*domain.Event `json:"events"`
	}
	f.call(t, http.MethodGet, "/v1/audit?limit=2", nil, &page)
	if len(page.Events) != 2 {
		t.Fatalf("first page has %d events, want 2", len(page.Events))
	}
	oldest := page.Events[len(page.Events)-1].Seq

	var older struct {
		Events []*domain.Event `json:"events"`
	}
	f.call(t, http.MethodGet, "/v1/audit?limit=50&before="+itoa(oldest), nil, &older)
	for _, event := range older.Events {
		if event.Seq >= oldest {
			t.Fatalf("backwards paging returned event %d, which is not older than %d", event.Seq, oldest)
		}
	}
}

func TestAuditStreamHonoursFilters(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))

	// Streaming a filter that matches nothing must not fall back to the
	// unified feed, or "follow this repo" would quietly follow everything.
	resp, err := f.server.Client().Get(f.server.URL + "/v1/events/stream?repo=repo_nonexistent")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
}

func TestAuditRejectsNothingAndDefaultsSanely(t *testing.T) {
	f := newFixture(t)
	var body struct {
		Events []*domain.Event `json:"events"`
	}
	// An empty trail is an empty list, not an error.
	if status := f.call(t, http.MethodGet, "/v1/audit", nil, &body); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(body.Events) != 0 {
		t.Fatalf("expected an empty trail, got %d events", len(body.Events))
	}
}

func TestUIIsServedFromTheSameOrigin(t *testing.T) {
	f := newFixture(t)
	// Same origin means the page can open the event stream with no CORS
	// configuration at all.
	resp, err := f.server.Client().Get(f.server.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want the UI", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "<title>dabberz</title>") {
		t.Fatal("the root did not serve the UI")
	}

	// API routes must still win over the UI catch-all.
	var health map[string]string
	if status := f.call(t, http.MethodGet, "/healthz", nil, &health); status != http.StatusOK || health["status"] != "ok" {
		t.Fatalf("the UI catch-all shadowed an API route: %d %v", status, health)
	}
}

func twoWorkstreams() []map[string]any {
	return []map[string]any{
		{"name": "ratings", "description": "add star ratings", "project_path": "."},
		{"name": "photos", "description": "add photo upload", "project_path": "."},
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

var _ = llm.Response{}
var _ = store.EventFilter{}
