// Package domain defines the entities dabberz orchestrates and the rules
// governing how they move between states.
//
// The hierarchy is repo -> project -> task -> fork. A repo is the root of the
// secrets and memory hierarchy; a project is one buildable unit discovered
// inside it (a repo may hold several); a task is one user request against a
// repo; and a fork is a single independently parallelizable workstream within
// that task, owning exactly one VM and one preview URL.
package domain

import (
	"time"
)

// User owns repos, tasks and forks. v1 runs single-user, but every entity is
// scoped by user id from the start so multi-user support is additive.
type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// Repo is a git repository under dabberz management and the root of the
// secrets, memory and configuration hierarchy.
type Repo struct {
	ID            string `json:"id"`
	UserID        string `json:"user_id"`
	Name          string `json:"name"`
	RemoteURL     string `json:"remote_url"`
	DefaultBranch string `json:"default_branch"`
	// Discovered records whether the monorepo discovery pass has run and been
	// confirmed by the user. Discovery is an LLM pass over the repo layout, so
	// it runs once per repo rather than once per task.
	Discovered bool      `json:"discovered"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Project is one distinct buildable unit inside a repo. Monorepos yield
// several; a single-project repo yields exactly one rooted at ".".
type Project struct {
	ID     string `json:"id"`
	RepoID string `json:"repo_id"`
	Name   string `json:"name"`
	// Path is the project root relative to the repo root, "." for the repo itself.
	Path string `json:"path"`
	// Toolchain is the primary toolchain inferred during discovery ("go",
	// "node", "rust", ...). It selects build and preview commands, not the VM
	// image: every VM boots the same generic golden image.
	Toolchain string `json:"toolchain"`
	// PreviewCommand starts the project's dev server inside its fork VM.
	PreviewCommand string `json:"preview_command"`
	// PreviewPort is the in-VM port the dev server listens on.
	PreviewPort int       `json:"preview_port"`
	Confirmed   bool      `json:"confirmed"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// MergeTarget selects where a finished fork lands. It is a per-task user
// preference and is never assumed.
type MergeTarget string

const (
	// MergeTargetDefaultBranch lands each fork on the repo's default branch.
	MergeTargetDefaultBranch MergeTarget = "default_branch"
	// MergeTargetIntegrationBranch lands forks on a shared branch owned by the
	// task, leaving the default branch untouched until the user merges it.
	MergeTargetIntegrationBranch MergeTarget = "integration_branch"
)

// Valid reports whether the merge target is one dabberz understands.
func (m MergeTarget) Valid() bool {
	return m == MergeTargetDefaultBranch || m == MergeTargetIntegrationBranch
}

// MergeTiming selects when a finished fork lands. Also a per-task preference.
type MergeTiming string

const (
	// MergeTimingImmediate merges each fork as soon as it verifies.
	MergeTimingImmediate MergeTiming = "immediate"
	// MergeTimingBatch holds every verified fork until all siblings in the task
	// have finished, then merges them together.
	MergeTimingBatch MergeTiming = "batch"
)

// Valid reports whether the merge timing is one dabberz understands.
func (m MergeTiming) Valid() bool {
	return m == MergeTimingImmediate || m == MergeTimingBatch
}

// Task is one user request against a repo, decomposed by the orchestrator into
// one or more forks.
type Task struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	RepoID string `json:"repo_id"`
	Title  string `json:"title"`
	// Request is the user's original text, kept verbatim so replanning after a
	// clarification round starts from the source rather than a summary.
	Request     string      `json:"request"`
	State       TaskState   `json:"state"`
	MergeTarget MergeTarget `json:"merge_target"`
	MergeTiming MergeTiming `json:"merge_timing"`
	// IntegrationBranch is the branch forks land on when MergeTarget is
	// MergeTargetIntegrationBranch.
	IntegrationBranch string `json:"integration_branch,omitempty"`
	// Plan is the orchestrator's current decomposition, replaced wholesale on
	// each planning round.
	Plan *Plan `json:"plan,omitempty"`
	// StateReason explains a terminal or blocked state to the user.
	StateReason string    `json:"state_reason,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Plan is the orchestrator's decomposition of a task into forkable
// workstreams, together with any questions it needs answered before it will
// spin up VMs.
type Plan struct {
	// Summary is the orchestrator's reading of the request, shown at the
	// planning checkpoint.
	Summary string `json:"summary"`
	// Workstreams are the proposed forks, in the order the orchestrator would
	// start them.
	Workstreams []PlannedWorkstream `json:"workstreams"`
	// Questions must be answered by the user before the plan can be approved.
	// An empty list means the orchestrator is ready to proceed.
	Questions []Question `json:"questions,omitempty"`
	// Round counts the planning iterations so far, starting at 1.
	Round     int       `json:"round"`
	CreatedAt time.Time `json:"created_at"`
}

// NeedsInput reports whether the plan is blocked on unanswered questions.
func (p *Plan) NeedsInput() bool {
	if p == nil {
		return false
	}
	for i := range p.Questions {
		if p.Questions[i].Answer == "" {
			return true
		}
	}
	return false
}

// Question is a clarifying question raised at the planning checkpoint.
type Question struct {
	ID       string     `json:"id"`
	Text     string     `json:"text"`
	Options  []string   `json:"options,omitempty"`
	Answer   string     `json:"answer,omitempty"`
	AnswerAt *time.Time `json:"answered_at,omitempty"`
}

// PlannedWorkstream is a proposed fork, before any VM exists.
type PlannedWorkstream struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// ProjectPath identifies the project within the repo this workstream
	// targets, matching Project.Path.
	ProjectPath string `json:"project_path"`
	// SerializeGroup holds workstreams the orchestrator judged likely to
	// collide. Workstreams sharing a non-empty group run one at a time, in plan
	// order; an empty group means the workstream runs in parallel with
	// everything else. Overlap is judged once, at fork time.
	SerializeGroup string `json:"serialize_group,omitempty"`
	// OverlapRationale records why the orchestrator grouped this workstream as
	// it did, so the judgement is auditable at the planning checkpoint.
	OverlapRationale string `json:"overlap_rationale,omitempty"`
}

// Fork is one independent workstream: its own VM, branch and preview URL, with
// no mutable state shared with its siblings.
type Fork struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	UserID    string `json:"user_id"`
	RepoID    string `json:"repo_id"`
	ProjectID string `json:"project_id"`

	Name        string `json:"name"`
	Description string `json:"description"`
	Branch      string `json:"branch"`

	State       ForkState `json:"state"`
	StateReason string    `json:"state_reason,omitempty"`

	// SerializeGroup mirrors PlannedWorkstream.SerializeGroup; the scheduler
	// admits at most one fork per group per task at a time.
	SerializeGroup string `json:"serialize_group,omitempty"`

	// InstanceID is the VM backing this fork. VMs are never reclaimed
	// automatically, so this stays populated after a merge or abandonment.
	InstanceID string `json:"instance_id,omitempty"`
	PreviewURL string `json:"preview_url,omitempty"`

	Usage Usage `json:"usage"`

	// Escalation is set when a tripwire fired or the agent needed the user.
	Escalation *Escalation `json:"escalation,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Active reports whether the fork still occupies scheduler capacity.
func (f *Fork) Active() bool { return f.State.Active() }

// Usage accumulates what a fork has spent, and is checked against the global
// tripwire thresholds after every verify/fix cycle.
type Usage struct {
	// Cycles counts completed verify/fix rounds.
	Cycles       int     `json:"cycles"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	// Wall is the wall-clock time the fork has been running, excluding time
	// spent queued or escalated.
	Wall time.Duration `json:"wall_ns"`
}

// Add merges another usage record into u.
func (u *Usage) Add(other Usage) {
	u.Cycles += other.Cycles
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CostUSD += other.CostUSD
	u.Wall += other.Wall
}

// EscalationKind classifies why a fork stopped and asked for the user.
type EscalationKind string

const (
	// EscalationConflictsInstructions means the only fix the agent could find
	// contradicts an instruction it was given.
	EscalationConflictsInstructions EscalationKind = "conflicts_instructions"
	// EscalationAmbiguous means the situation admits no clear direction.
	EscalationAmbiguous EscalationKind = "ambiguous"
	// EscalationTripwire means a global budget threshold was exceeded.
	EscalationTripwire EscalationKind = "tripwire"
	// EscalationRateLimited means the coding agent hit an upstream rate limit.
	EscalationRateLimited EscalationKind = "rate_limited"
)

// Escalation is a request for user input on a single fork. Escalation scope is
// per-fork: siblings in the same task keep running.
type Escalation struct {
	Kind    EscalationKind `json:"kind"`
	Message string         `json:"message"`
	// Detail carries the supporting context the user needs to decide, such as
	// the failing verifier report or the tripwire that fired.
	Detail     string     `json:"detail,omitempty"`
	RaisedAt   time.Time  `json:"raised_at"`
	Response   string     `json:"response,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// Actor names who or what performed an action.
//
// The split that matters for auditing is user versus system: everything the
// user did through the control plane is attributed to them, and everything a
// component did on its own is attributed to that component, so "what did I
// approve" and "what did the agents do unattended" are both answerable.
type Actor string

// Actors.
const (
	// ActorUser is an action the user took through the control plane.
	ActorUser Actor = "user"
	// ActorOrchestrator is the planning and lifecycle layer.
	ActorOrchestrator Actor = "orchestrator"
	// ActorScheduler is fork admission.
	ActorScheduler Actor = "scheduler"
	// ActorPipeline is the per-fork runner.
	ActorPipeline Actor = "pipeline"
	// ActorAgent is the coding agent inside a fork VM.
	ActorAgent Actor = "agent"
	// ActorVerifier is the browser verification pass.
	ActorVerifier Actor = "verifier"
	// ActorReviewer is the merge and review gate.
	ActorReviewer Actor = "reviewer"
)

// Automated reports whether the actor acted without the user asking. This is
// what distinguishes the unattended half of the audit trail.
func (a Actor) Automated() bool { return a != "" && a != ActorUser }

// EventType names something worth recording on the activity feed.
type EventType string

// Event types recorded against tasks and forks.
const (
	EventTaskCreated     EventType = "task.created"
	EventTaskPlanned     EventType = "task.planned"
	EventTaskStateChange EventType = "task.state_changed"
	EventForkCreated     EventType = "fork.created"
	EventForkStateChange EventType = "fork.state_changed"
	EventForkQueued      EventType = "fork.queued"
	EventForkAdmitted    EventType = "fork.admitted"
	EventForkEscalated   EventType = "fork.escalated"
	EventForkMerged      EventType = "fork.merged"
	EventVerifyStarted   EventType = "verify.started"
	EventVerifyFinished  EventType = "verify.finished"
	EventAgentMessage    EventType = "agent.message"
	EventPreviewReady    EventType = "preview.ready"
	EventVMProvisioned   EventType = "vm.provisioned"
	EventError           EventType = "error"

	// User-initiated actions. These exist so the audit trail answers what a
	// person did, not only what the system did on its own.
	EventRepoCreated        EventType = "repo.created"
	EventProjectsDiscovered EventType = "repo.projects_discovered"
	EventProjectsConfirmed  EventType = "repo.projects_confirmed"
	EventPlanAnswered       EventType = "task.plan_answered"
	EventPlanApproved       EventType = "task.plan_approved"
	EventTaskCancelled      EventType = "task.cancelled"
	EventEscalationResolved EventType = "fork.escalation_resolved"
	// Secret events record the name only. A value never reaches the audit
	// trail, which is stored in the clear.
	EventSecretSet     EventType = "secret.set"
	EventSecretDeleted EventType = "secret.deleted"
	// EventShellOpened records a person attaching a terminal to a machine an
	// agent is working in, from where they can change anything the agent can.
	EventShellOpened EventType = "shell.opened"
)

// Event is an append-only record on a task or fork, powering both the audit
// trail and the live web feed.
type Event struct {
	ID string `json:"id"`
	// Seq is the feed's monotonic cursor, assigned on write. Clients resume a
	// stream by asking for events after the last Seq they saw.
	Seq    int64  `json:"seq"`
	UserID string `json:"user_id"`
	// RepoID scopes the event to a repo, which is what lets one feed cover
	// every repo at once and still be filterable down to one.
	RepoID string `json:"repo_id,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	ForkID string `json:"fork_id,omitempty"`
	// Actor is who performed the action.
	Actor Actor     `json:"actor,omitempty"`
	Type  EventType `json:"type"`
	// Message is a human-readable one-liner for the activity feed.
	Message string `json:"message"`
	// Data carries structured detail; its shape depends on Type.
	Data      map[string]any `json:"data,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}
