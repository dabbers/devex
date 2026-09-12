package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/llm"
	"github.com/dabbers/devex/internal/memory"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/tripwire"
)

type nudgeCounter struct{ n atomic.Int32 }

func (c *nudgeCounter) Nudge() { c.n.Add(1) }

type fixture struct {
	store *store.Store
	model *llm.Mock
	mem   *memory.Store
	nudge *nudgeCounter
	orch  *Orchestrator
	user  *domain.User
	repo  *domain.Repo
	ctx   context.Context
}

func newFixture(t *testing.T, responses ...llm.Response) *fixture {
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
	repo := &domain.Repo{UserID: user.ID, Name: "devex", RemoteURL: "git@example.com:devex.git", DefaultBranch: "main"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	mem, err := memory.New(t.TempDir())
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}

	model := llm.NewMock(responses...)
	nudge := &nudgeCounter{}
	orch := New(st, model, mem, nudge, Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return &fixture{store: st, model: model, mem: mem, nudge: nudge, orch: orch, user: user, repo: repo, ctx: ctx}
}

// planJSON renders a planner response body.
func planJSON(summary string, questions []map[string]any, workstreams []map[string]any) llm.Response {
	body, _ := json.Marshal(map[string]any{
		"summary":     summary,
		"questions":   questions,
		"workstreams": workstreams,
	})
	return llm.Response{Content: string(body), FinishReason: "stop"}
}

func threeWorkstreams() []map[string]any {
	return []map[string]any{
		{"name": "ratings", "description": "add star ratings", "project_path": "."},
		{"name": "photo-upload", "description": "add photo upload", "project_path": ".",
			"serialize_group": "media", "overlap_rationale": "both touch image handling"},
		{"name": "image-resize", "description": "resize uploaded images", "project_path": ".",
			"serialize_group": "media", "overlap_rationale": "both touch image handling"},
	}
}

func (f *fixture) newTask(t *testing.T) *domain.Task {
	t.Helper()
	task, err := f.orch.CreateTask(f.ctx, CreateTaskRequest{
		RepoID:  f.repo.ID,
		Request: "add ratings, photo upload and image resizing",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task
}

func TestCreateTaskRequiresARequest(t *testing.T) {
	f := newFixture(t)
	if _, err := f.orch.CreateTask(f.ctx, CreateTaskRequest{RepoID: f.repo.ID, Request: "   "}); err == nil {
		t.Error("an empty request should be rejected")
	}
}

func TestCreateTaskDefaultsMergePreferencesConservatively(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t)

	// Merge preferences are per-task and never inherited from elsewhere, so an
	// unset value has to fall back to a defined default.
	if task.MergeTarget != domain.MergeTargetDefaultBranch {
		t.Fatalf("merge target = %q", task.MergeTarget)
	}
	if task.MergeTiming != domain.MergeTimingImmediate {
		t.Fatalf("merge timing = %q", task.MergeTiming)
	}
	if task.State != domain.TaskDraft {
		t.Fatalf("state = %q, want draft", task.State)
	}
	if task.Title == "" {
		t.Fatal("a title should be derived from the request")
	}
}

func TestCreateTaskRejectsUnknownPreferences(t *testing.T) {
	f := newFixture(t)
	if _, err := f.orch.CreateTask(f.ctx, CreateTaskRequest{RepoID: f.repo.ID, Request: "x", MergeTarget: "somewhere"}); err == nil {
		t.Error("an unknown merge target should be rejected")
	}
	if _, err := f.orch.CreateTask(f.ctx, CreateTaskRequest{RepoID: f.repo.ID, Request: "x", MergeTiming: "sometime"}); err == nil {
		t.Error("an unknown merge timing should be rejected")
	}
}

func TestCreateTaskNamesAnIntegrationBranch(t *testing.T) {
	f := newFixture(t)
	task, err := f.orch.CreateTask(f.ctx, CreateTaskRequest{
		RepoID:      f.repo.ID,
		Request:     "x",
		MergeTarget: domain.MergeTargetIntegrationBranch,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.IntegrationBranch == "" {
		t.Fatal("an integration-branch task needs a branch name")
	}
	if !strings.HasPrefix(task.IntegrationBranch, "dabberz/") {
		t.Fatalf("integration branch = %q", task.IntegrationBranch)
	}
}

func TestPlanParksAtTheCheckpointWithoutStartingWork(t *testing.T) {
	f := newFixture(t, planJSON("three workstreams", nil, threeWorkstreams()))
	task := f.newTask(t)

	planned, err := f.orch.Plan(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if planned.State != domain.TaskAwaitingPlan {
		t.Fatalf("state = %q, want awaiting_plan", planned.State)
	}
	if len(planned.Plan.Workstreams) != 3 {
		t.Fatalf("got %d workstreams, want 3", len(planned.Plan.Workstreams))
	}
	// The planning checkpoint exists precisely so nothing spins up first.
	forks, err := f.store.ListForks(f.ctx, store.ForkFilter{TaskID: task.ID})
	if err != nil {
		t.Fatalf("ListForks: %v", err)
	}
	if len(forks) != 0 {
		t.Fatalf("planning created %d forks before approval", len(forks))
	}
	if f.nudge.n.Load() != 0 {
		t.Fatal("planning should not wake the scheduler")
	}
}

func TestPlanCarriesOverlapJudgementThrough(t *testing.T) {
	f := newFixture(t, planJSON("three workstreams", nil, threeWorkstreams()))
	task := f.newTask(t)
	planned, err := f.orch.Plan(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Overlap is decided once, here. If the group and its rationale are lost,
	// nothing downstream will catch the collision.
	var grouped int
	for _, w := range planned.Plan.Workstreams {
		if w.SerializeGroup == "media" {
			grouped++
			if w.OverlapRationale == "" {
				t.Errorf("workstream %q was grouped with no recorded rationale", w.Name)
			}
		}
	}
	if grouped != 2 {
		t.Fatalf("%d workstreams landed in the media group, want 2", grouped)
	}
}

func TestPlanSurfacesClarifyingQuestions(t *testing.T) {
	f := newFixture(t, planJSON("needs input",
		[]map[string]any{{"text": "Star ratings or thumbs?", "options": []string{"stars", "thumbs"}}},
		threeWorkstreams()[:1],
	))
	task := f.newTask(t)

	planned, err := f.orch.Plan(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !planned.Plan.NeedsInput() {
		t.Fatal("a plan with an unanswered question should need input")
	}
	if planned.Plan.Questions[0].ID == "" {
		t.Fatal("questions need ids so answers can be matched to them")
	}

	// An unanswered plan must not be approvable.
	if _, _, err := f.orch.ApprovePlan(f.ctx, task.ID); err == nil {
		t.Fatal("approving a plan with open questions should be refused")
	}
}

func TestAnswerQuestionsReplansWithTheAnswers(t *testing.T) {
	f := newFixture(t,
		planJSON("needs input", []map[string]any{{"text": "Star ratings or thumbs?"}}, threeWorkstreams()[:1]),
		planJSON("refined", nil, threeWorkstreams()),
	)
	task := f.newTask(t)
	planned, err := f.orch.Plan(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	questionID := planned.Plan.Questions[0].ID
	refined, err := f.orch.AnswerQuestions(f.ctx, task.ID, map[string]string{questionID: "stars"})
	if err != nil {
		t.Fatalf("AnswerQuestions: %v", err)
	}
	if refined.Plan.Round != 2 {
		t.Fatalf("round = %d, want 2", refined.Plan.Round)
	}
	if refined.Plan.NeedsInput() {
		t.Fatal("the refined plan should have no open questions")
	}

	// The answer has to reach the model, or the second round would repeat the
	// same question.
	last := f.model.LastRequest()
	if !strings.Contains(last.Messages[1].Content, "stars") {
		t.Fatal("the user's answer was not carried into the replanning prompt")
	}
}

func TestAnswerQuestionsRejectsUnmatchedAnswers(t *testing.T) {
	f := newFixture(t, planJSON("needs input", []map[string]any{{"text": "Which?"}}, threeWorkstreams()[:1]))
	task := f.newTask(t)
	if _, err := f.orch.Plan(f.ctx, task.ID); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := f.orch.AnswerQuestions(f.ctx, task.ID, map[string]string{"qst_nonexistent": "x"}); err == nil {
		t.Fatal("answers that match no question should be rejected")
	}
}

func TestApprovePlanQueuesAForkPerWorkstream(t *testing.T) {
	f := newFixture(t, planJSON("three workstreams", nil, threeWorkstreams()))
	task := f.newTask(t)
	if _, err := f.orch.Plan(f.ctx, task.ID); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	running, forks, err := f.orch.ApprovePlan(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	if running.State != domain.TaskRunning {
		t.Fatalf("task state = %q, want running", running.State)
	}
	if len(forks) != 3 {
		t.Fatalf("created %d forks, want 3", len(forks))
	}

	branches := map[string]bool{}
	for _, fork := range forks {
		if fork.State != domain.ForkQueued {
			t.Errorf("fork %s is %q, want queued", fork.Name, fork.State)
		}
		if fork.Branch == "" {
			t.Errorf("fork %s has no branch", fork.Name)
		}
		if branches[fork.Branch] {
			t.Errorf("branch %q was assigned twice", fork.Branch)
		}
		branches[fork.Branch] = true
	}

	// The serialization groups survive into the forks the scheduler reads.
	var grouped int
	for _, fork := range forks {
		if fork.SerializeGroup == "media" {
			grouped++
		}
	}
	if grouped != 2 {
		t.Fatalf("%d forks carry the media group, want 2", grouped)
	}

	if f.nudge.n.Load() == 0 {
		t.Fatal("approving a plan should wake the scheduler")
	}
}

func TestApprovePlanIsTheOnlyPathThatCreatesForks(t *testing.T) {
	f := newFixture(t, planJSON("one workstream", nil, threeWorkstreams()[:1]))
	task := f.newTask(t)

	// Approving a draft that never reached the checkpoint must be refused.
	if _, _, err := f.orch.ApprovePlan(f.ctx, task.ID); err == nil {
		t.Fatal("approving an unplanned task should be refused")
	}
	if _, err := f.orch.Plan(f.ctx, task.ID); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, _, err := f.orch.ApprovePlan(f.ctx, task.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	// Approving twice must not duplicate the work.
	if _, _, err := f.orch.ApprovePlan(f.ctx, task.ID); err == nil {
		t.Fatal("approving twice should be refused")
	}
}

func TestApprovePlanCreatesMissingProjects(t *testing.T) {
	f := newFixture(t, planJSON("nested", nil, []map[string]any{
		{"name": "api-work", "description": "d", "project_path": "services/api"},
	}))
	task := f.newTask(t)
	if _, err := f.orch.Plan(f.ctx, task.ID); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, _, err := f.orch.ApprovePlan(f.ctx, task.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}

	project, err := f.store.GetProjectByPath(f.ctx, f.repo.ID, "services/api")
	if err != nil {
		t.Fatalf("a workstream targeting an undiscovered path should still start: %v", err)
	}
	if project.Name != "api" {
		t.Fatalf("project name = %q", project.Name)
	}
}

func TestPlanRejectsAnEmptyDecomposition(t *testing.T) {
	f := newFixture(t, planJSON("nothing to do", nil, nil))
	task := f.newTask(t)
	if _, err := f.orch.Plan(f.ctx, task.ID); err == nil {
		t.Fatal("a plan with no workstreams and no questions should be rejected")
	}

	got, err := f.store.GetTask(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	// The task must not be left stranded mid-transition.
	if got.State != domain.TaskFailed {
		t.Fatalf("state = %q, want failed", got.State)
	}
	if got.StateReason == "" {
		t.Fatal("a failed task should carry a reason")
	}
}

func TestPlanRejectsTruncatedAndUnparseableResponses(t *testing.T) {
	truncated := newFixture(t, llm.Response{Content: `{"summary": "cut`, FinishReason: "length"})
	task := truncated.newTask(t)
	if _, err := truncated.orch.Plan(truncated.ctx, task.ID); err == nil {
		t.Error("a truncated plan should be rejected")
	}

	garbage := newFixture(t, llm.Response{Content: "I'm afraid I can't do that", FinishReason: "stop"})
	task2 := garbage.newTask(t)
	if _, err := garbage.orch.Plan(garbage.ctx, task2.ID); err == nil {
		t.Error("a non-JSON plan should be rejected")
	}
}

func TestPlanIncludesRepoMemory(t *testing.T) {
	f := newFixture(t, planJSON("planned", nil, threeWorkstreams()[:1]))
	if err := f.mem.Put(f.ctx, f.repo.ID, &memory.Finding{
		Slug: "design-language", Title: "Design language", Tags: []string{"design"},
		Body: "Accent colour is teal throughout.",
	}); err != nil {
		t.Fatalf("memory.Put: %v", err)
	}

	task := f.newTask(t)
	if _, err := f.orch.Plan(f.ctx, task.ID); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Memory exists so a later plan does not re-derive what an earlier one
	// already learned.
	prompt := f.model.LastRequest().Messages[1].Content
	if !strings.Contains(prompt, "Design language") || !strings.Contains(prompt, "teal") {
		t.Fatalf("repo memory did not reach the planning prompt:\n%s", prompt)
	}
}

func TestEscalationIsPerForkAndLeavesSiblingsRunning(t *testing.T) {
	f := newFixture(t, planJSON("two", nil, threeWorkstreams()[:2]))
	task := f.newTask(t)
	if _, err := f.orch.Plan(f.ctx, task.ID); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	_, forks, err := f.orch.ApprovePlan(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}

	for _, fork := range forks {
		for _, next := range []domain.ForkState{domain.ForkProvisioning, domain.ForkCoding} {
			if err := f.store.TransitionFork(f.ctx, fork, next, ""); err != nil {
				t.Fatalf("transition: %v", err)
			}
		}
	}

	if err := f.orch.Escalate(f.ctx, forks[0], domain.EscalationAmbiguous, "no clear direction", "detail"); err != nil {
		t.Fatalf("Escalate: %v", err)
	}

	stuck, err := f.store.GetFork(f.ctx, forks[0].ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	if stuck.State != domain.ForkEscalated || stuck.Escalation == nil {
		t.Fatalf("escalated fork = %+v", stuck)
	}
	// The whole batch must not pause because one fork needs the user.
	sibling, err := f.store.GetFork(f.ctx, forks[1].ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	if sibling.State != domain.ForkCoding {
		t.Fatalf("sibling state = %q, want it still running", sibling.State)
	}
}

func TestCheckTripwireEscalatesAndResumeClearsTheCounter(t *testing.T) {
	f := newFixture(t, planJSON("one", nil, threeWorkstreams()[:1]))
	f.orch.opts.Tripwire = tripwire.Thresholds{MaxCycles: 3}

	task := f.newTask(t)
	if _, err := f.orch.Plan(f.ctx, task.ID); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	_, forks, err := f.orch.ApprovePlan(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	fork := forks[0]
	for _, next := range []domain.ForkState{domain.ForkProvisioning, domain.ForkCoding, domain.ForkVerifying, domain.ForkFixing} {
		if err := f.store.TransitionFork(f.ctx, fork, next, ""); err != nil {
			t.Fatalf("transition: %v", err)
		}
	}

	fork.Usage.Cycles = 2
	if fired, err := f.orch.CheckTripwire(f.ctx, fork); err != nil || fired {
		t.Fatalf("CheckTripwire under the limit = %v, %v", fired, err)
	}

	fork.Usage.Cycles = 3
	fired, err := f.orch.CheckTripwire(f.ctx, fork)
	if err != nil {
		t.Fatalf("CheckTripwire: %v", err)
	}
	if !fired {
		t.Fatal("the cycle threshold should have fired")
	}
	if fork.Escalation.Kind != domain.EscalationTripwire {
		t.Fatalf("escalation kind = %q", fork.Escalation.Kind)
	}

	resumed, err := f.orch.ResolveEscalation(f.ctx, fork.ID, "try a different approach", domain.ForkFixing)
	if err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}
	if resumed.State != domain.ForkFixing {
		t.Fatalf("resumed state = %q", resumed.State)
	}
	// Resuming without clearing the counter would trip again on the next check
	// and strand the fork.
	if resumed.Usage.Cycles != 0 {
		t.Fatalf("cycle counter = %d after resuming from a tripwire, want 0", resumed.Usage.Cycles)
	}
	if resumed.Escalation.Response == "" || resumed.Escalation.ResolvedAt == nil {
		t.Fatalf("the user's response was not recorded: %+v", resumed.Escalation)
	}
}

func TestResolveEscalationRejectsForksThatAreNotEscalated(t *testing.T) {
	f := newFixture(t, planJSON("one", nil, threeWorkstreams()[:1]))
	task := f.newTask(t)
	if _, err := f.orch.Plan(f.ctx, task.ID); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	_, forks, err := f.orch.ApprovePlan(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	if _, err := f.orch.ResolveEscalation(f.ctx, forks[0].ID, "x", domain.ForkCoding); err == nil {
		t.Fatal("resolving a fork that is not escalated should be refused")
	}
}

// approveAndAdvance plans, approves and walks every fork forward to
// awaiting_merge. A fork named in hold stops at (and including) the state it
// maps to, which is how a test leaves one workstream still in flight.
func (f *fixture) approveAndAdvance(t *testing.T, task *domain.Task, hold map[string]domain.ForkState) []*domain.Fork {
	t.Helper()
	if _, err := f.orch.Plan(f.ctx, task.ID); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	_, forks, err := f.orch.ApprovePlan(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	for _, fork := range forks {
		stopAt, held := hold[fork.Name]
		for _, next := range []domain.ForkState{domain.ForkProvisioning, domain.ForkCoding, domain.ForkVerifying, domain.ForkAwaitingMerge} {
			if err := f.store.TransitionFork(f.ctx, fork, next, ""); err != nil {
				t.Fatalf("transition %s -> %s: %v", fork.Name, next, err)
			}
			if held && next == stopAt {
				break
			}
		}
	}
	return forks
}

func TestReadyToMergeUnderImmediateTiming(t *testing.T) {
	f := newFixture(t, planJSON("two", nil, threeWorkstreams()[:2]))
	task := f.newTask(t)
	// One fork is still coding; under immediate timing the other should not
	// wait for it.
	f.approveAndAdvance(t, task, map[string]domain.ForkState{"photo-upload": domain.ForkVerifying})

	ready, err := f.orch.ReadyToMerge(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ReadyToMerge: %v", err)
	}
	if len(ready) != 1 || ready[0].Name != "ratings" {
		t.Fatalf("ready = %+v, want just the finished workstream", names(ready))
	}
}

func TestReadyToMergeUnderBatchTimingWaitsForSiblings(t *testing.T) {
	f := newFixture(t, planJSON("two", nil, threeWorkstreams()[:2]))
	task, err := f.orch.CreateTask(f.ctx, CreateTaskRequest{
		RepoID: f.repo.ID, Request: "batch please", MergeTiming: domain.MergeTimingBatch,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	forks := f.approveAndAdvance(t, task, map[string]domain.ForkState{"photo-upload": domain.ForkVerifying})

	ready, err := f.orch.ReadyToMerge(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ReadyToMerge: %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("batch timing released %v while a sibling was still working", names(ready))
	}

	// Once the straggler finishes, the whole batch is released together.
	for _, fork := range forks {
		if fork.Name != "photo-upload" {
			continue
		}
		current, err := f.store.GetFork(f.ctx, fork.ID)
		if err != nil {
			t.Fatalf("GetFork: %v", err)
		}
		if err := f.store.TransitionFork(f.ctx, current, domain.ForkAwaitingMerge, ""); err != nil {
			t.Fatalf("transition: %v", err)
		}
	}
	ready, err = f.orch.ReadyToMerge(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ReadyToMerge: %v", err)
	}
	if len(ready) != 2 {
		t.Fatalf("released %d forks, want the whole batch of 2", len(ready))
	}
}

func TestBatchTimingIsNotBlockedByAnEscalatedSibling(t *testing.T) {
	f := newFixture(t, planJSON("two", nil, threeWorkstreams()[:2]))
	task, err := f.orch.CreateTask(f.ctx, CreateTaskRequest{
		RepoID: f.repo.ID, Request: "batch please", MergeTiming: domain.MergeTimingBatch,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	forks := f.approveAndAdvance(t, task, map[string]domain.ForkState{"photo-upload": domain.ForkVerifying})

	// A fork waiting on the user is not "still working": it could sit there
	// indefinitely, and holding the batch on it would stall the whole task.
	for _, fork := range forks {
		if fork.Name != "photo-upload" {
			continue
		}
		current, _ := f.store.GetFork(f.ctx, fork.ID)
		if err := f.orch.Escalate(f.ctx, current, domain.EscalationAmbiguous, "stuck", ""); err != nil {
			t.Fatalf("Escalate: %v", err)
		}
	}

	ready, err := f.orch.ReadyToMerge(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ReadyToMerge: %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("ready = %v, want the finished sibling released", names(ready))
	}
}

func TestReconcileCompletesATaskOnlyWhenNothingCanProgress(t *testing.T) {
	f := newFixture(t, planJSON("two", nil, threeWorkstreams()[:2]))
	task := f.newTask(t)
	forks := f.approveAndAdvance(t, task, nil)

	// Still awaiting merge: not finished.
	got, err := f.orch.Reconcile(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.State != domain.TaskRunning {
		t.Fatalf("state = %q, want still running", got.State)
	}

	for _, fork := range forks {
		current, _ := f.store.GetFork(f.ctx, fork.ID)
		for _, next := range []domain.ForkState{domain.ForkMerging, domain.ForkMerged} {
			if err := f.store.TransitionFork(f.ctx, current, next, ""); err != nil {
				t.Fatalf("transition: %v", err)
			}
		}
	}

	got, err = f.orch.Reconcile(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.State != domain.TaskCompleted {
		t.Fatalf("state = %q, want completed", got.State)
	}
}

func TestReconcileHoldsWhileAForkIsEscalated(t *testing.T) {
	f := newFixture(t, planJSON("one", nil, threeWorkstreams()[:1]))
	task := f.newTask(t)
	forks := f.approveAndAdvance(t, task, nil)

	current, _ := f.store.GetFork(f.ctx, forks[0].ID)
	if err := f.orch.Escalate(f.ctx, current, domain.EscalationAmbiguous, "needs you", ""); err != nil {
		t.Fatalf("Escalate: %v", err)
	}

	got, err := f.orch.Reconcile(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// A task with a fork waiting on the user is not done; it is blocked.
	if got.State != domain.TaskRunning {
		t.Fatalf("state = %q, want still running while a fork awaits the user", got.State)
	}
}

func TestCancelAbandonsUnfinishedForks(t *testing.T) {
	f := newFixture(t, planJSON("two", nil, threeWorkstreams()[:2]))
	task := f.newTask(t)
	forks := f.approveAndAdvance(t, task, map[string]domain.ForkState{"photo-upload": domain.ForkVerifying})

	if err := f.orch.Cancel(f.ctx, task.ID, "changed my mind"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	got, err := f.store.GetTask(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != domain.TaskCancelled {
		t.Fatalf("state = %q", got.State)
	}
	for _, fork := range forks {
		current, err := f.store.GetFork(f.ctx, fork.ID)
		if err != nil {
			t.Fatalf("GetFork: %v", err)
		}
		if current.State != domain.ForkAbandoned {
			t.Errorf("fork %s is %q, want abandoned", current.Name, current.State)
		}
	}
	// Cancelling twice is a no-op, not an error.
	if err := f.orch.Cancel(f.ctx, task.ID, "again"); err != nil {
		t.Fatalf("Cancel (repeat): %v", err)
	}
}

func TestDiscoverProjectsInfersLayoutWithoutAManifest(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"projects": []map[string]any{
		{"name": "web", "path": "apps/web", "toolchain": "node", "preview_command": "npm run dev", "preview_port": 5173},
		{"name": "api", "path": "./services/api/", "toolchain": "go", "preview_port": 8080},
		{"name": "duplicate", "path": "apps/web"},
	}})
	f := newFixture(t, llm.Response{Content: string(body), FinishReason: "stop"})

	projects, err := f.orch.DiscoverProjects(f.ctx, f.repo.ID, []string{"apps/web/package.json", "services/api/go.mod"})
	if err != nil {
		t.Fatalf("DiscoverProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("discovered %d projects, want 2 after de-duplication", len(projects))
	}

	byPath := map[string]*domain.Project{}
	for _, p := range projects {
		byPath[p.Path] = p
		// Discovery proposes; the user confirms.
		if p.Confirmed {
			t.Errorf("project %s was marked confirmed by discovery", p.Name)
		}
	}
	if _, ok := byPath["services/api"]; !ok {
		t.Fatalf("paths were not normalised: %v", byPath)
	}
	if byPath["apps/web"].PreviewCommand != "npm run dev" {
		t.Fatalf("preview command lost: %+v", byPath["apps/web"])
	}
}

func TestDiscoverProjectsAlwaysYieldsAtLeastOne(t *testing.T) {
	f := newFixture(t, llm.Response{Content: `{"projects":[]}`, FinishReason: "stop"})
	projects, err := f.orch.DiscoverProjects(f.ctx, f.repo.ID, []string{"README.md"})
	if err != nil {
		t.Fatalf("DiscoverProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].Path != "." {
		t.Fatalf("expected a root-project fallback, got %+v", projects)
	}
}

func TestDiscoverProjectsRequiresATree(t *testing.T) {
	f := newFixture(t)
	if _, err := f.orch.DiscoverProjects(f.ctx, f.repo.ID, nil); err == nil {
		t.Error("discovery without a file listing should be rejected")
	}
}

func TestDiscoverIsIdempotentAcrossReruns(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"projects": []map[string]any{
		{"name": "web", "path": "apps/web", "toolchain": "node"},
	}})
	f := newFixture(t, llm.Response{Content: string(body), FinishReason: "stop"})

	for range 3 {
		if _, err := f.orch.DiscoverProjects(f.ctx, f.repo.ID, []string{"apps/web/package.json"}); err != nil {
			t.Fatalf("DiscoverProjects: %v", err)
		}
	}
	projects, err := f.store.ListProjects(f.ctx, f.repo.ID)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	// Understanding of a repo improves over time, so discovery reruns; it must
	// refine rather than duplicate.
	if len(projects) != 1 {
		t.Fatalf("rerunning discovery produced %d projects, want 1", len(projects))
	}
}

func TestConfirmProjectsMarksTheRepoDiscovered(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"projects": []map[string]any{
		{"name": "web", "path": "apps/web"},
		{"name": "scratch", "path": "tmp/scratch"},
	}})
	f := newFixture(t, llm.Response{Content: string(body), FinishReason: "stop"})
	if _, err := f.orch.DiscoverProjects(f.ctx, f.repo.ID, []string{"apps/web/package.json"}); err != nil {
		t.Fatalf("DiscoverProjects: %v", err)
	}

	confirmed, err := f.orch.ConfirmProjects(f.ctx, f.repo.ID, []string{"apps/web"})
	if err != nil {
		t.Fatalf("ConfirmProjects: %v", err)
	}
	if len(confirmed) != 1 || confirmed[0].Path != "apps/web" {
		t.Fatalf("confirmed = %+v", confirmed)
	}

	repo, err := f.store.GetRepo(f.ctx, f.repo.ID)
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	// Discovery runs once per repo, so the repo has to remember it happened.
	if !repo.Discovered {
		t.Fatal("the repo should be marked discovered after confirmation")
	}
}

func TestPlanningPropagatesModelFailures(t *testing.T) {
	f := newFixture(t)
	f.model.Err = errors.New("provider exploded")
	task := f.newTask(t)
	if _, err := f.orch.Plan(f.ctx, task.ID); err == nil {
		t.Fatal("a model failure should surface")
	}
}

func TestEventsRecordTheTaskLifecycle(t *testing.T) {
	f := newFixture(t, planJSON("one", nil, threeWorkstreams()[:1]))
	task := f.newTask(t)
	if _, err := f.orch.Plan(f.ctx, task.ID); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, _, err := f.orch.ApprovePlan(f.ctx, task.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}

	events, err := f.store.ListEvents(f.ctx, store.EventFilter{TaskID: task.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	seen := map[domain.EventType]bool{}
	for _, e := range events {
		seen[e.Type] = true
	}
	for _, want := range []domain.EventType{domain.EventTaskCreated, domain.EventTaskPlanned, domain.EventForkCreated, domain.EventTaskStateChange} {
		if !seen[want] {
			t.Errorf("no %q event was recorded", want)
		}
	}
}

func TestSummarize(t *testing.T) {
	if got := summarize("short request", 72); got != "short request" {
		t.Fatalf("summarize = %q", got)
	}
	long := strings.Repeat("word ", 40)
	got := summarize(long, 30)
	if len(got) > 34 || !strings.HasSuffix(got, "...") {
		t.Fatalf("summarize = %q (len %d)", got, len(got))
	}
}

func TestShortIDAndBranchNaming(t *testing.T) {
	f := newFixture(t)
	branch := f.orch.branchName("Photo Upload", "fork_0004k2m9x8q3v7bntp2wz1cgae")
	if !strings.HasPrefix(branch, "dabberz/photo-upload-") {
		t.Fatalf("branch = %q", branch)
	}
	if f.orch.branchName("!!!", "fork_abc") == "" {
		t.Fatal("an unnameable workstream should still get a branch")
	}
}

func names(forks []*domain.Fork) []string {
	out := make([]string, 0, len(forks))
	for _, f := range forks {
		out = append(out, f.Name)
	}
	return out
}

var _ = time.Now
