package domain

import "fmt"

// TaskState is where a task sits in the planning-to-completion lifecycle.
type TaskState string

// Task states. A task is created in TaskDraft, moves through a planning
// checkpoint the user must approve, and only then spins up VMs.
const (
	TaskDraft        TaskState = "draft"
	TaskPlanning     TaskState = "planning"
	TaskAwaitingPlan TaskState = "awaiting_plan"
	TaskRunning      TaskState = "running"
	TaskCompleted    TaskState = "completed"
	TaskFailed       TaskState = "failed"
	TaskCancelled    TaskState = "cancelled"
)

// taskTransitions is the complete set of legal task state changes.
var taskTransitions = map[TaskState][]TaskState{
	TaskDraft:    {TaskPlanning, TaskCancelled},
	TaskPlanning: {TaskAwaitingPlan, TaskFailed, TaskCancelled},
	// A plan awaiting approval can go back to planning when the user answers
	// clarifying questions, producing a fresh round.
	TaskAwaitingPlan: {TaskPlanning, TaskRunning, TaskCancelled},
	TaskRunning:      {TaskCompleted, TaskFailed, TaskCancelled},
	TaskCompleted:    nil,
	TaskFailed:       nil,
	TaskCancelled:    nil,
}

// Terminal reports whether the task has reached a state it cannot leave.
func (s TaskState) Terminal() bool { return len(taskTransitions[s]) == 0 }

// Valid reports whether s is a recognised task state.
func (s TaskState) Valid() bool {
	_, ok := taskTransitions[s]
	return ok
}

// CanTransition reports whether s may move directly to next.
func (s TaskState) CanTransition(next TaskState) bool {
	for _, allowed := range taskTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// String implements fmt.Stringer.
func (s TaskState) String() string { return string(s) }

// ForkState is where a single workstream sits in its lifecycle.
type ForkState string

// Fork states.
const (
	// ForkQueued means the fork is waiting on machine capacity or on a sibling
	// in its serialization group. Forks are never pre-empted, so a queued fork
	// waits until capacity frees up on its own.
	ForkQueued ForkState = "queued"
	// ForkProvisioning means its VM is booting and its preview route is being
	// published.
	ForkProvisioning ForkState = "provisioning"
	// ForkCoding means the coding agent is working.
	ForkCoding ForkState = "coding"
	// ForkVerifying means the verifier is driving a real browser against the
	// live preview.
	ForkVerifying ForkState = "verifying"
	// ForkFixing means verification failed and the report was fed back to the
	// coding agent. This is the automatic half of the verify/fix loop.
	ForkFixing ForkState = "fixing"
	// ForkAwaitingMerge means the fork verified and is waiting on its merge
	// window, which under batch timing is the completion of all siblings.
	ForkAwaitingMerge ForkState = "awaiting_merge"
	// ForkMerging means the merge/review agent is resolving conflicts and
	// running the quality gate.
	ForkMerging ForkState = "merging"
	// ForkMerged is the success terminal. The VM and preview URL persist.
	ForkMerged ForkState = "merged"
	// ForkEscalated means the fork is paused waiting on the user. Siblings keep
	// running: escalation scope is per-fork.
	ForkEscalated ForkState = "escalated"
	// ForkFailed means the fork stopped on an error the system could not route
	// back to the user as a question.
	ForkFailed ForkState = "failed"
	// ForkAbandoned is the user-initiated terminal. Like ForkMerged it leaves
	// the VM in place; cleanup is manual.
	ForkAbandoned ForkState = "abandoned"
)

// forkTransitions is the complete set of legal fork state changes.
var forkTransitions = map[ForkState][]ForkState{
	ForkQueued:       {ForkProvisioning, ForkAbandoned, ForkFailed},
	ForkProvisioning: {ForkCoding, ForkFailed, ForkEscalated, ForkAbandoned},
	ForkCoding:       {ForkVerifying, ForkEscalated, ForkFailed, ForkAbandoned},
	ForkVerifying:    {ForkAwaitingMerge, ForkFixing, ForkEscalated, ForkFailed, ForkAbandoned},
	// Fixing returns to verifying, closing the automatic loop. Every pass
	// through here increments the cycle counter the tripwire watches.
	ForkFixing:        {ForkVerifying, ForkEscalated, ForkFailed, ForkAbandoned},
	ForkAwaitingMerge: {ForkMerging, ForkEscalated, ForkAbandoned},
	// A merge review that requests changes sends the fork back to fixing: the
	// reworked change must be verified again before it is offered to the gate
	// a second time.
	ForkMerging: {ForkMerged, ForkFixing, ForkEscalated, ForkFailed},
	// A user answering an escalation resumes the fork at the stage that raised
	// it, so escalation can return to any working state.
	ForkEscalated: {ForkCoding, ForkFixing, ForkVerifying, ForkMerging, ForkAbandoned, ForkFailed},
	// A failed fork can be retried by hand without recreating it.
	ForkFailed:    {ForkCoding, ForkAbandoned},
	ForkMerged:    nil,
	ForkAbandoned: nil,
}

// activeForkStates are the states in which a fork holds VM capacity and counts
// against the scheduler's budget. A queued fork has no VM yet; terminal and
// escalated forks keep their VM (nothing is reclaimed automatically) but no
// longer consume scheduler slots, since they are not doing work.
var activeForkStates = map[ForkState]bool{
	ForkProvisioning:  true,
	ForkCoding:        true,
	ForkVerifying:     true,
	ForkFixing:        true,
	ForkAwaitingMerge: true,
	ForkMerging:       true,
}

// Terminal reports whether the fork has reached a state it cannot leave.
func (s ForkState) Terminal() bool { return len(forkTransitions[s]) == 0 }

// Active reports whether the fork is consuming scheduler capacity.
func (s ForkState) Active() bool { return activeForkStates[s] }

// Valid reports whether s is a recognised fork state.
func (s ForkState) Valid() bool {
	_, ok := forkTransitions[s]
	return ok
}

// CanTransition reports whether s may move directly to next.
func (s ForkState) CanTransition(next ForkState) bool {
	for _, allowed := range forkTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// String implements fmt.Stringer.
func (s ForkState) String() string { return string(s) }

// ErrInvalidTransition reports an attempt to move an entity between states the
// lifecycle does not connect.
type ErrInvalidTransition struct {
	Entity string
	From   string
	To     string
}

func (e *ErrInvalidTransition) Error() string {
	return fmt.Sprintf("%s: illegal state transition %s -> %s", e.Entity, e.From, e.To)
}

// TransitionTask moves a task to next, returning an error if the lifecycle
// does not allow it. Callers persist the task themselves.
func TransitionTask(t *Task, next TaskState) error {
	if !next.Valid() {
		return fmt.Errorf("task %s: unknown state %q", t.ID, next)
	}
	if t.State == next {
		return nil
	}
	if !t.State.CanTransition(next) {
		return &ErrInvalidTransition{Entity: "task " + t.ID, From: t.State.String(), To: next.String()}
	}
	t.State = next
	return nil
}

// TransitionFork moves a fork to next, returning an error if the lifecycle
// does not allow it. Callers persist the fork themselves.
func TransitionFork(f *Fork, next ForkState) error {
	if !next.Valid() {
		return fmt.Errorf("fork %s: unknown state %q", f.ID, next)
	}
	if f.State == next {
		return nil
	}
	if !f.State.CanTransition(next) {
		return &ErrInvalidTransition{Entity: "fork " + f.ID, From: f.State.String(), To: next.String()}
	}
	f.State = next
	return nil
}
