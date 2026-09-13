// Package orchestrator owns the lifecycle of tasks and forks.
//
// It sits outside the coding agent's reasoning loop: a separate, pluggable
// model decomposes a request into workstreams, judges at fork time which of
// them would collide, and decides what to spawn. Everything that needs a
// decision about *which* work happens lives here; everything about *how* the
// work happens lives in the coding agent.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
	"github.com/dabbers/devex/internal/llm"
	"github.com/dabbers/devex/internal/memory"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/tripwire"
)

// Notifier is told when work becomes runnable, so the scheduler can make an
// admission pass without waiting for its next poll.
type Notifier interface {
	Nudge()
}

// Options configures an Orchestrator.
type Options struct {
	// Tripwire holds the global escalation thresholds.
	Tripwire tripwire.Thresholds
	// BranchPrefix prefixes every fork's git branch.
	BranchPrefix string
	Logger       *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.BranchPrefix == "" {
		o.BranchPrefix = "dabberz"
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if (o.Tripwire == tripwire.Thresholds{}) {
		o.Tripwire = tripwire.Defaults()
	}
	return o
}

// Orchestrator plans tasks and manages their forks.
type Orchestrator struct {
	store    *store.Store
	model    llm.Client
	memory   *memory.Store
	notifier Notifier
	opts     Options
}

// New returns an orchestrator.
func New(st *store.Store, model llm.Client, mem *memory.Store, notifier Notifier, opts Options) *Orchestrator {
	return &Orchestrator{
		store:    st,
		model:    model,
		memory:   mem,
		notifier: notifier,
		opts:     opts.withDefaults(),
	}
}

// Thresholds reports the global tripwire settings.
func (o *Orchestrator) Thresholds() tripwire.Thresholds { return o.opts.Tripwire }

// CreateTaskRequest is a new user request against a repo.
type CreateTaskRequest struct {
	UserID string
	RepoID string
	Title  string
	// Request is the user's text, kept verbatim.
	Request string
	// MergeTarget and MergeTiming are per-task user preferences. They are
	// never assumed: an unset value falls back to the conservative default
	// rather than to whatever the last task used.
	MergeTarget       domain.MergeTarget
	MergeTiming       domain.MergeTiming
	IntegrationBranch string
}

// CreateTask records a request. No VMs are started: the task must go through
// the planning checkpoint first.
func (o *Orchestrator) CreateTask(ctx context.Context, req CreateTaskRequest) (*domain.Task, error) {
	if strings.TrimSpace(req.Request) == "" {
		return nil, errors.New("orchestrator: a task needs a request")
	}
	repo, err := o.store.GetRepo(ctx, req.RepoID)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: load repo: %w", err)
	}
	if req.UserID == "" {
		req.UserID = repo.UserID
	}

	if req.MergeTarget == "" {
		req.MergeTarget = domain.MergeTargetDefaultBranch
	}
	if !req.MergeTarget.Valid() {
		return nil, fmt.Errorf("orchestrator: unknown merge target %q", req.MergeTarget)
	}
	if req.MergeTiming == "" {
		req.MergeTiming = domain.MergeTimingImmediate
	}
	if !req.MergeTiming.Valid() {
		return nil, fmt.Errorf("orchestrator: unknown merge timing %q", req.MergeTiming)
	}

	title := req.Title
	if title == "" {
		title = summarize(req.Request, 72)
	}

	task := &domain.Task{
		UserID:            req.UserID,
		RepoID:            req.RepoID,
		Title:             title,
		Request:           req.Request,
		State:             domain.TaskDraft,
		MergeTarget:       req.MergeTarget,
		MergeTiming:       req.MergeTiming,
		IntegrationBranch: req.IntegrationBranch,
	}
	if task.MergeTarget == domain.MergeTargetIntegrationBranch && task.IntegrationBranch == "" {
		task.IntegrationBranch = o.opts.BranchPrefix + "/" + shortID(task.ID) + "-integration"
	}

	if err := o.store.CreateTask(ctx, task); err != nil {
		return nil, err
	}
	// The integration branch name needs the task id, which only exists after
	// the insert.
	if task.MergeTarget == domain.MergeTargetIntegrationBranch && strings.Contains(task.IntegrationBranch, "-integration") {
		task.IntegrationBranch = o.opts.BranchPrefix + "/" + shortID(task.ID) + "-integration"
		if err := o.store.UpdateTask(ctx, task); err != nil {
			return nil, err
		}
	}

	// A task exists because the user asked for it, so it is attributed to them
	// even though the orchestrator is the component writing the row.
	o.event(ctx, &domain.Event{
		UserID: task.UserID, RepoID: task.RepoID, TaskID: task.ID,
		Actor: domain.ActorUser,
		Type:  domain.EventTaskCreated, Message: "task created: " + task.Title,
	})
	return task, nil
}

// planResponse is the JSON shape the planning model is asked for.
type planResponse struct {
	Summary   string `json:"summary"`
	Questions []struct {
		Text    string   `json:"text"`
		Options []string `json:"options"`
	} `json:"questions"`
	Workstreams []struct {
		Name             string `json:"name"`
		Description      string `json:"description"`
		ProjectPath      string `json:"project_path"`
		SerializeGroup   string `json:"serialize_group"`
		OverlapRationale string `json:"overlap_rationale"`
	} `json:"workstreams"`
}

// Plan decomposes a task into workstreams and parks it at the planning
// checkpoint. It can run repeatedly: answering questions produces a new round
// that refines the previous plan.
func (o *Orchestrator) Plan(ctx context.Context, taskID string) (*domain.Task, error) {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.State != domain.TaskDraft && task.State != domain.TaskAwaitingPlan {
		return nil, fmt.Errorf("orchestrator: task %s is %s and cannot be planned", task.ID, task.State)
	}

	if err := o.transitionTask(ctx, task, domain.TaskPlanning, ""); err != nil {
		return nil, err
	}

	plan, err := o.buildPlan(ctx, task)
	if err != nil {
		// Planning failed outright; the task is not left stuck mid-transition.
		if txErr := o.transitionTask(ctx, task, domain.TaskFailed, err.Error()); txErr != nil {
			o.opts.Logger.Error("could not record planning failure", "task", task.ID, "error", txErr)
		}
		return nil, err
	}

	task.Plan = plan
	if err := o.transitionTask(ctx, task, domain.TaskAwaitingPlan, ""); err != nil {
		return nil, err
	}

	o.event(ctx, &domain.Event{
		UserID: task.UserID, RepoID: task.RepoID, TaskID: task.ID,
		Actor: domain.ActorOrchestrator, Type: domain.EventTaskPlanned,
		Message: fmt.Sprintf("planned %d workstream(s)", len(plan.Workstreams)),
		Data: map[string]any{
			"round":       plan.Round,
			"workstreams": len(plan.Workstreams),
			"questions":   len(plan.Questions),
		},
	})
	return task, nil
}

// buildPlan runs one planning round against the model.
func (o *Orchestrator) buildPlan(ctx context.Context, task *domain.Task) (*domain.Plan, error) {
	repo, err := o.store.GetRepo(ctx, task.RepoID)
	if err != nil {
		return nil, err
	}
	projects, err := o.store.ListProjects(ctx, task.RepoID)
	if err != nil {
		return nil, err
	}

	var findings []*memory.Finding
	if o.memory != nil {
		if findings, err = o.memory.List(ctx, task.RepoID); err != nil {
			// Memory is an optimisation, not a prerequisite: planning without
			// it produces a worse plan, not a wrong one.
			o.opts.Logger.Warn("could not read repo memory", "repo", task.RepoID, "error", err)
			findings = nil
		}
	}

	round := 1
	var answered []domain.Question
	if task.Plan != nil {
		round = task.Plan.Round + 1
		for _, q := range task.Plan.Questions {
			if q.Answer != "" {
				answered = append(answered, q)
			}
		}
	}

	temperature := 0.2
	resp, err := o.model.Complete(ctx, llm.Request{
		Messages: []llm.Message{
			llm.System(planSystemPrompt),
			llm.User(planUserPrompt(task, repo, projects, findings, answered)),
		},
		Temperature: &temperature,
		JSON:        true,
	})
	if err != nil {
		return nil, fmt.Errorf("orchestrator: planning call failed: %w", err)
	}
	if resp.Truncated() {
		return nil, errors.New("orchestrator: the planning response was cut off at the token limit")
	}

	var decoded planResponse
	if err := llm.DecodeJSON(resp.Content, &decoded); err != nil {
		return nil, fmt.Errorf("orchestrator: planning response: %w", err)
	}

	plan := &domain.Plan{
		Summary:   strings.TrimSpace(decoded.Summary),
		Round:     round,
		CreatedAt: time.Now().UTC(),
	}
	for _, q := range decoded.Questions {
		if strings.TrimSpace(q.Text) == "" {
			continue
		}
		plan.Questions = append(plan.Questions, domain.Question{
			ID:      id.New(id.Question),
			Text:    strings.TrimSpace(q.Text),
			Options: q.Options,
		})
	}
	for _, w := range decoded.Workstreams {
		name := strings.TrimSpace(w.Name)
		if name == "" {
			continue
		}
		path := strings.TrimSpace(w.ProjectPath)
		if path == "" {
			path = "."
		}
		plan.Workstreams = append(plan.Workstreams, domain.PlannedWorkstream{
			Name:             name,
			Description:      strings.TrimSpace(w.Description),
			ProjectPath:      path,
			SerializeGroup:   strings.TrimSpace(w.SerializeGroup),
			OverlapRationale: strings.TrimSpace(w.OverlapRationale),
		})
	}

	if len(plan.Workstreams) == 0 && len(plan.Questions) == 0 {
		// A plan with neither work to do nor anything to ask is not a plan the
		// user can act on.
		return nil, errors.New("orchestrator: the planner proposed no workstreams and asked no questions")
	}
	return plan, nil
}

// AnswerQuestions records answers to the current plan's questions and replans.
// This is the back-and-forth half of the planning checkpoint.
func (o *Orchestrator) AnswerQuestions(ctx context.Context, taskID string, answers map[string]string) (*domain.Task, error) {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.State != domain.TaskAwaitingPlan {
		return nil, fmt.Errorf("orchestrator: task %s is %s and has no open questions", task.ID, task.State)
	}
	if task.Plan == nil {
		return nil, fmt.Errorf("orchestrator: task %s has no plan", task.ID)
	}

	now := time.Now().UTC()
	matched := 0
	for i := range task.Plan.Questions {
		q := &task.Plan.Questions[i]
		answer, ok := answers[q.ID]
		if !ok || strings.TrimSpace(answer) == "" {
			continue
		}
		q.Answer = strings.TrimSpace(answer)
		q.AnswerAt = &now
		matched++
	}
	if matched == 0 {
		return nil, errors.New("orchestrator: none of the supplied answers matched an open question")
	}
	if err := o.store.UpdateTask(ctx, task); err != nil {
		return nil, err
	}
	o.event(ctx, &domain.Event{
		UserID: task.UserID, RepoID: task.RepoID, TaskID: task.ID,
		Actor: domain.ActorUser, Type: domain.EventPlanAnswered,
		Message: fmt.Sprintf("answered %d planning question(s)", matched),
		Data:    map[string]any{"answered": matched, "round": task.Plan.Round},
	})

	// Replanning with the answers in hand is the point of asking.
	return o.Plan(ctx, taskID)
}

// ApprovePlan accepts the current plan and queues a fork per workstream.
//
// This is the only path that creates VMs, and it is gated on the user: nothing
// spins up before the plan has been seen and approved.
func (o *Orchestrator) ApprovePlan(ctx context.Context, taskID string) (*domain.Task, []*domain.Fork, error) {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	if task.State != domain.TaskAwaitingPlan {
		return nil, nil, fmt.Errorf("orchestrator: task %s is %s and has no plan awaiting approval", task.ID, task.State)
	}
	if task.Plan == nil || len(task.Plan.Workstreams) == 0 {
		return nil, nil, fmt.Errorf("orchestrator: task %s has no workstreams to start", task.ID)
	}
	if task.Plan.NeedsInput() {
		return nil, nil, fmt.Errorf("orchestrator: task %s still has unanswered questions", task.ID)
	}

	forks := make([]*domain.Fork, 0, len(task.Plan.Workstreams))
	for _, workstream := range task.Plan.Workstreams {
		project, err := o.resolveProject(ctx, task.RepoID, workstream.ProjectPath)
		if err != nil {
			return nil, nil, err
		}

		fork := &domain.Fork{
			TaskID:         task.ID,
			UserID:         task.UserID,
			RepoID:         task.RepoID,
			ProjectID:      project.ID,
			Name:           workstream.Name,
			Description:    workstream.Description,
			SerializeGroup: workstream.SerializeGroup,
			State:          domain.ForkQueued,
		}
		if err := o.store.CreateFork(ctx, fork); err != nil {
			return nil, nil, err
		}

		// The branch name needs the fork id to stay unique across tasks that
		// happen to name a workstream the same way.
		fork.Branch = o.branchName(workstream.Name, fork.ID)
		if err := o.store.UpdateFork(ctx, fork); err != nil {
			return nil, nil, err
		}

		o.event(ctx, &domain.Event{
			UserID: task.UserID, RepoID: task.RepoID, TaskID: task.ID, ForkID: fork.ID,
			Actor:   domain.ActorOrchestrator,
			Type:    domain.EventForkCreated,
			Message: "queued workstream " + fork.Name,
			Data: map[string]any{
				"branch":            fork.Branch,
				"serialize_group":   fork.SerializeGroup,
				"overlap_rationale": workstream.OverlapRationale,
			},
		})
		forks = append(forks, fork)
	}

	// Approval is the moment the user authorises real machines to start, so it
	// is recorded as its own action rather than inferred from a state change.
	o.event(ctx, &domain.Event{
		UserID: task.UserID, RepoID: task.RepoID, TaskID: task.ID,
		Actor: domain.ActorUser, Type: domain.EventPlanApproved,
		Message: fmt.Sprintf("approved the plan; starting %d workstream(s)", len(forks)),
		Data: map[string]any{
			"workstreams":  len(forks),
			"round":        task.Plan.Round,
			"merge_target": string(task.MergeTarget),
			"merge_timing": string(task.MergeTiming),
		},
	})

	if err := o.transitionTask(ctx, task, domain.TaskRunning, ""); err != nil {
		return nil, nil, err
	}
	// Let the scheduler admit whatever fits right away rather than waiting for
	// its next poll.
	if o.notifier != nil {
		o.notifier.Nudge()
	}
	return task, forks, nil
}

// resolveProject finds the project a workstream targets, creating a record for
// an undiscovered path rather than refusing to start.
func (o *Orchestrator) resolveProject(ctx context.Context, repoID, path string) (*domain.Project, error) {
	if path == "" {
		path = "."
	}
	project, err := o.store.GetProjectByPath(ctx, repoID, path)
	if err == nil {
		return project, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	project = &domain.Project{
		RepoID: repoID,
		Name:   projectNameFromPath(path),
		Path:   path,
	}
	if err := o.store.CreateProject(ctx, project); err != nil {
		return nil, err
	}
	return project, nil
}

// Cancel stops a task and abandons any fork that has not finished. VMs are
// left in place: nothing is reclaimed automatically.
func (o *Orchestrator) Cancel(ctx context.Context, taskID, reason string) error {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.State.Terminal() {
		return nil
	}

	forks, err := o.store.ListForks(ctx, store.ForkFilter{TaskID: taskID})
	if err != nil {
		return err
	}
	abandoned := 0
	for _, fork := range forks {
		if fork.State.Terminal() {
			continue
		}
		abandoned++
		if err := o.store.TransitionFork(ctx, fork, domain.ForkAbandoned, reason); err != nil {
			// One fork in an awkward state must not block cancelling the rest.
			o.opts.Logger.Warn("could not abandon fork during cancellation",
				"fork", fork.ID, "state", fork.State, "error", err)
		}
	}
	o.event(ctx, &domain.Event{
		UserID: task.UserID, RepoID: task.RepoID, TaskID: task.ID,
		Actor: domain.ActorUser, Type: domain.EventTaskCancelled,
		Message: "task cancelled by the user",
		Data:    map[string]any{"reason": reason, "forks_abandoned": abandoned},
	})
	return o.transitionTask(ctx, task, domain.TaskCancelled, reason)
}

// Escalate pauses a fork and asks the user for direction.
//
// Escalation is per-fork: siblings in the same task keep running, so one stuck
// workstream does not stall a batch.
func (o *Orchestrator) Escalate(ctx context.Context, fork *domain.Fork, kind domain.EscalationKind, message, detail string) error {
	fork.Escalation = &domain.Escalation{
		Kind:     kind,
		Message:  message,
		Detail:   detail,
		RaisedAt: time.Now().UTC(),
	}
	if err := o.store.TransitionFork(ctx, fork, domain.ForkEscalated, message); err != nil {
		return err
	}
	o.event(ctx, &domain.Event{
		UserID: fork.UserID, RepoID: fork.RepoID, TaskID: fork.TaskID, ForkID: fork.ID,
		Actor:   domain.ActorOrchestrator,
		Type:    domain.EventForkEscalated,
		Message: message,
		Data:    map[string]any{"kind": string(kind), "detail": detail},
	})
	return nil
}

// CheckTripwire escalates a fork if it has crossed a global threshold.
// It reports whether the fork was escalated.
func (o *Orchestrator) CheckTripwire(ctx context.Context, fork *domain.Fork) (bool, error) {
	breach := o.opts.Tripwire.Check(fork.Usage)
	if breach == nil {
		return false, nil
	}
	detail := fmt.Sprintf("%s limit %s, reached %s", breach.Kind, breach.Limit, breach.Actual)
	if err := o.Escalate(ctx, fork, domain.EscalationTripwire, breach.Message, detail); err != nil {
		return false, err
	}
	return true, nil
}

// ResolveEscalation records the user's answer and returns the fork to work.
func (o *Orchestrator) ResolveEscalation(ctx context.Context, forkID, response string, resume domain.ForkState) (*domain.Fork, error) {
	fork, err := o.store.GetFork(ctx, forkID)
	if err != nil {
		return nil, err
	}
	if fork.State != domain.ForkEscalated {
		return nil, fmt.Errorf("orchestrator: fork %s is %s, not escalated", fork.ID, fork.State)
	}
	if resume == "" {
		resume = domain.ForkCoding
	}

	now := time.Now().UTC()
	if fork.Escalation != nil {
		fork.Escalation.Response = response
		fork.Escalation.ResolvedAt = &now
	}
	// A tripwire breach is what paused the fork, so resuming without clearing
	// the counter would trip again immediately on the next check.
	if fork.Escalation != nil && fork.Escalation.Kind == domain.EscalationTripwire {
		fork.Usage.Cycles = 0
	}

	if err := o.store.TransitionFork(ctx, fork, resume, "resumed by user"); err != nil {
		return nil, err
	}
	o.event(ctx, &domain.Event{
		UserID: fork.UserID, RepoID: fork.RepoID, TaskID: fork.TaskID, ForkID: fork.ID,
		Actor: domain.ActorUser, Type: domain.EventEscalationResolved,
		Message: "escalation answered; fork resumed",
		Data:    map[string]any{"response": response, "resumed_to": string(resume)},
	})
	return fork, nil
}

// ReadyToMerge returns the forks of a task whose merge window has arrived.
//
// Merge timing is a per-task user preference: under immediate timing a fork
// merges as soon as it verifies, while under batch timing every fork waits
// until no sibling is still working.
func (o *Orchestrator) ReadyToMerge(ctx context.Context, taskID string) ([]*domain.Fork, error) {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	forks, err := o.store.ListForks(ctx, store.ForkFilter{TaskID: taskID})
	if err != nil {
		return nil, err
	}

	var waiting []*domain.Fork
	stillWorking := false
	for _, fork := range forks {
		switch {
		case fork.State == domain.ForkAwaitingMerge:
			waiting = append(waiting, fork)
		case !fork.State.Terminal() && fork.State != domain.ForkEscalated:
			stillWorking = true
		}
	}

	if task.MergeTiming == domain.MergeTimingBatch && stillWorking {
		// The batch is not complete, so nothing lands yet.
		return nil, nil
	}
	return waiting, nil
}

// Reconcile completes a task once none of its forks can make further progress.
func (o *Orchestrator) Reconcile(ctx context.Context, taskID string) (*domain.Task, error) {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.State != domain.TaskRunning {
		return task, nil
	}

	forks, err := o.store.ListForks(ctx, store.ForkFilter{TaskID: taskID})
	if err != nil {
		return nil, err
	}
	if len(forks) == 0 {
		return task, nil
	}

	var merged, failed, escalated int
	for _, fork := range forks {
		switch {
		case fork.State == domain.ForkMerged:
			merged++
		case fork.State == domain.ForkFailed:
			failed++
		case fork.State == domain.ForkEscalated:
			// An escalated fork is waiting on the user, so the task is not
			// finished even though nothing is running.
			escalated++
		case !fork.State.Terminal():
			return task, nil
		}
	}
	if escalated > 0 {
		return task, nil
	}

	next := domain.TaskCompleted
	reason := fmt.Sprintf("%d of %d workstream(s) merged", merged, len(forks))
	if merged == 0 && failed > 0 {
		next = domain.TaskFailed
		reason = "every workstream failed"
	}
	if err := o.transitionTask(ctx, task, next, reason); err != nil {
		return nil, err
	}
	return task, nil
}

// transitionTask moves a task and records the change on the activity feed.
func (o *Orchestrator) transitionTask(ctx context.Context, task *domain.Task, next domain.TaskState, reason string) error {
	from := task.State
	if err := domain.TransitionTask(task, next); err != nil {
		return err
	}
	task.StateReason = reason
	if err := o.store.UpdateTask(ctx, task); err != nil {
		return err
	}
	if from == next {
		return nil
	}
	o.event(ctx, &domain.Event{
		UserID: task.UserID, RepoID: task.RepoID, TaskID: task.ID,
		Actor: domain.ActorOrchestrator, Type: domain.EventTaskStateChange,
		Message: fmt.Sprintf("task %s -> %s", from, next),
		Data:    map[string]any{"from": string(from), "to": string(next), "reason": reason},
	})
	return nil
}

// event records an activity-feed entry, logging rather than failing: losing a
// feed entry must never take down the work it describes.
func (o *Orchestrator) event(ctx context.Context, e *domain.Event) {
	if err := o.store.AppendEvent(ctx, e); err != nil {
		o.opts.Logger.Error("could not record event", "type", e.Type, "error", err)
	}
}

// branchName builds a unique branch for a workstream.
func (o *Orchestrator) branchName(workstream, forkID string) string {
	slug := memory.Slug(workstream)
	if slug == "" {
		slug = "workstream"
	}
	return fmt.Sprintf("%s/%s-%s", o.opts.BranchPrefix, slug, shortID(forkID))
}

// shortID returns the trailing, random part of an id.
func shortID(v string) string {
	_, rest, ok := strings.Cut(v, "_")
	if !ok {
		rest = v
	}
	if len(rest) <= 6 {
		return rest
	}
	return rest[len(rest)-6:]
}

// projectNameFromPath derives a readable name for an undiscovered project.
func projectNameFromPath(path string) string {
	if path == "." || path == "" {
		return "root"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return parts[len(parts)-1]
}

// summarize truncates text to a title-length summary on a word boundary.
func summarize(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	clipped := text[:limit]
	if idx := strings.LastIndex(clipped, " "); idx > limit/2 {
		clipped = clipped[:idx]
	}
	return clipped + "..."
}
