package merge

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/dabbers/devex/internal/agent"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/vm"
)

// scriptDriver answers commands from a table keyed by a prefix of the argv.
type scriptDriver struct {
	mu       sync.Mutex
	handlers []handler
	commands []string
}

type handler struct {
	match  string
	result *vm.ExecResult
	err    error
	// once limits a handler to a single use, so a test can model state
	// changing between two identical commands.
	once bool
	used bool
}

func (d *scriptDriver) on(match string, result *vm.ExecResult) *scriptDriver {
	d.handlers = append(d.handlers, handler{match: match, result: result})
	return d
}

func (d *scriptDriver) onceOn(match string, result *vm.ExecResult) *scriptDriver {
	d.handlers = append(d.handlers, handler{match: match, result: result, once: true})
	return d
}

func (d *scriptDriver) Name() string                                  { return "script" }
func (d *scriptDriver) Capacity(context.Context) (vm.Capacity, error) { return vm.Capacity{}, nil }
func (d *scriptDriver) Create(context.Context, vm.Spec) (*vm.Instance, error) {
	return nil, vm.ErrNotSupported
}
func (d *scriptDriver) Get(context.Context, string) (*vm.Instance, error) { return nil, vm.ErrNotFound }
func (d *scriptDriver) List(context.Context) ([]*vm.Instance, error)      { return nil, nil }
func (d *scriptDriver) Destroy(context.Context, string) error             { return nil }

func (d *scriptDriver) Exec(_ context.Context, _ string, cmd vm.Command) (*vm.ExecResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	line := strings.Join(cmd.Argv, " ")
	d.commands = append(d.commands, line)
	for i := range d.handlers {
		h := &d.handlers[i]
		if h.once && h.used {
			continue
		}
		if strings.Contains(line, h.match) {
			h.used = true
			if h.err != nil {
				return nil, h.err
			}
			return h.result, nil
		}
	}
	return &vm.ExecResult{ExitCode: 0}, nil
}

func (d *scriptDriver) ran(match string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, cmd := range d.commands {
		if strings.Contains(cmd, match) {
			return true
		}
	}
	return false
}

func ok(stdout string) *vm.ExecResult   { return &vm.ExecResult{Stdout: stdout} }
func fail(stderr string) *vm.ExecResult { return &vm.ExecResult{Stderr: stderr, ExitCode: 1} }

// agentSaying builds an agent runner whose claude invocation returns text.
func agentSaying(driver *scriptDriver, text string) *agent.Runner {
	body, _ := json.Marshal(map[string]any{
		"type": "result", "is_error": false, "result": text,
		"total_cost_usd": 0.05,
		"usage":          map[string]any{"input_tokens": 500, "output_tokens": 100},
	})
	driver.on("claude", ok(string(body)))
	return agent.New(driver, agent.Config{})
}

func testFork() *domain.Fork {
	return &domain.Fork{
		ID: "fork_1", Name: "ratings", Description: "add star ratings",
		Branch: "dabberz/ratings-abc123",
	}
}

func testRequest() Request {
	return Request{Fork: testFork(), TargetBranch: "main", InstanceID: "vm_1"}
}

func TestMergeLandsACleanChange(t *testing.T) {
	driver := (&scriptDriver{}).
		on("git diff origin/main...", ok("diff --git a/x b/x\n+added")).
		on("git merge", ok(""))
	runner := agentSaying(driver, "Looks correct and matches the repo's conventions.\nVERDICT: APPROVED")

	r := New(driver, runner, Config{Push: true})
	res, err := r.Merge(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("outcome = %q, summary %q", res.Outcome, res.Summary)
	}
	// The merge must be against a freshly fetched target, not a stale one.
	if !driver.ran("git fetch origin") {
		t.Error("the remote was not fetched before merging")
	}
	if !driver.ran("git push origin dabberz/ratings-abc123:main") {
		t.Error("the change was not landed on the target branch")
	}
	// The review cost counts toward the fork's budget.
	if res.Usage.CostUSD == 0 {
		t.Error("review usage was not recorded")
	}
}

func TestQualityGateBlocksBeforeAnythingIsPushed(t *testing.T) {
	driver := (&scriptDriver{}).
		on("git diff origin/main...", ok("diff --git a/x b/x\n+console.log('debug')"))
	runner := agentSaying(driver, "Left debugging output in the request handler.\nVERDICT: CHANGES_REQUIRED")

	res, err := New(driver, runner, Config{Push: true}).Merge(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Outcome != OutcomeRejected {
		t.Fatalf("outcome = %q, want rejected", res.Outcome)
	}
	// Nothing may reach a shared branch once the gate says no.
	if driver.ran("git merge") || driver.ran("git push") {
		t.Fatalf("a rejected change still touched the repo: %v", driver.commands)
	}
	if !strings.Contains(res.QualityFeedback, "debugging output") {
		t.Fatalf("the gate's feedback was lost: %q", res.QualityFeedback)
	}
}

func TestQualityGateIsClosedByDefault(t *testing.T) {
	// An agent that answers without a verdict, or cannot run, must not be
	// read as approval.
	for name, verdict := range map[string]string{
		"no verdict":     "The change seems fine to me.",
		"ambiguous text": "I would approve this if the tests passed.",
		"empty":          "",
	} {
		driver := (&scriptDriver{}).on("git diff origin/main...", ok("some diff"))
		runner := agentSaying(driver, verdict)

		res, err := New(driver, runner, Config{Push: true}).Merge(context.Background(), testRequest())
		if err != nil {
			t.Fatalf("Merge(%s): %v", name, err)
		}
		if res.Outcome == OutcomeMerged {
			t.Errorf("Merge(%s) landed without an explicit approval", name)
		}
	}
}

func TestEmptyDiffNeedsNoReview(t *testing.T) {
	driver := (&scriptDriver{}).on("git diff origin/main...", ok("   "))
	runner := agentSaying(driver, "VERDICT: CHANGES_REQUIRED")

	res, err := New(driver, runner, Config{}).Merge(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("outcome = %q; a workstream with no changes should land trivially", res.Outcome)
	}
}

func TestConflictsAreResolvedOnTheForkBranch(t *testing.T) {
	driver := (&scriptDriver{}).
		on("git diff origin/main...", ok("some diff")).
		on("git merge", fail("CONFLICT (content): Merge conflict in app/images.go")).
		// The conflict is reported once, then the agent resolves it.
		onceOn("git diff --name-only --diff-filter=U", ok("app/images.go\n")).
		on("git diff --name-only --diff-filter=U", ok("")).
		on("git status --porcelain", ok(" M app/images.go"))

	runner := agentSaying(driver, "Kept both changes.\nVERDICT: APPROVED")

	res, err := New(driver, runner, Config{Push: true}).Merge(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("outcome = %q, summary %q", res.Outcome, res.Summary)
	}
	if !res.ResolvedConflicts {
		t.Error("the resolution was not recorded")
	}
	if len(res.ConflictedFiles) != 1 || res.ConflictedFiles[0] != "app/images.go" {
		t.Fatalf("conflicted files = %v", res.ConflictedFiles)
	}
	// Resolving happens on the fork's branch, so a failed resolution never
	// leaves the shared branch broken.
	if !driver.ran("git merge --no-edit origin/main") {
		t.Error("the target should be merged into the fork, not the other way round")
	}
}

func TestUnresolvedConflictsGoToTheUser(t *testing.T) {
	driver := (&scriptDriver{}).
		on("git diff origin/main...", ok("some diff")).
		on("git merge", fail("CONFLICT (content): Merge conflict in app/images.go")).
		// Still conflicted after the agent's attempt.
		on("git diff --name-only --diff-filter=U", ok("app/images.go\n"))

	// The gate approves; whether the conflict is resolved is decided by the
	// state of the tree afterwards, not by what the agent says.
	runner := agentSaying(driver, "VERDICT: APPROVED\nI could not reconcile these.")

	res, err := New(driver, runner, Config{Push: true}).Merge(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Outcome != OutcomeNeedsUser {
		t.Fatalf("outcome = %q, want needs_user", res.Outcome)
	}
	// A tree that still has conflicts must never be pushed, whatever the
	// agent claimed.
	if driver.ran("git push") {
		t.Fatal("an unresolved merge was pushed")
	}
	if !strings.Contains(res.Summary, "app/images.go") {
		t.Fatalf("summary should name the conflict: %q", res.Summary)
	}
}

func TestAgentClaimingSuccessIsCheckedAgainstTheTree(t *testing.T) {
	driver := (&scriptDriver{}).
		on("git diff origin/main...", ok("some diff")).
		on("git merge", fail("CONFLICT")).
		onceOn("git diff --name-only --diff-filter=U", ok("a.go\n")).
		on("git diff --name-only --diff-filter=U", ok("")).
		// git still reports an unmerged path in status.
		on("git status --porcelain", ok("UU a.go"))

	runner := agentSaying(driver, "VERDICT: APPROVED\nAll conflicts resolved!")

	res, err := New(driver, runner, Config{Push: true}).Merge(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// Trust the tree, not the agent's account of it.
	if res.Outcome != OutcomeNeedsUser {
		t.Fatalf("outcome = %q; an unfinished merge must not be treated as resolved", res.Outcome)
	}
}

func TestMergeFailureWithoutConflictsGoesToTheUser(t *testing.T) {
	driver := (&scriptDriver{}).
		on("git diff origin/main...", ok("some diff")).
		on("git merge", fail("error: Your local changes would be overwritten")).
		on("git diff --name-only --diff-filter=U", ok(""))

	runner := agentSaying(driver, "VERDICT: APPROVED")

	res, err := New(driver, runner, Config{}).Merge(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// Guessing at a non-conflict failure would be worse than asking.
	if res.Outcome != OutcomeNeedsUser {
		t.Fatalf("outcome = %q, want needs_user", res.Outcome)
	}
	if !strings.Contains(res.Summary, "local changes") {
		t.Fatalf("summary lost git's explanation: %q", res.Summary)
	}
}

func TestPushFailureIsReportedRatherThanClaimingSuccess(t *testing.T) {
	driver := (&scriptDriver{}).
		on("git diff origin/main...", ok("some diff")).
		on("git merge", ok("")).
		on("git push origin dabberz/ratings-abc123:main", fail("! [rejected] non-fast-forward"))

	runner := agentSaying(driver, "VERDICT: APPROVED")

	res, err := New(driver, runner, Config{Push: true}).Merge(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Outcome != OutcomeNeedsUser {
		t.Fatalf("outcome = %q, want the push failure surfaced", res.Outcome)
	}
	if !strings.Contains(res.Summary, "non-fast-forward") {
		t.Fatalf("summary lost git's explanation: %q", res.Summary)
	}
}

func TestMergeRespectsTheTaskTargetBranch(t *testing.T) {
	driver := (&scriptDriver{}).
		on("git diff origin/feat/batch-1...", ok("some diff")).
		on("git merge", ok(""))
	runner := agentSaying(driver, "VERDICT: APPROVED")

	req := testRequest()
	// Where a fork lands is a per-task preference, not an assumption.
	req.TargetBranch = "feat/batch-1"

	res, err := New(driver, runner, Config{Push: true}).Merge(context.Background(), req)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("outcome = %q", res.Outcome)
	}
	if !driver.ran("git push origin dabberz/ratings-abc123:feat/batch-1") {
		t.Fatalf("did not land on the integration branch: %v", driver.commands)
	}
}

func TestMergeValidatesItsRequest(t *testing.T) {
	driver := &scriptDriver{}
	r := New(driver, agentSaying(driver, ""), Config{})

	if _, err := r.Merge(context.Background(), Request{Fork: testFork(), TargetBranch: "main"}); err == nil {
		t.Error("a merge without a VM should be rejected")
	}
	if _, err := r.Merge(context.Background(), Request{Fork: testFork(), InstanceID: "vm_1"}); err == nil {
		t.Error("a merge without a target branch should be rejected")
	}
}

func TestFetchFailureIsAnError(t *testing.T) {
	driver := (&scriptDriver{}).on("git fetch", fail("could not resolve host"))
	r := New(driver, agentSaying(driver, ""), Config{})

	// Being unable to reach the remote is infrastructure, not a verdict.
	if _, err := r.Merge(context.Background(), testRequest()); err == nil {
		t.Fatal("a failed fetch should surface as an error")
	}
}

func TestSkipQualityGateBypassesReview(t *testing.T) {
	driver := (&scriptDriver{}).on("git merge", ok(""))
	runner := agentSaying(driver, "VERDICT: CHANGES_REQUIRED")

	res, err := New(driver, runner, Config{SkipQualityGate: true}).Merge(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Outcome != OutcomeMerged {
		t.Fatalf("outcome = %q", res.Outcome)
	}
	if driver.ran("claude") {
		t.Error("the review agent ran despite the gate being skipped")
	}
}

func TestReviewPromptDemandsAnExplicitVerdict(t *testing.T) {
	prompt := reviewPrompt(testFork(), "main")
	for _, want := range []string{"VERDICT: APPROVED", "VERDICT: CHANGES_REQUIRED", "ratings", "add star ratings", "main"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("review prompt is missing %q", want)
		}
	}
	// A gate that blocks on taste would never let anything land.
	if !strings.Contains(prompt, "proportionate") {
		t.Error("the review prompt should ask for proportionate judgement")
	}
}

func TestConflictPromptProtectsBothSides(t *testing.T) {
	prompt := conflictPrompt(testFork(), "main", []string{"app/images.go"})
	if !strings.Contains(prompt, "app/images.go") {
		t.Error("the conflicted file should be named")
	}
	// The other side is another workstream's finished work; discarding it to
	// make the conflict disappear silently loses that work.
	if !strings.Contains(prompt, "Do not discard the other side's changes") {
		t.Error("the prompt should forbid resolving by discarding the other side")
	}
	if !strings.Contains(prompt, "DABBERZ-ESCALATE") {
		t.Error("the prompt should offer the escalation route")
	}
}
