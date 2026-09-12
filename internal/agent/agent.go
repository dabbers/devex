// Package agent runs the coding agent inside a fork's VM.
//
// The coding agent is Claude Code, fixed for v1: keeping one execution layer
// avoids passing state back and forth between two agent frameworks. dabberz
// drives it in print mode over the VM driver, so the same code path works
// whether the fork is a real microVM or a local development workspace.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/vm"
)

// DefaultTimeout bounds a single agent run.
const DefaultTimeout = 45 * time.Minute

// TokenEnv is the environment variable Claude Code authenticates with. It is
// injected into the fork VM alongside the repo's inherited secrets.
const TokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// escalationPattern is how an agent asks for the user without dabberz having
// to infer intent from prose. The agent is told to emit this marker when it
// hits something it must not decide alone.
var escalationPattern = regexp.MustCompile(`(?m)^DABBERZ-ESCALATE\[(conflicts_instructions|ambiguous)\]:\s*(.+)$`)

// Config configures the agent runner.
type Config struct {
	// Binary is the Claude Code executable inside the VM.
	Binary string `yaml:"binary" json:"binary"`
	// OAuthToken authenticates Claude Code. It is passed per-run rather than
	// baked into the golden image so it can be rotated without a rebuild.
	OAuthToken string `yaml:"oauth_token" json:"oauth_token"`
	// MCPConfig is the path, inside the VM, of the dabberz MCP server config
	// that gives the agent secrets, preview control and browser driving.
	MCPConfig string `yaml:"mcp_config" json:"mcp_config"`
	// Timeout bounds one run.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
	// ExtraArgs are appended to every invocation.
	ExtraArgs []string `yaml:"extra_args" json:"extra_args"`
}

func (c Config) withDefaults() Config {
	if c.Binary == "" {
		c.Binary = "claude"
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	return c
}

// Runner drives the coding agent inside VMs.
type Runner struct {
	driver vm.Driver
	cfg    Config
}

// New returns a runner using driver to reach fork VMs.
func New(driver vm.Driver, cfg Config) *Runner {
	return &Runner{driver: driver, cfg: cfg.withDefaults()}
}

// Request is one agent invocation.
type Request struct {
	// InstanceID is the fork's VM.
	InstanceID string
	// Prompt is the instruction: the workstream description on the first run,
	// or a verification report on a fix run.
	Prompt string
	// Dir is the working directory inside the VM. Empty means the workspace.
	Dir string
	// Env adds to the VM's environment for this run, carrying repo secrets.
	Env map[string]string
	// Timeout overrides the configured timeout.
	Timeout time.Duration
}

// Result is the outcome of an agent run.
type Result struct {
	// Output is the agent's final message.
	Output string `json:"output"`
	// Usage is what the run cost, fed straight into the tripwire.
	Usage domain.Usage `json:"usage"`
	// Failed reports that the agent itself reported an error.
	Failed bool `json:"failed"`
	// Escalation is set when the agent asked for the user rather than
	// deciding alone.
	Escalation *Escalation `json:"escalation,omitempty"`
	// RateLimited reports an upstream rate limit, which is surfaced as an
	// error and handled like any other pause rather than pre-empted with a
	// concurrency cap.
	RateLimited bool `json:"rate_limited"`
	// Raw is the agent's unparsed output, kept for the activity feed.
	Raw string `json:"-"`
}

// Escalation is a request for user input raised by the agent.
type Escalation struct {
	Kind    domain.EscalationKind `json:"kind"`
	Message string                `json:"message"`
}

// claudeResult is the JSON Claude Code emits in print mode.
type claudeResult struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	SessionID    string  `json:"session_id"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// Run invokes the coding agent and returns what it did.
//
// A non-zero exit or an agent-reported error is a Result, not a Go error: the
// verify/fix loop needs to read the failure and feed it back. A Go error means
// dabberz could not run the agent at all.
func (r *Runner) Run(ctx context.Context, req Request) (*Result, error) {
	if req.InstanceID == "" {
		return nil, fmt.Errorf("agent: a VM instance is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("agent: a prompt is required")
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.cfg.Timeout
	}

	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	if r.cfg.OAuthToken != "" {
		env[TokenEnv] = r.cfg.OAuthToken
	}

	started := time.Now()
	exec, err := r.driver.Exec(ctx, req.InstanceID, vm.Command{
		Argv: r.argv(),
		Dir:  req.Dir,
		Env:  env,
		// The prompt goes over stdin rather than the command line: workstream
		// descriptions and verification reports are long and contain
		// characters no argv should have to carry.
		Stdin:   []byte(req.Prompt),
		Timeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: run in %s: %w", req.InstanceID, err)
	}

	result := &Result{
		Raw:   exec.Stdout,
		Usage: domain.Usage{Wall: time.Since(started)},
	}
	if exec.TimedOut {
		result.Failed = true
		result.Output = fmt.Sprintf("the coding agent was still running after %s and was stopped", timeout)
		return result, nil
	}

	parsed := parseResult(exec.Stdout)
	if parsed != nil {
		result.Output = parsed.Result
		result.Failed = parsed.IsError
		result.Usage.InputTokens = parsed.Usage.InputTokens + parsed.Usage.CacheCreationInputTokens + parsed.Usage.CacheReadInputTokens
		result.Usage.OutputTokens = parsed.Usage.OutputTokens
		result.Usage.CostUSD = parsed.TotalCostUSD
	} else {
		// No parseable result: fall back to the raw streams so the failure is
		// still legible in the activity feed.
		result.Output = strings.TrimSpace(exec.Stdout)
		if result.Output == "" {
			result.Output = strings.TrimSpace(exec.Stderr)
		}
	}
	if !exec.OK() {
		result.Failed = true
	}

	// The escalation marker is part of the agent's final message, so it must be
	// read from the decoded result: in the raw stream it is JSON-escaped and
	// never appears at the start of a line.
	result.Escalation = parseEscalation(result.Output + "\n" + exec.Stderr)
	// Rate limits can surface anywhere, including before any result is
	// emitted, so those are matched against everything the run produced.
	result.RateLimited = looksRateLimited(exec.Stdout + "\n" + exec.Stderr)
	return result, nil
}

// argv builds the Claude Code invocation.
func (r *Runner) argv() []string {
	argv := []string{
		r.cfg.Binary,
		// Print mode: run to completion non-interactively and emit a machine
		// readable result rather than a TUI.
		"-p",
		"--output-format", "json",
	}
	if r.cfg.MCPConfig != "" {
		argv = append(argv, "--mcp-config", r.cfg.MCPConfig)
	}
	return append(argv, r.cfg.ExtraArgs...)
}

// parseResult extracts Claude Code's JSON result from its output, tolerating
// leading log lines.
func parseResult(stdout string) *claudeResult {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return nil
	}

	var direct claudeResult
	if err := json.Unmarshal([]byte(trimmed), &direct); err == nil && direct.Type != "" {
		return &direct
	}

	// Scan backwards: the result object is the last thing emitted, and earlier
	// lines may be progress output.
	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var candidate claudeResult
		if err := json.Unmarshal([]byte(line), &candidate); err == nil && candidate.Type == "result" {
			return &candidate
		}
	}
	return nil
}

// parseEscalation finds an escalation marker in the agent's output.
func parseEscalation(output string) *Escalation {
	match := escalationPattern.FindStringSubmatch(output)
	if match == nil {
		return nil
	}
	return &Escalation{
		Kind:    domain.EscalationKind(match[1]),
		Message: strings.TrimSpace(match[2]),
	}
}

// rateLimitHints are the phrasings an upstream rate limit arrives with.
var rateLimitHints = []string{
	"rate limit",
	"rate_limit",
	"429",
	"usage limit reached",
	"too many requests",
}

func looksRateLimited(output string) bool {
	lower := strings.ToLower(output)
	for _, hint := range rateLimitHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}
