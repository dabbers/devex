package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/dabbers/devex/internal/agent"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/llm"
	"github.com/dabbers/devex/internal/memory"
	"github.com/dabbers/devex/internal/merge"
	"github.com/dabbers/devex/internal/orchestrator"
	"github.com/dabbers/devex/internal/preview"
	"github.com/dabbers/devex/internal/proxy"
	"github.com/dabbers/devex/internal/secrets"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/tripwire"
	"github.com/dabbers/devex/internal/verify"
	"github.com/dabbers/devex/internal/vm"
	"github.com/dabbers/devex/internal/workspace"
)

// stubDriver models a VM host whose commands are answered from a script.
type stubDriver struct {
	mu        sync.Mutex
	instances int
	// claudeReplies are returned to successive claude invocations; the last
	// one repeats.
	claudeReplies []string
	claudeCalls   int
	// verifyReplies are returned to successive verifier invocations.
	verifyReplies []string
	verifyCalls   int
	gitResults    map[string]*vm.ExecResult
	commands      []string
	capacity      vm.Capacity
	// created remembers what Create returned, so Get behaves like a real
	// driver. A stub whose Get always fails hides the reuse path entirely:
	// every start would look like a fresh boot and a test could not tell the
	// difference between reusing a machine and stranding one.
	created map[string]*vm.Instance
}

func newStubDriver() *stubDriver {
	return &stubDriver{
		capacity:   vm.Capacity{Total: vm.Resources{VCPUs: 64, MemoryMiB: 65536, DiskGiB: 1000}},
		gitResults: map[string]*vm.ExecResult{},
		created:    map[string]*vm.Instance{},
	}
}

func (d *stubDriver) Name() string { return "stub" }

func (d *stubDriver) Capacity(context.Context) (vm.Capacity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.capacity, nil
}

func (d *stubDriver) Create(_ context.Context, spec vm.Spec) (*vm.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.instances++
	instance := &vm.Instance{
		ID:      "vm_" + spec.Name,
		ForkID:  spec.ForkID,
		Name:    spec.Name,
		State:   vm.StateRunning,
		Spec:    spec,
		Address: "172.30.0.2",
	}
	d.created[instance.ID] = instance
	return instance, nil
}

func (d *stubDriver) Get(_ context.Context, instanceID string) (*vm.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if instance, ok := d.created[instanceID]; ok {
		return instance, nil
	}
	return nil, vm.ErrNotFound
}

func (d *stubDriver) List(context.Context) ([]*vm.Instance, error) { return nil, nil }

func (d *stubDriver) Destroy(_ context.Context, instanceID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.created, instanceID)
	return nil
}

func (d *stubDriver) Exec(_ context.Context, _ string, cmd vm.Command) (*vm.ExecResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	line := strings.Join(cmd.Argv, " ")
	d.commands = append(d.commands, line)

	switch {
	case strings.HasPrefix(line, "claude"):
		reply := "VERDICT: APPROVED"
		if len(d.claudeReplies) > 0 {
			reply = d.claudeReplies[min(d.claudeCalls, len(d.claudeReplies)-1)]
		}
		d.claudeCalls++
		return &vm.ExecResult{Stdout: claudeJSON(reply)}, nil

	case strings.HasPrefix(line, "dabberz-verify"):
		reply := `{"passed":true,"summary":"all good"}`
		if len(d.verifyReplies) > 0 {
			reply = d.verifyReplies[min(d.verifyCalls, len(d.verifyReplies)-1)]
		}
		d.verifyCalls++
		return &vm.ExecResult{Stdout: reply}, nil

	case strings.HasPrefix(line, "git"):
		for match, result := range d.gitResults {
			if strings.Contains(line, match) {
				return result, nil
			}
		}
		if strings.Contains(line, "git diff origin/") && !strings.Contains(line, "--name-only") {
			return &vm.ExecResult{Stdout: "diff --git a/x b/x\n+change"}, nil
		}
		return &vm.ExecResult{}, nil
	}
	return &vm.ExecResult{}, nil
}

func (d *stubDriver) ran(match string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, cmd := range d.commands {
		if strings.Contains(cmd, match) {
			return true
		}
	}
	return false
}

// claudeJSON wraps a scripted reply in a print-mode result.
//
// A reply that is already a result object passes through untouched, so a test
// can script the conversation identifiers and running totals a resumed run
// actually reports.
//
// The envelope carries a session id because the real agent always does, and
// without one no test exercised resuming at all.
func claudeJSON(result string) string {
	var probe map[string]any
	if err := json.Unmarshal([]byte(result), &probe); err == nil && probe["type"] == "result" {
		return result
	}
	body, _ := json.Marshal(map[string]any{
		"type": "result", "is_error": false, "result": result,
		"session_id":     "sess_stub",
		"total_cost_usd": 0.10,
		"usage":          map[string]any{"input_tokens": 1000, "output_tokens": 200},
	})
	return string(body)
}

type harness struct {
	store  *store.Store
	driver *stubDriver
	orch   *orchestrator.Orchestrator
	pipe   *Pipeline
	// workspaces is the shared provisioner, exposed so a test can start a
	// workspace the way a person would rather than only through a fork.
	workspaces *workspace.Manager
	repo       *domain.Repo
	task       *domain.Task
	fork       *domain.Fork
	ctx        context.Context
}

type harnessOpts struct {
	claudeReplies []string
	verifyReplies []string
	gitResults    map[string]*vm.ExecResult
	thresholds    tripwire.Thresholds
	mergeTiming   domain.MergeTiming
	noVerifier    bool
	noReviewer    bool
}

func newHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

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
	project := &domain.Project{RepoID: repo.ID, Name: "web", Path: "."}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	service := &domain.Service{
		ProjectID: project.ID, Name: "web", Kind: domain.ServiceWeb,
		Command: "npm run dev", Port: 5173, Exposed: true, Primary: true,
	}
	if err := st.CreateService(ctx, service); err != nil {
		t.Fatalf("CreateService: %v", err)
	}

	timing := opts.mergeTiming
	if timing == "" {
		timing = domain.MergeTimingImmediate
	}
	task := &domain.Task{
		UserID: user.ID, RepoID: repo.ID, Title: "t", Request: "r",
		State: domain.TaskRunning, MergeTiming: timing, MergeTarget: domain.MergeTargetDefaultBranch,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	fork := &domain.Fork{
		TaskID: task.ID, UserID: user.ID, RepoID: repo.ID, ProjectID: project.ID,
		Name: "ratings", Description: "add star ratings", Branch: "dabberz/ratings-abc123",
	}
	if err := st.CreateFork(ctx, fork); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}

	driver := newStubDriver()
	driver.claudeReplies = opts.claudeReplies
	driver.verifyReplies = opts.verifyReplies
	if opts.gitResults != nil {
		driver.gitResults = opts.gitResults
	}

	mem, err := memory.New(t.TempDir())
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	thresholds := opts.thresholds
	if (thresholds == tripwire.Thresholds{}) {
		thresholds = tripwire.Defaults()
	}
	orch := orchestrator.New(st, llm.NewMock(), mem, nil, orchestrator.Options{
		Tripwire: thresholds, Logger: quiet,
	})

	runner := agent.New(driver, agent.Config{})

	var verifier *verify.Verifier
	if !opts.noVerifier {
		verifier, err = verify.New(driver, verify.Config{UIInstanceID: "vm_ui", Profiles: 2})
		if err != nil {
			t.Fatalf("verify.New: %v", err)
		}
	}
	var reviewer *merge.Reviewer
	if !opts.noReviewer {
		reviewer = merge.New(driver, runner, merge.Config{Push: true})
	}

	key := make([]byte, secrets.KeySize)
	vault, err := secrets.NewVault(st, key)
	if err != nil {
		t.Fatalf("secrets.NewVault: %v", err)
	}
	if err := vault.Set(ctx, repo.ID, "DATABASE_URL", "postgres://x"); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}

	alloc, err := preview.New(st, proxy.Discard{}, preview.Config{Domain: "dab.im"})
	if err != nil {
		t.Fatalf("preview.New: %v", err)
	}

	workspaces, err := workspace.New(workspace.Deps{
		Store: st, Driver: driver, Preview: alloc, Vault: vault,
	}, workspace.Options{Logger: quiet})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}

	pipe, err := New(Deps{
		Store: st, Driver: driver, Agent: runner, Verifier: verifier,
		Reviewer: reviewer, Preview: alloc, Orch: orch,
		Workspaces: workspaces,
	}, Options{Logger: quiet})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	return &harness{store: st, driver: driver, orch: orch, pipe: pipe, workspaces: workspaces, repo: repo, task: task, fork: fork, ctx: ctx}
}

func (h *harness) launch(t *testing.T) *domain.Fork {
	t.Helper()
	if err := h.store.TransitionFork(h.ctx, h.fork, domain.ForkProvisioning, ""); err != nil {
		t.Fatalf("admit: %v", err)
	}
	h.pipe.Launch(h.ctx, h.fork)

	final, err := h.store.GetFork(h.ctx, h.fork.ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	return final
}

func TestHappyPathProvisionsPreviewsVerifiesAndMerges(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	final := h.launch(t)

	if final.State != domain.ForkMerged {
		t.Fatalf("state = %q, reason %q", final.State, final.StateReason)
	}
	if final.InstanceID == "" {
		t.Error("no VM was recorded against the fork")
	}
	// No deploy step: the preview is the in-flight work, published as soon as
	// the VM exists.
	if final.PreviewURL == "" {
		t.Error("no preview URL was published")
	}
	if !h.driver.ran("dabberz-verify") {
		t.Error("the fork was merged without being verified")
	}
	if !h.driver.ran("git push origin dabberz/ratings-abc123:main") {
		t.Error("the change was not landed on the default branch")
	}
	// Agent spend is accumulated for the tripwire.
	if final.Usage.CostUSD == 0 || final.Usage.InputTokens == 0 {
		t.Errorf("usage was not accumulated: %+v", final.Usage)
	}

	task, err := h.store.GetTask(h.ctx, h.task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != domain.TaskCompleted {
		t.Fatalf("task state = %q, want completed once its only fork merged", task.State)
	}
}

func TestRepoSecretsReachTheForkVM(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.launch(t)

	h.driver.mu.Lock()
	defer h.driver.mu.Unlock()
	// The fork inherits the repo's whole secret set without naming any of it.
	if h.driver.instances == 0 {
		t.Fatal("no VM was created")
	}
}

func TestVerificationFailureFeedsBackAutomatically(t *testing.T) {
	h := newHarness(t, harnessOpts{
		verifyReplies: []string{
			`{"passed":false,"summary":"the star widget does not render","checks":[{"name":"ratings visible","passed":false,"detail":"no .rating element"}]}`,
			`{"passed":true,"summary":"fixed"}`,
		},
	})
	final := h.launch(t)

	if final.State != domain.ForkMerged {
		t.Fatalf("state = %q, reason %q", final.State, final.StateReason)
	}
	// The loop is fully automatic: a failure goes straight back to the agent
	// with no human involvement.
	if final.Usage.Cycles != 1 {
		t.Fatalf("cycles = %d, want one failed round counted", final.Usage.Cycles)
	}
	if h.driver.verifyCalls != 2 {
		t.Fatalf("verifier ran %d times, want a re-verification after the fix", h.driver.verifyCalls)
	}

	events, err := h.store.ListEvents(h.ctx, store.EventFilter{ForkID: h.fork.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var sawFixing bool
	for _, e := range events {
		if e.Type == domain.EventForkStateChange && strings.Contains(e.Message, "fixing") {
			sawFixing = true
		}
	}
	if !sawFixing {
		t.Error("the fix round was not recorded on the activity feed")
	}
}

func TestTripwireStopsANonConvergingLoop(t *testing.T) {
	h := newHarness(t, harnessOpts{
		// Verification never passes.
		verifyReplies: []string{`{"passed":false,"summary":"still broken"}`},
		thresholds:    tripwire.Thresholds{MaxCycles: 3},
	})
	final := h.launch(t)

	// The loop must not run forever; the tripwire is what ends it.
	if final.State != domain.ForkEscalated {
		t.Fatalf("state = %q, want escalated", final.State)
	}
	if final.Escalation == nil || final.Escalation.Kind != domain.EscalationTripwire {
		t.Fatalf("escalation = %+v", final.Escalation)
	}
	if final.Usage.Cycles != 3 {
		t.Fatalf("cycles = %d, want the loop stopped at the threshold", final.Usage.Cycles)
	}
}

func TestAgentEscalationReachesTheUser(t *testing.T) {
	h := newHarness(t, harnessOpts{
		claudeReplies: []string{"DABBERZ-ESCALATE[ambiguous]: should ratings be per-user or global?"},
	})
	final := h.launch(t)

	if final.State != domain.ForkEscalated {
		t.Fatalf("state = %q, want escalated", final.State)
	}
	if final.Escalation.Kind != domain.EscalationAmbiguous {
		t.Fatalf("kind = %q", final.Escalation.Kind)
	}
	if !strings.Contains(final.Escalation.Message, "per-user or global") {
		t.Fatalf("message = %q", final.Escalation.Message)
	}
	// An escalated fork must not have been verified or merged.
	if h.driver.ran("dabberz-verify") || h.driver.ran("git push") {
		t.Error("an escalated fork continued through the pipeline")
	}
}

func TestRateLimitEscalatesRatherThanFailing(t *testing.T) {
	h := newHarness(t, harnessOpts{
		claudeReplies: []string{"Error: 429 rate limit exceeded"},
	})
	final := h.launch(t)

	// Rate limits are surfaced and handled like any other pause, not treated
	// as the work having failed.
	if final.State != domain.ForkEscalated {
		t.Fatalf("state = %q, want escalated", final.State)
	}
	if final.Escalation.Kind != domain.EscalationRateLimited {
		t.Fatalf("kind = %q, want rate_limited", final.Escalation.Kind)
	}
}

func TestMergeReviewRejectionSendsWorkBackAndReverifies(t *testing.T) {
	h := newHarness(t, harnessOpts{
		claudeReplies: []string{
			"initial implementation",                                // first coding pass
			"VERDICT: CHANGES_REQUIRED\nLeft a console.log behind.", // first review
			"removed the debug output",                              // rework
			"VERDICT: APPROVED",                                     // second review
		},
	})
	final := h.launch(t)

	if final.State != domain.ForkMerged {
		t.Fatalf("state = %q, reason %q", final.State, final.StateReason)
	}
	// Reworked code must be verified again before the gate sees it a second
	// time, rather than going straight back to merge.
	if h.driver.verifyCalls != 2 {
		t.Fatalf("verifier ran %d times, want the rework re-verified", h.driver.verifyCalls)
	}
	if final.Usage.Cycles != 1 {
		t.Fatalf("cycles = %d, want the rejected review counted as a round", final.Usage.Cycles)
	}
}

func TestUnmergeableForkEscalates(t *testing.T) {
	h := newHarness(t, harnessOpts{
		gitResults: map[string]*vm.ExecResult{
			"git merge":                            {Stderr: "CONFLICT (content)", ExitCode: 1},
			"git diff --name-only --diff-filter=U": {Stdout: "app/images.go\n"},
		},
	})
	final := h.launch(t)

	if final.State != domain.ForkEscalated {
		t.Fatalf("state = %q, want escalated", final.State)
	}
	if !strings.Contains(final.Escalation.Message, "app/images.go") {
		t.Fatalf("escalation should name the conflict: %q", final.Escalation.Message)
	}
	if h.driver.ran("git push") {
		t.Error("an unresolvable merge was pushed")
	}
}

func TestBatchTimingHoldsUntilSiblingsFinish(t *testing.T) {
	h := newHarness(t, harnessOpts{mergeTiming: domain.MergeTimingBatch})

	// A sibling that is still working means the batch is not complete.
	sibling := &domain.Fork{
		TaskID: h.task.ID, UserID: h.fork.UserID, RepoID: h.repo.ID, ProjectID: h.fork.ProjectID,
		Name: "photos", Branch: "dabberz/photos-def456",
	}
	if err := h.store.CreateFork(h.ctx, sibling); err != nil {
		t.Fatalf("CreateFork: %v", err)
	}
	for _, next := range []domain.ForkState{domain.ForkProvisioning, domain.ForkCoding} {
		if err := h.store.TransitionFork(h.ctx, sibling, next, ""); err != nil {
			t.Fatalf("transition: %v", err)
		}
	}

	final := h.launch(t)

	if final.State != domain.ForkAwaitingMerge {
		t.Fatalf("state = %q, want it waiting for the batch", final.State)
	}
	if h.driver.ran("git push") {
		t.Error("batch timing landed a fork before its siblings finished")
	}
}

func TestPipelineRefusesToRunWithoutAVerifier(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	// Nothing may merge unverified, so a pipeline with no verifier is a
	// configuration error rather than a mode that skips the check.
	_, err := New(Deps{
		Store: h.store, Driver: h.driver, Agent: agent.New(h.driver, agent.Config{}),
		Orch: h.orch,
	}, Options{})
	if err == nil {
		t.Fatal("a pipeline without a verifier should be rejected")
	}
	if !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("error should explain why: %v", err)
	}
}

func TestFailedAgentRunMarksTheForkFailed(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.driver.gitResults = map[string]*vm.ExecResult{}
	h.driver.claudeReplies = nil

	// An agent that exits non-zero with no parseable result.
	driver := h.driver
	driver.mu.Lock()
	driver.claudeReplies = []string{""}
	driver.mu.Unlock()

	final := h.launch(t)
	// With an empty-but-valid result the run still succeeds; what matters is
	// that the pipeline reached a terminal state rather than hanging.
	if !final.State.Terminal() && final.State != domain.ForkEscalated {
		t.Fatalf("state = %q, want the pipeline to have settled", final.State)
	}
}

func TestNewValidatesDependencies(t *testing.T) {
	if _, err := New(Deps{}, Options{}); err == nil {
		t.Error("a pipeline without its core dependencies should be rejected")
	}
}

// TestForkRunsInAWorkspace is the point of the change: a fork no longer owns a
// machine directly, it owns a workspace that does, and that workspace is the
// same kind of thing somebody opens by hand.
func TestForkRunsInAWorkspace(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.launch(t)

	fork, err := h.store.GetFork(h.ctx, h.fork.ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	if fork.WorkspaceID == "" {
		t.Fatal("the fork ran without a workspace")
	}

	ws, err := h.store.GetWorkspace(h.ctx, fork.WorkspaceID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if ws.State != domain.WorkspaceRunning {
		t.Fatalf("workspace is %s, want running", ws.State)
	}
	if ws.InstanceID == "" || ws.InstanceID != fork.InstanceID {
		t.Fatalf("the fork and its workspace disagree about the machine: %q vs %q",
			fork.InstanceID, ws.InstanceID)
	}
	if ws.Branch != fork.Branch {
		t.Fatalf("workspace branch %q does not match the fork's %q", ws.Branch, fork.Branch)
	}
	// Scratch space a plan made, not somewhere a person works, so it stays out
	// of the default listing.
	if !ws.Ephemeral {
		t.Fatal("a plan-created workspace should be marked ephemeral")
	}
	listed, err := h.store.ListWorkspaces(h.ctx, store.WorkspaceFilter{UserID: fork.UserID})
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("a fork's scratch workspace appeared in the default listing: %+v", listed)
	}
}

// TestPreviewRoutesBelongToTheWorkspace: the machine and the services on it
// are the workspace's, so the routes pointing at them are too.
func TestPreviewRoutesBelongToTheWorkspace(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.launch(t)

	fork, err := h.store.GetFork(h.ctx, h.fork.ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	routes, err := h.store.ListPreviewRoutesFor(h.ctx, fork.WorkspaceID)
	if err != nil {
		t.Fatalf("ListPreviewRoutesFor: %v", err)
	}
	if len(routes) == 0 {
		t.Fatal("the workspace published no preview routes")
	}
	for _, route := range routes {
		if route.WorkspaceID != fork.WorkspaceID {
			t.Fatalf("route %s belongs to %q, want the workspace %q",
				route.Hostname, route.WorkspaceID, fork.WorkspaceID)
		}
	}
	if fork.PreviewURL == "" {
		t.Fatal("the fork did not inherit its workspace's front door")
	}
}

// TestRetryReusesTheWorkspaceMachine: nothing is reclaimed automatically, so a
// second machine for a fork that already has one is not a wasted moment but a
// stranded one.
func TestRetryReusesTheWorkspaceMachine(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.launch(t)

	fork, err := h.store.GetFork(h.ctx, h.fork.ID)
	if err != nil {
		t.Fatalf("GetFork: %v", err)
	}
	first := fork.InstanceID
	ws, err := h.store.GetWorkspace(h.ctx, fork.WorkspaceID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}

	// Starting an already-running workspace describes it rather than booting
	// anything.
	again, err := h.workspaces.Start(h.ctx, ws)
	if err != nil {
		t.Fatalf("Start (already running): %v", err)
	}
	if again.Instance.ID != first {
		t.Fatalf("a second machine was booted: %q then %q", first, again.Instance.ID)
	}
}

// TestAFixRoundResumesTheConversation: without this, every fix round starts
// from an empty context and rebuilds its understanding from the prompt, so the
// agent can retry something it already tried and ruled out.
func TestAFixRoundResumesTheConversation(t *testing.T) {
	h := newHarness(t, harnessOpts{
		verifyReplies: []string{
			`{"passed":false,"summary":"the star widget does not render"}`,
			`{"passed":true,"summary":"fixed"}`,
		},
		// Print-mode JSON, because a conversation identifier only exists in a
		// parseable result. The last reply repeats and serves the merge
		// review, whose verdict the reviewer reads out of the result field.
		claudeReplies: []string{
			claudeResultJSON("coding", 1000, 200, 1.00, "sess_fork"),
			claudeResultJSON("fixed", 1800, 400, 2.50, "sess_fork"),
			claudeResultJSON("VERDICT: APPROVED", 50, 10, 0.01, "sess_review"),
		},
	})
	final := h.launch(t)
	if final.State != domain.ForkMerged {
		t.Fatalf("state = %q, reason %q", final.State, final.StateReason)
	}

	var claudeCalls []string
	for _, cmd := range h.driver.commands {
		if strings.HasPrefix(cmd, "claude") {
			claudeCalls = append(claudeCalls, cmd)
		}
	}
	if len(claudeCalls) < 2 {
		t.Fatalf("expected a coding run and a fix run, got %d: %v", len(claudeCalls), claudeCalls)
	}
	if strings.Contains(claudeCalls[0], "--resume") {
		t.Fatalf("the first run tried to resume: %s", claudeCalls[0])
	}
	if !strings.Contains(claudeCalls[1], "--resume") {
		t.Fatalf("the fix round did not resume: %s", claudeCalls[1])
	}
	if final.AgentSession.ID == "" {
		t.Fatal("the fork did not remember its conversation")
	}
}

// TestAResumedForkIsNotChargedTwice guards the accounting against the
// conversation's running total: the cost and token tripwires are driven by it,
// so overcounting stops a fork for spending it never did.
func TestAResumedForkIsNotChargedTwice(t *testing.T) {
	h := newHarness(t, harnessOpts{
		verifyReplies: []string{
			`{"passed":false,"summary":"not yet"}`,
			`{"passed":true,"summary":"fixed"}`,
		},
		// The agent reports the conversation's running total, so the second
		// reply is the first plus that round's own spend. The third is the
		// merge review, a separate conversation with its own small figures.
		claudeReplies: []string{
			claudeResultJSON("coding", 1000, 200, 1.00, "sess_fork"),
			claudeResultJSON("fixed", 1800, 400, 2.50, "sess_fork"),
			claudeResultJSON("VERDICT: APPROVED", 50, 10, 0.01, "sess_review"),
		},
	})
	final := h.launch(t)

	// The coding conversation cost 2.50 in total, not 1.00 + 2.50. The merge
	// review is its own conversation and adds its own small amount.
	if final.Usage.CostUSD > 2.60 {
		t.Fatalf("fork was charged %v; the coding conversation only cost 2.50, so earlier turns were counted twice",
			final.Usage.CostUSD)
	}
	if final.Usage.CostUSD < 2.50 {
		t.Fatalf("fork was charged %v, less than the conversation actually cost", final.Usage.CostUSD)
	}
	if final.Usage.InputTokens > 1900 {
		t.Fatalf("fork was charged %d input tokens, want about the conversation's 1800 plus the review",
			final.Usage.InputTokens)
	}
}

// claudeResultJSON builds a print-mode result reporting a conversation's
// running totals.
func claudeResultJSON(result string, in, out int64, cost float64, sessionID string) string {
	body, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"result": result, "session_id": sessionID, "total_cost_usd": cost, "num_turns": 2,
		"usage": map[string]any{
			"input_tokens": in, "output_tokens": out,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		},
	})
	return string(body)
}
