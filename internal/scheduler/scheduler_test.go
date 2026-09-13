package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/vm"
)

// fakeDriver reports a fixed capacity and records nothing else: the scheduler
// only ever asks it how much room there is.
type fakeDriver struct {
	mu       sync.Mutex
	capacity vm.Capacity
}

func (d *fakeDriver) Name() string { return "fake" }

func (d *fakeDriver) Capacity(context.Context) (vm.Capacity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.capacity, nil
}

func (d *fakeDriver) Create(context.Context, vm.Spec) (*vm.Instance, error) {
	return nil, vm.ErrNotSupported
}
func (d *fakeDriver) Get(context.Context, string) (*vm.Instance, error) { return nil, vm.ErrNotFound }
func (d *fakeDriver) List(context.Context) ([]*vm.Instance, error)      { return nil, nil }
func (d *fakeDriver) Destroy(context.Context, string) error             { return vm.ErrNotSupported }
func (d *fakeDriver) Exec(context.Context, string, vm.Command) (*vm.ExecResult, error) {
	return nil, vm.ErrNotSupported
}

// recordingLauncher captures which forks were handed off.
type recordingLauncher struct {
	mu       sync.Mutex
	launched []string
	done     chan struct{}
}

func newRecordingLauncher() *recordingLauncher {
	return &recordingLauncher{done: make(chan struct{}, 64)}
}

func (l *recordingLauncher) Launch(_ context.Context, fork *domain.Fork) {
	l.mu.Lock()
	l.launched = append(l.launched, fork.ID)
	l.mu.Unlock()
	l.done <- struct{}{}
}

func (l *recordingLauncher) ids() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.launched...)
}

type fixture struct {
	store    *store.Store
	driver   *fakeDriver
	launcher *recordingLauncher
	sched    *Scheduler
	user     *domain.User
	repo     *domain.Repo
	project  *domain.Project
	task     *domain.Task
	ctx      context.Context
}

// forkSize is the allocation each fork receives in these tests.
var forkSize = vm.Resources{VCPUs: 2, MemoryMiB: 2048, DiskGiB: 10}

func newFixture(t *testing.T, total vm.Resources, opts Options) *fixture {
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
	repo := &domain.Repo{UserID: user.ID, Name: "devex", RemoteURL: "git@example.com:devex.git"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	project := &domain.Project{RepoID: repo.ID, Name: "web", Path: "."}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	task := &domain.Task{UserID: user.ID, RepoID: repo.ID, Title: "t", Request: "r", State: domain.TaskRunning}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	driver := &fakeDriver{capacity: vm.Capacity{Total: total}}
	launcher := newRecordingLauncher()

	opts.ForkResources = forkSize
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return &fixture{
		store: st, driver: driver, launcher: launcher,
		sched: New(st, driver, launcher, opts),
		user:  user, repo: repo, project: project, task: task,
		ctx: ctx,
	}
}

// enqueue creates a queued fork, optionally in a serialization group.
func (f *fixture) enqueue(t *testing.T, name, group string) *domain.Fork {
	t.Helper()
	fork := &domain.Fork{
		TaskID: f.task.ID, UserID: f.user.ID, RepoID: f.repo.ID, ProjectID: f.project.ID,
		Name: name, Branch: "dabberz/" + name, SerializeGroup: group,
	}
	if err := f.store.CreateFork(f.ctx, fork); err != nil {
		t.Fatalf("CreateFork(%s): %v", name, err)
	}
	return fork
}

func (f *fixture) state(t *testing.T, forkID string) domain.ForkState {
	t.Helper()
	fork, err := f.store.GetFork(f.ctx, forkID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	return fork.State
}

func TestTickAdmitsWhatFits(t *testing.T) {
	// Room for exactly two forks.
	f := newFixture(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 20}, Options{})
	a := f.enqueue(t, "ratings", "")
	b := f.enqueue(t, "photos", "")
	c := f.enqueue(t, "rank-notes", "")

	result, err := f.sched.Tick(f.ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(result.Admitted) != 2 {
		t.Fatalf("admitted %d forks, want 2 (%v)", len(result.Admitted), result.Admitted)
	}
	if f.state(t, a.ID) != domain.ForkProvisioning || f.state(t, b.ID) != domain.ForkProvisioning {
		t.Fatal("admitted forks should be provisioning")
	}
	// The third waits rather than being squeezed in.
	if f.state(t, c.ID) != domain.ForkQueued {
		t.Fatalf("third fork state = %q, want it still queued", f.state(t, c.ID))
	}
	if len(result.Waiting) != 1 || result.Waiting[0].Reason != ReasonNoCapacity {
		t.Fatalf("waiting = %+v, want one fork waiting on capacity", result.Waiting)
	}

	// Each admitted fork is handed to the launcher exactly once.
	for range 2 {
		<-f.launcher.done
	}
	if got := len(f.launcher.ids()); got != 2 {
		t.Fatalf("launcher saw %d forks, want 2", got)
	}
}

func TestTickNeverPreemptsRunningWork(t *testing.T) {
	f := newFixture(t, vm.Resources{VCPUs: 2, MemoryMiB: 2048, DiskGiB: 10}, Options{})
	running := f.enqueue(t, "running", "")
	if _, err := f.sched.Tick(f.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	<-f.launcher.done

	// Report the machine as full and queue more work.
	f.driver.mu.Lock()
	f.driver.capacity.Used = forkSize
	f.driver.capacity.Instances = 1
	f.driver.mu.Unlock()

	waiting := f.enqueue(t, "waiting", "")
	result, err := f.sched.Tick(f.ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(result.Admitted) != 0 {
		t.Fatalf("admitted %v on a full machine", result.Admitted)
	}
	// The running fork must be untouched: nothing is evicted to make room.
	if got := f.state(t, running.ID); got != domain.ForkProvisioning {
		t.Fatalf("running fork state = %q, want it undisturbed", got)
	}
	if got := f.state(t, waiting.ID); got != domain.ForkQueued {
		t.Fatalf("waiting fork state = %q, want queued", got)
	}
}

func TestSerializationGroupRunsOneAtATime(t *testing.T) {
	f := newFixture(t, vm.Resources{VCPUs: 16, MemoryMiB: 16384, DiskGiB: 200}, Options{})
	// Two overlapping workstreams the orchestrator judged likely to collide,
	// plus one independent workstream.
	first := f.enqueue(t, "photo-upload", "media")
	second := f.enqueue(t, "image-resize", "media")
	independent := f.enqueue(t, "ratings", "")

	result, err := f.sched.Tick(f.ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(result.Admitted) != 2 {
		t.Fatalf("admitted %v, want the first of the group plus the independent fork", result.Admitted)
	}
	if f.state(t, second.ID) != domain.ForkQueued {
		t.Fatal("the second fork in a serialization group should wait")
	}
	// The independent fork must not be held up by an unrelated group.
	if f.state(t, independent.ID) != domain.ForkProvisioning {
		t.Fatal("an independent fork should not be blocked by another group")
	}
	var sawGroupReason bool
	for _, d := range result.Waiting {
		if d.ForkID == second.ID && d.Reason == ReasonGroupBusy {
			sawGroupReason = true
		}
	}
	if !sawGroupReason {
		t.Fatalf("waiting reasons = %+v, want the group-busy reason for %s", result.Waiting, second.ID)
	}

	// Once the first finishes, the sibling is admitted.
	firstFork, err := f.store.GetFork(f.ctx, first.ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	for _, next := range []domain.ForkState{domain.ForkCoding, domain.ForkVerifying, domain.ForkAwaitingMerge, domain.ForkMerging, domain.ForkMerged} {
		if err := f.store.TransitionFork(f.ctx, firstFork, next, ""); err != nil {
			t.Fatalf("transition to %q: %v", next, err)
		}
	}

	if _, err := f.sched.Tick(f.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := f.state(t, second.ID); got != domain.ForkProvisioning {
		t.Fatalf("sibling state = %q, want it admitted once the group freed up", got)
	}
}

func TestGroupsAreScopedPerTask(t *testing.T) {
	f := newFixture(t, vm.Resources{VCPUs: 16, MemoryMiB: 16384, DiskGiB: 200}, Options{})

	other := &domain.Task{UserID: f.user.ID, RepoID: f.repo.ID, Title: "other", Request: "r", State: domain.TaskRunning}
	if err := f.store.CreateTask(f.ctx, other); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	mine := f.enqueue(t, "photos", "media")
	theirs := &domain.Fork{
		TaskID: other.ID, UserID: f.user.ID, RepoID: f.repo.ID, ProjectID: f.project.ID,
		Name: "avatars", Branch: "b", SerializeGroup: "media",
	}
	if err := f.store.CreateFork(f.ctx, theirs); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}

	if _, err := f.sched.Tick(f.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	// Group names come from one task's plan, so an identical label in another
	// task must not serialize unrelated work.
	if f.state(t, mine.ID) != domain.ForkProvisioning || f.state(t, theirs.ID) != domain.ForkProvisioning {
		t.Fatal("identically named groups in different tasks should not serialize each other")
	}
}

func TestForksOfNonRunningTasksAreNotAdmitted(t *testing.T) {
	f := newFixture(t, vm.Resources{VCPUs: 16, MemoryMiB: 16384, DiskGiB: 200}, Options{})

	// A task still at the planning checkpoint must not spin up VMs.
	planning := &domain.Task{UserID: f.user.ID, RepoID: f.repo.ID, Title: "p", Request: "r", State: domain.TaskAwaitingPlan}
	if err := f.store.CreateTask(f.ctx, planning); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	fork := &domain.Fork{
		TaskID: planning.ID, UserID: f.user.ID, RepoID: f.repo.ID, ProjectID: f.project.ID,
		Name: "premature", Branch: "b",
	}
	if err := f.store.CreateFork(f.ctx, fork); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}

	result, err := f.sched.Tick(f.ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(result.Admitted) != 0 {
		t.Fatalf("admitted %v before the plan was approved", result.Admitted)
	}
	if len(result.Waiting) != 1 || result.Waiting[0].Reason != ReasonTaskNotReady {
		t.Fatalf("waiting = %+v, want the task-not-ready reason", result.Waiting)
	}
}

func TestHeadOfLineBlockingKeepsTheQueueFair(t *testing.T) {
	// One slot free, and a queue of three.
	f := newFixture(t, vm.Resources{VCPUs: 2, MemoryMiB: 2048, DiskGiB: 10}, Options{HeadOfLineBlocking: true})
	f.driver.mu.Lock()
	f.driver.capacity.Used = forkSize // already full
	f.driver.mu.Unlock()

	first := f.enqueue(t, "first", "")
	second := f.enqueue(t, "second", "")

	result, err := f.sched.Tick(f.ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(result.Admitted) != 0 {
		t.Fatalf("admitted %v on a full machine", result.Admitted)
	}
	reasons := map[string]string{}
	for _, d := range result.Waiting {
		reasons[d.ForkID] = d.Reason
	}
	if reasons[first.ID] != ReasonNoCapacity {
		t.Fatalf("first fork reason = %q, want %q", reasons[first.ID], ReasonNoCapacity)
	}
	// Everything behind the blocked head reports queue position, so the UI can
	// explain the wait rather than showing an unexplained stall.
	if reasons[second.ID] != ReasonQueueOrder {
		t.Fatalf("second fork reason = %q, want %q", reasons[second.ID], ReasonQueueOrder)
	}
}

func TestAdmissionIsRecordedOnTheActivityFeed(t *testing.T) {
	f := newFixture(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 40}, Options{})
	fork := f.enqueue(t, "ratings", "")

	if _, err := f.sched.Tick(f.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	<-f.launcher.done

	events, err := f.store.ListEvents(f.ctx, store.EventFilter{ForkID: fork.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Type == domain.EventForkAdmitted {
			found = true
		}
	}
	if !found {
		t.Fatalf("no admission event recorded; got %d events", len(events))
	}
}

func TestTickIsIdempotentForAlreadyAdmittedForks(t *testing.T) {
	f := newFixture(t, vm.Resources{VCPUs: 8, MemoryMiB: 8192, DiskGiB: 80}, Options{})
	f.enqueue(t, "ratings", "")

	if _, err := f.sched.Tick(f.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	<-f.launcher.done

	// A second pass must not re-admit a fork that has already left the queue.
	result, err := f.sched.Tick(f.ctx)
	if err != nil {
		t.Fatalf("Tick (repeat): %v", err)
	}
	if len(result.Admitted) != 0 {
		t.Fatalf("re-admitted %v on the second pass", result.Admitted)
	}
	if got := len(f.launcher.ids()); got != 1 {
		t.Fatalf("launcher was called %d times, want 1", got)
	}
}

func TestSnapshotReportsQueueAndCapacity(t *testing.T) {
	f := newFixture(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 40}, Options{})
	f.enqueue(t, "a", "")
	f.enqueue(t, "b", "")
	if _, err := f.sched.Tick(f.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	for range 2 {
		<-f.launcher.done
	}

	snap, err := f.sched.Snapshot(f.ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Queued) != 0 {
		t.Fatalf("queued = %d, want 0", len(snap.Queued))
	}
	if len(snap.Active) != 2 {
		t.Fatalf("active = %d, want 2", len(snap.Active))
	}
}

func TestRunStopsWithContext(t *testing.T) {
	f := newFixture(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 40}, Options{})
	ctx, cancel := context.WithCancel(f.ctx)

	done := make(chan error, 1)
	go func() { done <- f.sched.Run(ctx) }()

	f.enqueue(t, "ratings", "")
	f.sched.Nudge()
	<-f.launcher.done

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil on cancellation", err)
	}
}

func TestNothingIsAdmittedWithoutARunner(t *testing.T) {
	f := newFixture(t, vm.Resources{VCPUs: 16, MemoryMiB: 16384, DiskGiB: 200}, Options{})
	// A scheduler with nowhere to send admitted work must leave it queued
	// rather than moving it into a state where it holds capacity and stalls.
	f.sched.launcher = nil

	fork := f.enqueue(t, "ratings", "")
	result, err := f.sched.Tick(f.ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(result.Admitted) != 0 {
		t.Fatalf("admitted %v with no runner configured", result.Admitted)
	}
	if got := f.state(t, fork.ID); got != domain.ForkQueued {
		t.Fatalf("fork state = %q, want it left queued", got)
	}
	if len(result.Waiting) != 1 || result.Waiting[0].Reason != ReasonNoLauncher {
		t.Fatalf("waiting = %+v, want the missing-runner reason", result.Waiting)
	}
}

func TestNudgeNeverBlocks(t *testing.T) {
	f := newFixture(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 40}, Options{})
	// Nobody is consuming nudges; none of these may block.
	for range 100 {
		f.sched.Nudge()
	}
}
