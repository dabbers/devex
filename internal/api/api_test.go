package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/llm"
	"github.com/dabbers/devex/internal/memory"
	"github.com/dabbers/devex/internal/orchestrator"
	"github.com/dabbers/devex/internal/secrets"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/verify"
	"github.com/dabbers/devex/internal/vm"
	"github.com/dabbers/devex/internal/web"
)

// uiVMDriver stands in for the shared UI VM: the verification harness always
// reports a pass, so tests exercise the plumbing rather than a browser.
type uiVMDriver struct{}

func (uiVMDriver) Name() string                                  { return "stub-ui" }
func (uiVMDriver) Capacity(context.Context) (vm.Capacity, error) { return vm.Capacity{}, nil }
func (uiVMDriver) Create(context.Context, vm.Spec) (*vm.Instance, error) {
	return nil, vm.ErrNotSupported
}
func (uiVMDriver) Get(context.Context, string) (*vm.Instance, error) { return nil, vm.ErrNotFound }
func (uiVMDriver) List(context.Context) ([]*vm.Instance, error)      { return nil, nil }
func (uiVMDriver) Destroy(context.Context, string) error             { return nil }
func (uiVMDriver) Exec(context.Context, string, vm.Command) (*vm.ExecResult, error) {
	return &vm.ExecResult{Stdout: `{"passed":true,"summary":"the preview renders"}`}, nil
}

type fixture struct {
	store  *store.Store
	orch   *orchestrator.Orchestrator
	model  *llm.Mock
	vault  *secrets.Vault
	memory *memory.Store
	server *httptest.Server
	owner  *domain.User
	repo   *domain.Repo
	ctx    context.Context
}

func planJSON(questions []map[string]any, workstreams []map[string]any) llm.Response {
	body, _ := json.Marshal(map[string]any{
		"summary": "planned", "questions": questions, "workstreams": workstreams,
	})
	return llm.Response{Content: string(body), FinishReason: "stop"}
}

func oneWorkstream() []map[string]any {
	return []map[string]any{{"name": "ratings", "description": "add star ratings", "project_path": "."}}
}

func newFixture(t *testing.T, responses ...llm.Response) *fixture {
	t.Helper()
	return buildFixture(t, true, responses...)
}

// newFixtureWithoutVerifier models a deployment with no shared UI VM.
func newFixtureWithoutVerifier(t *testing.T) *fixture {
	t.Helper()
	return buildFixture(t, false)
}

func buildFixture(t *testing.T, withVerifier bool, responses ...llm.Response) *fixture {
	t.Helper()
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	owner, err := st.EnsureUser(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	repo := &domain.Repo{UserID: owner.ID, Name: "devex", RemoteURL: "git@example.com:devex.git", DefaultBranch: "main"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	mem, err := memory.New(t.TempDir())
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	vault, err := secrets.NewVault(st, make([]byte, secrets.KeySize))
	if err != nil {
		t.Fatalf("secrets.NewVault: %v", err)
	}

	model := llm.NewMock(responses...)
	orch := orchestrator.New(st, model, mem, nil, orchestrator.Options{Logger: quiet})

	ui, err := web.Handler()
	if err != nil {
		t.Fatalf("web.Handler: %v", err)
	}
	var verifier *verify.Verifier
	if withVerifier {
		if verifier, err = verify.New(uiVMDriver{}, verify.Config{UIInstanceID: "vm_ui", Profiles: 2}); err != nil {
			t.Fatalf("verify.New: %v", err)
		}
	}

	srv, err := New(Deps{
		Store: st, Orch: orch, Vault: vault, Memory: mem, UI: ui,
		Verifier: verifier, Owner: owner, Logger: quiet,
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	// The stream polls the store; keep tests quick.
	srv.streamGap = 5 * time.Millisecond

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	return &fixture{store: st, orch: orch, model: model, vault: vault, memory: mem, server: httpSrv, owner: owner, repo: repo, ctx: ctx}
}

// call issues a request and decodes the JSON response.
func (f *fixture) call(t *testing.T, method, path string, body any, into any) int {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	if into != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil && err != io.EOF {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func TestHealth(t *testing.T) {
	f := newFixture(t)
	var body map[string]string
	if status := f.call(t, http.MethodGet, "/healthz", nil, &body); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestRepoLifecycle(t *testing.T) {
	f := newFixture(t)

	var created domain.Repo
	status := f.call(t, http.MethodPost, "/v1/repos", map[string]string{
		"name": "other", "remote_url": "git@example.com:other.git",
	}, &created)
	if status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	if created.ID == "" || created.UserID != f.owner.ID {
		t.Fatalf("created = %+v", created)
	}

	var listed struct {
		Repos []*domain.Repo `json:"repos"`
	}
	f.call(t, http.MethodGet, "/v1/repos", nil, &listed)
	if len(listed.Repos) != 2 {
		t.Fatalf("listed %d repos, want 2", len(listed.Repos))
	}

	// A duplicate name is a conflict, not a generic failure.
	status = f.call(t, http.MethodPost, "/v1/repos", map[string]string{
		"name": "other", "remote_url": "git@example.com:other.git",
	}, nil)
	if status != http.StatusConflict {
		t.Fatalf("duplicate repo status = %d, want 409", status)
	}
}

func TestCreateRepoValidatesInput(t *testing.T) {
	f := newFixture(t)
	if status := f.call(t, http.MethodPost, "/v1/repos", map[string]string{"name": "x"}, nil); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestUnknownRepoIsNotFound(t *testing.T) {
	f := newFixture(t)
	if status := f.call(t, http.MethodGet, "/v1/repos/repo_missing", nil, nil); status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	f := newFixture(t)
	// A typo in a merge preference must not be silently ignored: it would
	// change where finished work lands.
	status := f.call(t, http.MethodPost, "/v1/tasks", map[string]any{
		"repo_id": f.repo.ID, "request": "add ratings", "merge_targets": "default_branch",
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field", status)
	}
}

func TestTaskPlanningCheckpointFlow(t *testing.T) {
	f := newFixture(t,
		planJSON([]map[string]any{{"text": "Stars or thumbs?"}}, oneWorkstream()),
		planJSON(nil, oneWorkstream()),
	)

	var task domain.Task
	status := f.call(t, http.MethodPost, "/v1/tasks", map[string]any{
		"repo_id": f.repo.ID, "request": "add ratings", "plan": true,
	}, &task)
	if status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	if task.State != domain.TaskAwaitingPlan {
		t.Fatalf("state = %q, want the planning checkpoint", task.State)
	}

	// Approving before the questions are answered must be refused.
	if status := f.call(t, http.MethodPost, "/v1/tasks/"+task.ID+"/approve", nil, nil); status == http.StatusOK {
		t.Fatal("approving with open questions should have been refused")
	}

	questionID := task.Plan.Questions[0].ID
	var refined domain.Task
	status = f.call(t, http.MethodPost, "/v1/tasks/"+task.ID+"/answers", map[string]any{
		"answers": map[string]string{questionID: "stars"},
	}, &refined)
	if status != http.StatusOK {
		t.Fatalf("answer status = %d", status)
	}
	if refined.Plan.NeedsInput() {
		t.Fatal("the refined plan should have no open questions")
	}

	var approved struct {
		Task  *domain.Task   `json:"task"`
		Forks []*domain.Fork `json:"forks"`
	}
	status = f.call(t, http.MethodPost, "/v1/tasks/"+task.ID+"/approve", nil, &approved)
	if status != http.StatusOK {
		t.Fatalf("approve status = %d", status)
	}
	if approved.Task.State != domain.TaskRunning || len(approved.Forks) != 1 {
		t.Fatalf("approved = %+v", approved)
	}
	// Forks are queued, not started: the scheduler decides when they run.
	if approved.Forks[0].State != domain.ForkQueued {
		t.Fatalf("fork state = %q, want queued", approved.Forks[0].State)
	}
}

func TestLifecycleViolationsAreConflicts(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))

	var task domain.Task
	f.call(t, http.MethodPost, "/v1/tasks", map[string]any{
		"repo_id": f.repo.ID, "request": "add ratings", "plan": true,
	}, &task)
	f.call(t, http.MethodPost, "/v1/tasks/"+task.ID+"/approve", nil, nil)

	// Approving a second time is a client mistake about state, not a server
	// failure.
	status := f.call(t, http.MethodPost, "/v1/tasks/"+task.ID+"/approve", nil, nil)
	if status != http.StatusConflict && status != http.StatusBadRequest {
		t.Fatalf("status = %d, want a 4xx", status)
	}
}

func TestGetTaskIncludesItsForks(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))

	var task domain.Task
	f.call(t, http.MethodPost, "/v1/tasks", map[string]any{
		"repo_id": f.repo.ID, "request": "add ratings", "plan": true,
	}, &task)
	f.call(t, http.MethodPost, "/v1/tasks/"+task.ID+"/approve", nil, nil)

	var got struct {
		Task  *domain.Task   `json:"task"`
		Forks []*domain.Fork `json:"forks"`
	}
	if status := f.call(t, http.MethodGet, "/v1/tasks/"+task.ID, nil, &got); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(got.Forks) != 1 {
		t.Fatalf("got %d forks", len(got.Forks))
	}
}

func TestForkViewReportsRemainingBudget(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))

	var task domain.Task
	f.call(t, http.MethodPost, "/v1/tasks", map[string]any{
		"repo_id": f.repo.ID, "request": "add ratings", "plan": true,
	}, &task)
	var approved struct {
		Forks []*domain.Fork `json:"forks"`
	}
	f.call(t, http.MethodPost, "/v1/tasks/"+task.ID+"/approve", nil, &approved)

	var view map[string]any
	if status := f.call(t, http.MethodGet, "/v1/forks/"+approved.Forks[0].ID, nil, &view); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	// The UI needs to show how much budget a running fork has left before the
	// tripwire stops it.
	if _, ok := view["budget_remaining"]; !ok {
		t.Fatalf("fork view has no budget headroom: %v", view)
	}
}

func TestResolveEscalationResumesAFork(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))

	var task domain.Task
	f.call(t, http.MethodPost, "/v1/tasks", map[string]any{
		"repo_id": f.repo.ID, "request": "add ratings", "plan": true,
	}, &task)
	var approved struct {
		Forks []*domain.Fork `json:"forks"`
	}
	f.call(t, http.MethodPost, "/v1/tasks/"+task.ID+"/approve", nil, &approved)

	fork, err := f.store.GetFork(f.ctx, approved.Forks[0].ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	for _, next := range []domain.ForkState{domain.ForkProvisioning, domain.ForkCoding} {
		if err := f.store.TransitionFork(f.ctx, fork, next, ""); err != nil {
			t.Fatalf("transition: %v", err)
		}
	}
	// Escalate the way the pipeline does, so there is a real escalation record
	// for the user's answer to land on.
	if err := f.orch.Escalate(f.ctx, fork, domain.EscalationAmbiguous, "stars or thumbs?", ""); err != nil {
		t.Fatalf("Escalate: %v", err)
	}

	var resumed domain.Fork
	status := f.call(t, http.MethodPost, "/v1/forks/"+fork.ID+"/resolve", map[string]any{
		"response": "use stars", "resume": string(domain.ForkCoding),
	}, &resumed)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if resumed.State != domain.ForkCoding {
		t.Fatalf("state = %q", resumed.State)
	}
	if resumed.Escalation.Response != "use stars" {
		t.Fatalf("the response was not recorded: %+v", resumed.Escalation)
	}
}

func TestCancelTask(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))

	var task domain.Task
	f.call(t, http.MethodPost, "/v1/tasks", map[string]any{
		"repo_id": f.repo.ID, "request": "add ratings", "plan": true,
	}, &task)

	if status := f.call(t, http.MethodPost, "/v1/tasks/"+task.ID+"/cancel", map[string]string{"reason": "changed my mind"}, nil); status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", status)
	}
	got, err := f.store.GetTask(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != domain.TaskCancelled {
		t.Fatalf("state = %q", got.State)
	}
}

func TestSecretsEndpointsNeverReturnValues(t *testing.T) {
	f := newFixture(t)

	status := f.call(t, http.MethodPut, "/v1/repos/"+f.repo.ID+"/secrets/DATABASE_URL",
		map[string]string{"value": "postgres://secret"}, nil)
	if status != http.StatusNoContent {
		t.Fatalf("status = %d", status)
	}

	var listed struct {
		Secrets []string `json:"secrets"`
	}
	f.call(t, http.MethodGet, "/v1/repos/"+f.repo.ID+"/secrets", nil, &listed)
	if len(listed.Secrets) != 1 || listed.Secrets[0] != "DATABASE_URL" {
		t.Fatalf("listed = %v", listed.Secrets)
	}

	// The listing must not expose values: it is a UI call with no business
	// decrypting anything.
	resp, err := f.server.Client().Get(f.server.URL + "/v1/repos/" + f.repo.ID + "/secrets")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "postgres://secret") {
		t.Fatalf("a secret value leaked into the listing: %s", raw)
	}

	if status := f.call(t, http.MethodDelete, "/v1/repos/"+f.repo.ID+"/secrets/DATABASE_URL", nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete status = %d", status)
	}
	if status := f.call(t, http.MethodDelete, "/v1/repos/"+f.repo.ID+"/secrets/DATABASE_URL", nil, nil); status != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", status)
	}
}

func TestInvalidSecretNameIsRejected(t *testing.T) {
	f := newFixture(t)
	status := f.call(t, http.MethodPut, "/v1/repos/"+f.repo.ID+"/secrets/lower-case",
		map[string]string{"value": "x"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestMemoryEndpoints(t *testing.T) {
	f := newFixture(t)

	var written memory.Finding
	status := f.call(t, http.MethodPut, "/v1/repos/"+f.repo.ID+"/memory/design-language", map[string]any{
		"title": "Design language", "tags": []string{"design"}, "body": "Accent is teal.",
	}, &written)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if written.Slug != "design-language" {
		t.Fatalf("slug = %q", written.Slug)
	}

	var listed struct {
		Findings []*memory.Finding `json:"findings"`
	}
	f.call(t, http.MethodGet, "/v1/repos/"+f.repo.ID+"/memory?tag=design", nil, &listed)
	if len(listed.Findings) != 1 {
		t.Fatalf("tag search returned %d findings", len(listed.Findings))
	}

	f.call(t, http.MethodGet, "/v1/repos/"+f.repo.ID+"/memory?q=nothing-matches", nil, &listed)
	if len(listed.Findings) != 0 {
		t.Fatalf("text search returned %d findings, want 0", len(listed.Findings))
	}
}

func TestDiscoveryAndConfirmation(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"projects": []map[string]any{
		{"name": "web", "path": "apps/web", "toolchain": "node"},
	}})
	f := newFixture(t, llm.Response{Content: string(body), FinishReason: "stop"})

	var discovered struct {
		Projects []*domain.Project `json:"projects"`
	}
	status := f.call(t, http.MethodPost, "/v1/repos/"+f.repo.ID+"/discover",
		map[string]any{"tree": []string{"apps/web/package.json"}}, &discovered)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(discovered.Projects) != 1 || discovered.Projects[0].Confirmed {
		t.Fatalf("discovery should propose, not confirm: %+v", discovered.Projects)
	}

	var confirmed struct {
		Projects []*domain.Project `json:"projects"`
	}
	f.call(t, http.MethodPost, "/v1/repos/"+f.repo.ID+"/projects/confirm",
		map[string]any{"paths": []string{"apps/web"}}, &confirmed)
	if len(confirmed.Projects) != 1 || !confirmed.Projects[0].Confirmed {
		t.Fatalf("confirmation did not take: %+v", confirmed.Projects)
	}
}

func TestEventsListAndCursor(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))

	var task domain.Task
	f.call(t, http.MethodPost, "/v1/tasks", map[string]any{
		"repo_id": f.repo.ID, "request": "add ratings", "plan": true,
	}, &task)

	var first struct {
		Events []*domain.Event `json:"events"`
	}
	f.call(t, http.MethodGet, "/v1/events?task="+task.ID, nil, &first)
	if len(first.Events) == 0 {
		t.Fatal("no events recorded for the task")
	}

	cursor := first.Events[len(first.Events)-1].Seq
	var rest struct {
		Events []*domain.Event `json:"events"`
	}
	f.call(t, http.MethodGet, "/v1/events?task="+task.ID+"&after="+strconv.FormatInt(cursor, 10), nil, &rest)
	// Resuming from the last seen sequence number must replay nothing.
	if len(rest.Events) != 0 {
		t.Fatalf("cursor replayed %d events", len(rest.Events))
	}
}

func TestEventStreamDeliversNewEventsWithResumableIDs(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))

	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/v1/events/stream", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}

	// Generate activity after the stream is open.
	go func() {
		var task domain.Task
		f.call(t, http.MethodPost, "/v1/tasks", map[string]any{
			"repo_id": f.repo.ID, "request": "add ratings", "plan": true,
		}, &task)
	}()

	buf := make([]byte, 4096)
	var seen strings.Builder
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
			if strings.Contains(seen.String(), "task.created") {
				break
			}
		}
		if err != nil {
			break
		}
	}

	body := seen.String()
	if !strings.Contains(body, "task.created") {
		t.Fatalf("the stream did not deliver the event:\n%s", body)
	}
	// Each frame carries its sequence number so a dropped client resumes
	// exactly where it left off.
	if !strings.Contains(body, "id: ") {
		t.Fatalf("stream frames carry no resume cursor:\n%s", body)
	}
}

func TestNewValidatesDependencies(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Error("an API server without its core dependencies should be rejected")
	}
}
