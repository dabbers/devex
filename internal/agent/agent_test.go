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

	prompt := BuildPrompt(fork, repo, project, "https://preview-web-photo-upload.dab.im", "/srv/memory/repo_1")

	for _, want := range []string{
		"photo-upload",
		"dabberz/photo-upload-abc123",
		"https://preview-web-photo-upload.dab.im",
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
