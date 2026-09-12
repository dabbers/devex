package domain

import (
	"errors"
	"testing"
)

func TestForkTransitionsReferenceKnownStates(t *testing.T) {
	for from, targets := range forkTransitions {
		if !from.Valid() {
			t.Errorf("fork state %q has transitions but is not itself valid", from)
		}
		for _, to := range targets {
			if !to.Valid() {
				t.Errorf("fork transition %q -> %q targets an unknown state", from, to)
			}
			if to == from {
				t.Errorf("fork transition %q -> %q is a self-loop", from, to)
			}
		}
	}
}

func TestTaskTransitionsReferenceKnownStates(t *testing.T) {
	for from, targets := range taskTransitions {
		for _, to := range targets {
			if !to.Valid() {
				t.Errorf("task transition %q -> %q targets an unknown state", from, to)
			}
		}
	}
}

// Every non-initial state must be reachable, or it is dead configuration.
func TestEveryForkStateIsReachableFromQueued(t *testing.T) {
	seen := map[ForkState]bool{ForkQueued: true}
	queue := []ForkState{ForkQueued}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range forkTransitions[cur] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	for state := range forkTransitions {
		if !seen[state] {
			t.Errorf("fork state %q is unreachable from %q", state, ForkQueued)
		}
	}
}

// Every non-terminal state must be able to reach a terminal one, or a fork can
// get permanently stuck.
func TestEveryForkStateCanReachTerminal(t *testing.T) {
	for start := range forkTransitions {
		seen := map[ForkState]bool{start: true}
		queue := []ForkState{start}
		reached := false
		for len(queue) > 0 && !reached {
			cur := queue[0]
			queue = queue[1:]
			if cur.Terminal() {
				reached = true
				break
			}
			for _, next := range forkTransitions[cur] {
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
		if !reached {
			t.Errorf("fork state %q cannot reach any terminal state", start)
		}
	}
}

func TestTerminalForkStatesAreNotActive(t *testing.T) {
	for state := range forkTransitions {
		if state.Terminal() && state.Active() {
			t.Errorf("terminal fork state %q is marked active", state)
		}
	}
	// A queued fork has no VM yet, so it must not count against capacity.
	if ForkQueued.Active() {
		t.Error("queued forks must not consume scheduler capacity")
	}
	// An escalated fork is paused on the user and should free its slot.
	if ForkEscalated.Active() {
		t.Error("escalated forks must not consume scheduler capacity")
	}
}

func TestTransitionForkRejectsIllegalMoves(t *testing.T) {
	f := &Fork{ID: "fork_x", State: ForkQueued}
	if err := TransitionFork(f, ForkMerged); err == nil {
		t.Fatal("expected queued -> merged to be rejected")
	} else {
		var invalid *ErrInvalidTransition
		if !errors.As(err, &invalid) {
			t.Fatalf("expected *ErrInvalidTransition, got %T", err)
		}
	}
	if f.State != ForkQueued {
		t.Fatalf("rejected transition mutated state to %q", f.State)
	}
}

func TestTransitionForkAcceptsLegalMovesAndIsIdempotent(t *testing.T) {
	f := &Fork{ID: "fork_x", State: ForkQueued}
	for _, next := range []ForkState{ForkProvisioning, ForkCoding, ForkVerifying, ForkFixing, ForkVerifying, ForkAwaitingMerge, ForkMerging, ForkMerged} {
		if err := TransitionFork(f, next); err != nil {
			t.Fatalf("TransitionFork(-> %q): %v", next, err)
		}
	}
	if f.State != ForkMerged {
		t.Fatalf("final state = %q, want %q", f.State, ForkMerged)
	}
	if err := TransitionFork(f, ForkMerged); err != nil {
		t.Fatalf("re-entering the current state should be a no-op, got %v", err)
	}
}

func TestTransitionForkRejectsUnknownState(t *testing.T) {
	f := &Fork{ID: "fork_x", State: ForkQueued}
	if err := TransitionFork(f, ForkState("banana")); err == nil {
		t.Fatal("expected unknown target state to be rejected")
	}
}

func TestTaskLifecycleAllowsReplanningAfterQuestions(t *testing.T) {
	task := &Task{ID: "task_x", State: TaskDraft}
	for _, next := range []TaskState{TaskPlanning, TaskAwaitingPlan, TaskPlanning, TaskAwaitingPlan, TaskRunning, TaskCompleted} {
		if err := TransitionTask(task, next); err != nil {
			t.Fatalf("TransitionTask(-> %q): %v", next, err)
		}
	}
	if !task.State.Terminal() {
		t.Fatalf("state %q should be terminal", task.State)
	}
}

func TestPlanNeedsInput(t *testing.T) {
	var nilPlan *Plan
	if nilPlan.NeedsInput() {
		t.Error("a nil plan does not need input")
	}
	ready := &Plan{Questions: []Question{{Text: "q", Answer: "a"}}}
	if ready.NeedsInput() {
		t.Error("a fully answered plan does not need input")
	}
	blocked := &Plan{Questions: []Question{{Text: "q1", Answer: "a"}, {Text: "q2"}}}
	if !blocked.NeedsInput() {
		t.Error("a plan with an unanswered question needs input")
	}
}

func TestMergePreferencesValidate(t *testing.T) {
	if !MergeTargetDefaultBranch.Valid() || !MergeTargetIntegrationBranch.Valid() {
		t.Error("known merge targets should validate")
	}
	if MergeTarget("elsewhere").Valid() {
		t.Error("unknown merge target should not validate")
	}
	if !MergeTimingImmediate.Valid() || !MergeTimingBatch.Valid() {
		t.Error("known merge timings should validate")
	}
	if MergeTiming("eventually").Valid() {
		t.Error("unknown merge timing should not validate")
	}
}

func TestUsageAdd(t *testing.T) {
	u := Usage{Cycles: 1, InputTokens: 10, OutputTokens: 5, CostUSD: 0.5}
	u.Add(Usage{Cycles: 2, InputTokens: 20, OutputTokens: 7, CostUSD: 1.25})
	want := Usage{Cycles: 3, InputTokens: 30, OutputTokens: 12, CostUSD: 1.75}
	if u != want {
		t.Fatalf("Add() = %+v, want %+v", u, want)
	}
}
