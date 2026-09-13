// Package scheduler decides when a queued fork is allowed to start.
//
// The policy is deliberately one policy, applied everywhere: dabberz never
// pre-empts running work. When the machine is near capacity, new fork requests
// queue and wait, exactly as verification jobs queue for the shared UI VM.
// Nothing is evicted, downsized or killed to make room.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/vm"
)

// DefaultInterval is how often the scheduler re-examines the queue when
// nothing has nudged it.
const DefaultInterval = 5 * time.Second

// Launcher provisions an admitted fork. The scheduler calls it once per
// admission and does not wait for it: provisioning a VM takes far longer than
// a scheduling pass.
type Launcher interface {
	Launch(ctx context.Context, fork *domain.Fork)
}

// LauncherFunc adapts a function to Launcher.
type LauncherFunc func(ctx context.Context, fork *domain.Fork)

// Launch implements Launcher.
func (f LauncherFunc) Launch(ctx context.Context, fork *domain.Fork) { f(ctx, fork) }

// Options configures a Scheduler.
type Options struct {
	// Interval is the idle polling period. Admission is also triggered by
	// Nudge, so this is a safety net rather than the main path.
	Interval time.Duration
	// ForkResources is the allocation each fork VM receives.
	ForkResources vm.Resources
	// HeadOfLineBlocking keeps the queue strictly first-come-first-served: if
	// the oldest waiting fork does not fit, nothing behind it is admitted
	// either. This trades some utilisation for the guarantee that a large fork
	// is never starved by a stream of small ones. Forks held back only by
	// their serialization group are always passed over regardless, since
	// waiting on a sibling says nothing about capacity.
	HeadOfLineBlocking bool
	Logger             *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.ForkResources == (vm.Resources{}) {
		o.ForkResources = vm.Resources{VCPUs: 2, MemoryMiB: 4096, DiskGiB: 20}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Scheduler admits queued forks as capacity frees up.
type Scheduler struct {
	store    *store.Store
	driver   vm.Driver
	launcher Launcher
	opts     Options

	nudge chan struct{}

	// mu guards admission so that two passes cannot both claim the last slot.
	mu sync.Mutex
}

// New returns a scheduler that admits forks from st onto driver.
func New(st *store.Store, driver vm.Driver, launcher Launcher, opts Options) *Scheduler {
	return &Scheduler{
		store:    st,
		driver:   driver,
		launcher: launcher,
		opts:     opts.withDefaults(),
		nudge:    make(chan struct{}, 1),
	}
}

// Nudge asks the scheduler to run an admission pass promptly. It never blocks:
// a pass is already pending if the channel is full.
func (s *Scheduler) Nudge() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Run drives admission passes until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.opts.Interval)
	defer ticker.Stop()

	for {
		if _, err := s.Tick(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			// A failed pass must not kill the scheduler: the next pass may well
			// succeed, and stopping would strand every queued fork.
			s.opts.Logger.Error("scheduler pass failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-s.nudge:
		}
	}
}

// Decision records what happened to one queued fork in a pass.
type Decision struct {
	ForkID   string `json:"fork_id"`
	Admitted bool   `json:"admitted"`
	// Reason explains a refusal in terms the user can act on.
	Reason string `json:"reason,omitempty"`
}

// Result summarises an admission pass.
type Result struct {
	Admitted []string    `json:"admitted"`
	Waiting  []Decision  `json:"waiting"`
	Capacity vm.Capacity `json:"capacity"`
}

// Reasons a fork was not admitted.
const (
	ReasonNoCapacity   = "waiting for machine capacity"
	ReasonGroupBusy    = "waiting for an overlapping sibling workstream to finish"
	ReasonTaskNotReady = "task is not running"
	ReasonQueueOrder   = "waiting behind an earlier fork in the queue"
	// ReasonNoLauncher reports that nothing is configured to actually run an
	// admitted fork.
	ReasonNoLauncher = "no runner configured; work cannot start"
)

// Tick runs one admission pass: it walks the queue oldest-first and starts
// every fork that fits.
func (s *Scheduler) Tick(ctx context.Context) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	capacity, err := s.driver.Capacity(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("scheduler: read capacity: %w", err)
	}
	result := Result{Capacity: capacity}

	queued, err := s.store.ListForks(ctx, store.ForkFilter{States: []domain.ForkState{domain.ForkQueued}})
	if err != nil {
		return Result{}, fmt.Errorf("scheduler: list queued forks: %w", err)
	}
	if len(queued) == 0 {
		return result, nil
	}

	// With nothing to hand an admitted fork to, admitting one would move it
	// out of the queue into a state where it holds capacity and never runs.
	// Leaving it queued keeps the system honest about what is happening.
	if s.launcher == nil {
		for _, fork := range queued {
			result.Waiting = append(result.Waiting, Decision{ForkID: fork.ID, Reason: ReasonNoLauncher})
		}
		return result, nil
	}

	busyGroups, err := s.activeGroups(ctx)
	if err != nil {
		return Result{}, err
	}

	// runnable tracks whether any earlier fork was held back purely by
	// capacity, which is what head-of-line blocking keys off.
	capacityBlocked := false

	for _, fork := range queued {
		if capacityBlocked && s.opts.HeadOfLineBlocking {
			result.Waiting = append(result.Waiting, Decision{ForkID: fork.ID, Reason: ReasonQueueOrder})
			continue
		}

		ready, reason, err := s.taskReady(ctx, fork)
		if err != nil {
			return Result{}, err
		}
		if !ready {
			result.Waiting = append(result.Waiting, Decision{ForkID: fork.ID, Reason: reason})
			continue
		}

		// A fork whose serialization group is busy is passed over rather than
		// blocking the queue: it is waiting on a sibling, not on the machine.
		if key := groupKey(fork); key != "" && busyGroups[key] {
			result.Waiting = append(result.Waiting, Decision{ForkID: fork.ID, Reason: ReasonGroupBusy})
			continue
		}

		if !capacity.CanFit(s.opts.ForkResources) {
			capacityBlocked = true
			result.Waiting = append(result.Waiting, Decision{ForkID: fork.ID, Reason: ReasonNoCapacity})
			continue
		}

		if err := s.admit(ctx, fork); err != nil {
			return Result{}, err
		}

		// Claim the resources locally so the rest of this pass sees them as
		// taken; the driver will report them on the next pass.
		capacity.Used = capacity.Used.Add(s.opts.ForkResources)
		capacity.Instances++
		if key := groupKey(fork); key != "" {
			busyGroups[key] = true
		}
		result.Admitted = append(result.Admitted, fork.ID)
	}

	return result, nil
}

// admit moves a fork out of the queue and hands it to the launcher.
func (s *Scheduler) admit(ctx context.Context, fork *domain.Fork) error {
	if err := s.store.TransitionFork(ctx, fork, domain.ForkProvisioning, "admitted by scheduler"); err != nil {
		return fmt.Errorf("scheduler: admit %s: %w", fork.ID, err)
	}
	if err := s.store.AppendEvent(ctx, &domain.Event{
		UserID: fork.UserID, RepoID: fork.RepoID, TaskID: fork.TaskID, ForkID: fork.ID,
		Actor:   domain.ActorScheduler,
		Type:    domain.EventForkAdmitted,
		Message: "fork admitted; provisioning its VM",
		Data:    map[string]any{"waited_seconds": time.Since(fork.CreatedAt).Seconds()},
	}); err != nil {
		return fmt.Errorf("scheduler: record admission of %s: %w", fork.ID, err)
	}

	if s.launcher != nil {
		// Detached from this pass: provisioning is slow and must not hold the
		// scheduler lock or the caller's deadline.
		go s.launcher.Launch(context.WithoutCancel(ctx), fork)
	}
	return nil
}

// taskReady reports whether a fork's task is in a state that permits work.
func (s *Scheduler) taskReady(ctx context.Context, fork *domain.Fork) (bool, string, error) {
	task, err := s.store.GetTask(ctx, fork.TaskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, ReasonTaskNotReady, nil
		}
		return false, "", fmt.Errorf("scheduler: load task %s: %w", fork.TaskID, err)
	}
	if task.State != domain.TaskRunning {
		return false, ReasonTaskNotReady, nil
	}
	return true, "", nil
}

// activeGroups returns the serialization groups currently held.
//
// A group is held only while an agent is working, not merely while a fork
// holds a machine. A fork parked in awaiting_merge has finished; keeping its
// group would deadlock batch merge timing, where it waits for a grouped
// sibling that cannot start until it lets go.
func (s *Scheduler) activeGroups(ctx context.Context) (map[string]bool, error) {
	active, err := s.store.ListForks(ctx, store.ForkFilter{States: domain.WorkingStates()})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list working forks: %w", err)
	}
	busy := map[string]bool{}
	for _, fork := range active {
		if key := groupKey(fork); key != "" {
			busy[key] = true
		}
	}
	return busy, nil
}

// groupKey scopes a serialization group to its task. Group names come from one
// task's plan ("both touch image handling"), so they carry no meaning across
// tasks and must not serialize unrelated work that happens to share a label.
func groupKey(fork *domain.Fork) string {
	if fork.SerializeGroup == "" {
		return ""
	}
	return fork.TaskID + "/" + fork.SerializeGroup
}

// Snapshot describes the queue for the web UI without changing anything.
type Snapshot struct {
	Capacity vm.Capacity    `json:"capacity"`
	Queued   []*domain.Fork `json:"queued"`
	Active   []*domain.Fork `json:"active"`
}

// Snapshot reports the current queue and capacity.
func (s *Scheduler) Snapshot(ctx context.Context) (Snapshot, error) {
	capacity, err := s.driver.Capacity(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("scheduler: read capacity: %w", err)
	}
	queued, err := s.store.ListForks(ctx, store.ForkFilter{States: []domain.ForkState{domain.ForkQueued}})
	if err != nil {
		return Snapshot{}, fmt.Errorf("scheduler: list queued forks: %w", err)
	}
	active, err := s.store.ListForks(ctx, store.ForkFilter{Active: true})
	if err != nil {
		return Snapshot{}, fmt.Errorf("scheduler: list active forks: %w", err)
	}
	return Snapshot{Capacity: capacity, Queued: queued, Active: active}, nil
}
