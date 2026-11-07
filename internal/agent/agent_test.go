package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/vm"
)

// fakeDriver returns a scripted exec result and records the command it got.
type fakeDriver struct {
	result  *vm.ExecResult
	err     error
	lastCmd vm.Command
	lastVM  string
}

func (d *fakeDriver) Name() string                                  { return "fake" }
func (d *fakeDriver) Capacity(context.Context) (vm.Capacity, error) { return vm.Capacity{}, nil }
func (d *fakeDriver) Create(context.Context, vm.Spec) (*vm.Instance, error) {
	return nil, vm.ErrNotSupported
}
func (d *fakeDriver) Get(context.Context, string) (*vm.Instance, error) { return nil, vm.ErrNotFound }
func (d *fakeDriver) List(context.Context) ([]*vm.Instance, error)      { return nil, nil }
func (d *fakeDriver) Destroy(context.Context, string) error             { return nil }

func (d *fakeDriver) Exec(_ context.Context, instanceID string, cmd vm.Command) (*vm.ExecResult, error) {
	d.lastVM = instanceID
	d.lastCmd = cmd
	if d.err != nil {
		return nil, d.err
	}
	return d.result, nil
}

func claudeJSON(result string, isError bool, in, out int64, cost float64) string {
	body, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": isError,
		"result": result, "session_id": "s1", "total_cost_usd": cost, "num_turns": 3,
		"usage": map[string]any{
			"input_tokens": in, "output_tokens": out,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		},
	})
	return string(body)
}

func TestRunInvokesClaudeCodeInPrintMode(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{Stdout: claudeJSON("done", false, 1000, 200, 0.42)}}
	r := New(driver, Config{OAuthToken: "tok", MCPConfig: "/etc/dabberz/mcp.json"})

	res, err := r.Run(context.Background(), Request{
		InstanceID: "vm_1",
		Prompt:     "add star ratings",
		Env:        map[string]string{"DATABASE_URL": "postgres://x"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := strings.Join(driver.lastCmd.Argv, " ")
	if !strings.Contains(argv, "-p") || !strings.Contains(argv, "--output-format json") {
		t.Fatalf("argv = %q, want non-interactive print mode with JSON output", argv)
	}
	if !strings.Contains(argv, "--mcp-config /etc/dabberz/mcp.json") {
		t.Fatalf("argv = %q, want the dabberz MCP toolset attached", argv)
	}
	// Long prompts go over stdin, not argv.
	if string(driver.lastCmd.Stdin) != "add star ratings" {
		t.Fatalf("prompt was not sent over stdin: %q", driver.lastCmd.Stdin)
	}
	// The token is injected per run, alongside the repo's inherited secrets.
	if driver.lastCmd.Env[TokenEnv] != "tok" {
		t.Fatalf("auth token not injected: %v", driver.lastCmd.Env)
	}
	if driver.lastCmd.Env["DATABASE_URL"] != "postgres://x" {
		t.Fatalf("repo secrets were not passed through: %v", driver.lastCmd.Env)
	}

	if res.Output != "done" || res.Failed {
		t.Fatalf("result = %+v", res)
	}
	// Usage feeds the tripwire directly, so it has to come through intact.
	if res.Usage.InputTokens != 1000 || res.Usage.OutputTokens != 200 || res.Usage.CostUSD != 0.42 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if res.Usage.Wall <= 0 {
		t.Fatal("wall-clock time was not recorded")
	}
}

func TestRunCountsCacheTokensTowardsTheBudget(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"type": "result", "result": "ok",
		"usage": map[string]any{
			"input_tokens": 100, "output_tokens": 50,
			"cache_creation_input_tokens": 300, "cache_read_input_tokens": 600,
		},
	})
	driver := &fakeDriver{result: &vm.ExecResult{Stdout: string(body)}}

	res, err := New(driver, Config{}).Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "p"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Cached tokens are still tokens spent; leaving them out would let a fork
	// run far past its budget.
	if res.Usage.InputTokens != 1000 {
		t.Fatalf("input tokens = %d, want cache tokens counted in", res.Usage.InputTokens)
	}
}

func TestRunTreatsAgentFailureAsAResultNotAnError(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{
		Stdout:   claudeJSON("could not build the project", true, 10, 5, 0.01),
		ExitCode: 1,
	}}

	// The fix loop has to be able to read the failure and feed it back, so a
	// failing agent must not come back as a Go error.
	res, err := New(driver, Config{}).Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "p"})
	if err != nil {
		t.Fatalf("Run returned an error for an agent failure: %v", err)
	}
	if !res.Failed {
		t.Fatal("the result should be marked failed")
	}
	if res.Output != "could not build the project" {
		t.Fatalf("output = %q", res.Output)
	}
}

func TestRunReportsDriverFailuresAsErrors(t *testing.T) {
	driver := &fakeDriver{err: vm.ErrNotFound}
	// Being unable to reach the VM at all is a dabberz problem, not an agent
	// result.
	if _, err := New(driver, Config{}).Run(context.Background(), Request{InstanceID: "vm_gone", Prompt: "p"}); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("Run = %v, want the driver error", err)
	}
}

func TestRunHandlesTimeout(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{TimedOut: true, ExitCode: -1}}
	res, err := New(driver, Config{Timeout: time.Minute}).Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "p"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Output, "still running") {
		t.Fatalf("a timed-out run should be reported clearly: %+v", res)
	}
}

func TestRunParsesResultAmongProgressOutput(t *testing.T) {
	stdout := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":"working"}`,
		claudeJSON("finished", false, 20, 10, 0.02),
	}, "\n")
	driver := &fakeDriver{result: &vm.ExecResult{Stdout: stdout}}

	res, err := New(driver, Config{}).Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "p"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "finished" {
		t.Fatalf("output = %q, want the final result object", res.Output)
	}
}

func TestUnparseableOutputIsMarkedUnaccounted(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{Stdout: "claude: command not found", ExitCode: 127}}
	res, err := New(driver, Config{}).Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "p"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Zero usage from an unparseable run means unaccounted, not free. Without
	// this flag the cost and token tripwires would silently stop counting.
	if res.UsageReported {
		t.Fatal("usage should not be reported as accounted when nothing could be parsed")
	}
	if res.Usage.CostUSD != 0 {
		t.Fatalf("cost = %v, want zero for an unaccounted run", res.Usage.CostUSD)
	}
}

func TestParsedRunIsMarkedAccounted(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{Stdout: claudeJSON("done", false, 10, 5, 0.25)}}
	res, err := New(driver, Config{}).Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "p"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.UsageReported {
		t.Fatal("a parsed result should count as accounted")
	}
}

func TestRunFallsBackToRawOutput(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{
		Stdout:   "claude: command not found",
		ExitCode: 127,
	}}
	res, err := New(driver, Config{}).Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "p"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Unparseable output must still be legible on the activity feed.
	if !strings.Contains(res.Output, "command not found") || !res.Failed {
		t.Fatalf("result = %+v", res)
	}
}

func TestEscalationMarkerIsRecognised(t *testing.T) {
	tests := map[string]domain.EscalationKind{
		"DABBERZ-ESCALATE[ambiguous]: no clear direction on the rating scale":            domain.EscalationAmbiguous,
		"DABBERZ-ESCALATE[conflicts_instructions]: the only fix breaks the API contract": domain.EscalationConflictsInstructions,
	}
	for marker, wantKind := range tests {
		driver := &fakeDriver{result: &vm.ExecResult{Stdout: claudeJSON("stopped\n"+marker, false, 1, 1, 0)}}
		res, err := New(driver, Config{}).Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "p"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Escalation == nil {
			t.Fatalf("no escalation parsed from %q", marker)
		}
		if res.Escalation.Kind != wantKind {
			t.Errorf("kind = %q, want %q", res.Escalation.Kind, wantKind)
		}
		if res.Escalation.Message == "" {
			t.Error("escalation message is empty")
		}
	}
}

func TestOrdinaryOutputIsNotMistakenForAnEscalation(t *testing.T) {
	// An agent discussing escalation must not accidentally trigger one.
	driver := &fakeDriver{result: &vm.ExecResult{
		Stdout: claudeJSON("I considered whether to emit DABBERZ-ESCALATE[ambiguous] but decided to continue.", false, 1, 1, 0),
	}}
	res, err := New(driver, Config{}).Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "p"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Escalation != nil {
		t.Fatalf("prose mentioning the marker triggered an escalation: %+v", res.Escalation)
	}
}

func TestRateLimitsAreDetected(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{
		Stderr:   "Error: 429 rate limit exceeded, retry later",
		ExitCode: 1,
	}}
	res, err := New(driver, Config{}).Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "p"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// There is no artificial concurrency cap on coding agents, so hitting a
	// real rate limit has to be recognisable when it happens.
	if !res.RateLimited {
		t.Fatal("a 429 should be recognised as a rate limit")
	}
}

func TestRunValidatesItsInput(t *testing.T) {
	r := New(&fakeDriver{result: &vm.ExecResult{}}, Config{})
	if _, err := r.Run(context.Background(), Request{Prompt: "p"}); err == nil {
		t.Error("a run without an instance should be rejected")
	}
	if _, err := r.Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "  "}); err == nil {
		t.Error("a run without a prompt should be rejected")
	}
}

func TestBuildPromptStatesTheParallelModelAndEscalationProtocol(t *testing.T) {
	fork := &domain.Fork{Name: "photo-upload", Description: "add photo upload", Branch: "dabberz/photo-upload-abc123"}
	repo := &domain.Repo{Name: "devex"}
	project := &domain.Project{Name: "web", Path: "apps/web"}

	services := []*domain.Service{
		{Name: "db", Kind: domain.ServiceDatastore, Command: "postgres -D data", Port: 5432},
		{Name: "api", Kind: domain.ServiceAPI, Command: "go run ./cmd/api", Port: 8080, Exposed: true, DependsOn: []string{"db"}},
		{Name: "web", Kind: domain.ServiceWeb, Command: "npm run dev", Port: 5173, Exposed: true, Primary: true, DependsOn: []string{"api"}},
	}
	previews := Previews{
		Primary: "https://web-web-photo-upload.dab.im",
		URLs: map[string]string{
			"web": "https://web-web-photo-upload.dab.im",
			"api": "https://api-web-photo-upload.dab.im",
		},
	}

	prompt := BuildPrompt(fork, repo, project, services, previews, "/srv/memory/repo_1")

	for _, want := range []string{
		"photo-upload",
		"dabberz/photo-upload-abc123",
		"https://web-web-photo-upload.dab.im",
		// Every service, so the agent does not read a missing worker or
		// database as a bug in the code it is changing.
		"api (api) on port 8080",
		"db (datastore) on port 5432, internal only",
		"needs api running first",
		"start with: npm run dev",
		"/srv/memory/repo_1",
		"DABBERZ-ESCALATE[ambiguous]",
		"DABBERZ-ESCALATE[conflicts_instructions]",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	// Agents must not try to coordinate with siblings they cannot see.
	if !strings.Contains(prompt, "cannot see them") {
		t.Error("the prompt should say sibling workstreams are invisible")
	}
}

func TestBuildFixPromptCarriesTheReport(t *testing.T) {
	fork := &domain.Fork{Name: "ratings"}
	prompt := BuildFixPrompt(fork, "The star widget does not render on mobile.", 2)

	if !strings.Contains(prompt, "star widget does not render") {
		t.Error("the verification report should reach the agent verbatim")
	}
	if !strings.Contains(prompt, "round 2") {
		t.Error("the fix round should be stated")
	}
	// A report must not be able to override the user's instructions.
	if !strings.Contains(prompt, "not as a specification") {
		t.Error("the prompt should tell the agent the report is evidence, not a spec")
	}
}

// scriptedDriver returns a different result per call, so a resume followed by
// a fallback can be exercised.
type scriptedDriver struct {
	results []*vm.ExecResult
	calls   []vm.Command
}

func (d *scriptedDriver) Name() string                                  { return "scripted" }
func (d *scriptedDriver) Capacity(context.Context) (vm.Capacity, error) { return vm.Capacity{}, nil }
func (d *scriptedDriver) Create(context.Context, vm.Spec) (*vm.Instance, error) {
	return nil, vm.ErrNotSupported
}
func (d *scriptedDriver) Get(context.Context, string) (*vm.Instance, error) {
	return nil, vm.ErrNotFound
}
func (d *scriptedDriver) List(context.Context) ([]*vm.Instance, error) { return nil, nil }
func (d *scriptedDriver) Destroy(context.Context, string) error        { return nil }

func (d *scriptedDriver) Exec(_ context.Context, _ string, cmd vm.Command) (*vm.ExecResult, error) {
	d.calls = append(d.calls, cmd)
	if len(d.calls) > len(d.results) {
		return d.results[len(d.results)-1], nil
	}
	return d.results[len(d.calls)-1], nil
}

func argvOf(cmd vm.Command) string { return strings.Join(cmd.Argv, " ") }

func TestFirstRunStartsAConversationAndRemembersIt(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{Stdout: claudeJSON("done", false, 1000, 200, 0.42)}}
	r := New(driver, Config{})
	session := &domain.AgentSession{}

	res, err := r.Run(context.Background(), Request{
		InstanceID: "vm_1", Prompt: "add ratings", Session: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(argvOf(driver.lastCmd), "--resume") {
		t.Fatalf("the first run tried to resume: %s", argvOf(driver.lastCmd))
	}
	if res.Resumed {
		t.Fatal("the first run reported itself resumed")
	}
	if session.ID != "s1" {
		t.Fatalf("session id = %q, want the one the agent reported", session.ID)
	}
	if res.Usage.CostUSD != 0.42 {
		t.Fatalf("cost = %v, want the reported 0.42", res.Usage.CostUSD)
	}
}

// TestSecondRunResumesAndChargesOnlyTheDifference is the whole point: without
// it a fix round re-reads the workstream from scratch, and the reported total
// would be charged again in full.
func TestSecondRunResumesAndChargesOnlyTheDifference(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{Stdout: claudeJSON("first", false, 1000, 200, 1.00)}}
	r := New(driver, Config{})
	session := &domain.AgentSession{}

	if _, err := r.Run(context.Background(), Request{
		InstanceID: "vm_1", Prompt: "add ratings", Session: session,
	}); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// The conversation now totals $2.50 and 1800 input tokens.
	driver.result = &vm.ExecResult{Stdout: claudeJSON("second", false, 1800, 400, 2.50)}
	res, err := r.Run(context.Background(), Request{
		InstanceID: "vm_1", Prompt: "fix the thing", Session: session,
	})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if !strings.Contains(argvOf(driver.lastCmd), "--resume s1") {
		t.Fatalf("the second run did not resume: %s", argvOf(driver.lastCmd))
	}
	if !res.Resumed {
		t.Fatal("the second run did not report itself resumed")
	}
	if res.Usage.CostUSD != 1.50 {
		t.Fatalf("charged %v for the second run, want the 1.50 difference", res.Usage.CostUSD)
	}
	if res.Usage.InputTokens != 800 || res.Usage.OutputTokens != 200 {
		t.Fatalf("charged %d/%d tokens, want the 800/200 difference",
			res.Usage.InputTokens, res.Usage.OutputTokens)
	}
}

// TestALostConversationFallsBackToAFreshOne: a machine can be rebuilt, or the
// session store not persisted across a restart. Losing context is a setback;
// failing the fork for it is not warranted when starting fresh is exactly what
// would have happened before resuming existed.
func TestALostConversationFallsBackToAFreshOne(t *testing.T) {
	driver := &scriptedDriver{results: []*vm.ExecResult{
		{ExitCode: 1, Stderr: "Error: No conversation found with session ID: s1"},
		{Stdout: claudeJSON("done from scratch", false, 900, 150, 0.30)},
	}}
	r := New(driver, Config{})
	session := &domain.AgentSession{ID: "s1", Accounted: domain.Usage{CostUSD: 5, InputTokens: 4000}}

	res, err := r.Run(context.Background(), Request{
		InstanceID: "vm_1", Prompt: "fix the thing", Session: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(driver.calls) != 2 {
		t.Fatalf("made %d calls, want a resume then a fresh run", len(driver.calls))
	}
	if !strings.Contains(argvOf(driver.calls[0]), "--resume s1") {
		t.Fatalf("the first attempt did not resume: %s", argvOf(driver.calls[0]))
	}
	if strings.Contains(argvOf(driver.calls[1]), "--resume") {
		t.Fatalf("the fallback still tried to resume: %s", argvOf(driver.calls[1]))
	}
	if res.Failed {
		t.Fatal("a lost conversation failed the run instead of starting fresh")
	}
	if res.Resumed {
		t.Fatal("the run reported itself resumed when it had started fresh")
	}
	// The stale accounting went with the conversation; charging the
	// difference against it would have produced a negative turn.
	if res.Usage.CostUSD != 0.30 {
		t.Fatalf("charged %v, want the fresh conversation's 0.30", res.Usage.CostUSD)
	}
}

// TestAFailedRunIsNotMistakenForALostConversation: the fallback must not
// re-run work that genuinely failed, which would do it twice.
func TestAFailedRunIsNotMistakenForALostConversation(t *testing.T) {
	driver := &scriptedDriver{results: []*vm.ExecResult{
		{ExitCode: 1, Stderr: "the build failed: undefined symbol"},
	}}
	r := New(driver, Config{})
	session := &domain.AgentSession{ID: "s1"}

	res, err := r.Run(context.Background(), Request{
		InstanceID: "vm_1", Prompt: "fix the thing", Session: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(driver.calls) != 1 {
		t.Fatalf("made %d calls; a genuine failure was retried", len(driver.calls))
	}
	if !res.Failed {
		t.Fatal("a failing run was not reported as failed")
	}
}

func TestRunWithoutASessionStillWorks(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{Stdout: claudeJSON("done", false, 100, 20, 0.05)}}
	r := New(driver, Config{})

	// Callers that do not keep a conversation -- the merge reviewer, for one
	// -- pass none, and must get the reported figures unchanged.
	res, err := r.Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "review"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Usage.CostUSD != 0.05 {
		t.Fatalf("cost = %v, want the reported 0.05", res.Usage.CostUSD)
	}
}

// TestSessionDirIsPassedToTheAgent: the conversation is read back from this
// directory, so it has to be on storage that outlives a restart. Without it a
// lost conversation shows up only as a resume quietly falling back to a fresh
// start -- the behaviour resuming was added to replace.
func TestSessionDirIsPassedToTheAgent(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{Stdout: claudeJSON("done", false, 10, 5, 0.01)}}
	r := New(driver, Config{SessionDir: "/workspace/.dabberz/agent"})

	if _, err := r.Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "go"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := driver.lastCmd.Env[ConfigDirEnv]; got != "/workspace/.dabberz/agent" {
		t.Fatalf("%s = %q, want the configured directory", ConfigDirEnv, got)
	}
}

func TestSessionDirIsOmittedWhenUnset(t *testing.T) {
	driver := &fakeDriver{result: &vm.ExecResult{Stdout: claudeJSON("done", false, 10, 5, 0.01)}}
	r := New(driver, Config{})

	if _, err := r.Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "go"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Leaving the agent's own default alone is correct when the machine's
	// home directory is itself persistent.
	if _, set := driver.lastCmd.Env[ConfigDirEnv]; set {
		t.Fatalf("%s was set without being configured", ConfigDirEnv)
	}
}
