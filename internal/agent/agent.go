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

	"github.com/dabbers/devex/internal/agentauth"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/vm"
)

// DefaultTimeout bounds a single agent run.
const DefaultTimeout = 45 * time.Minute

// DefaultPermissionMode lets the agent act without asking.
//
// Chosen rather than inherited: the agent's own default is to ask, and there
// is nobody to ask. See Config.PermissionMode for what makes it safe, which is
// the isolation and the promote gate rather than anything about the flag.
const DefaultPermissionMode = "bypassPermissions"

// TokenEnv is the environment variable Claude Code authenticates with. It is
// injected into the run alongside the repo's inherited secrets, and always
// from the account the run belongs to -- see Credentials.
const TokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// Credentials resolves the subscription credential a run is driven by.
//
// Per account, because the customer brings the subscription: a run in one
// customer's workspace must authenticate as that customer and nobody else.
// internal/agentauth is the implementation; an error -- including an account
// that has stored nothing -- refuses the run rather than letting it start
// unauthenticated or on somebody else's credential.
type Credentials interface {
	AgentCredential(ctx context.Context, userID string) (agentauth.Credential, error)
}

// Provider is the subscription this runner drives. Claude Code is the
// execution layer for v1; a credential for any other provider is refused
// here rather than handed to a binary that cannot use it.
const Provider = domain.ProviderClaude

// AuthorName and AuthorEmail are who the agent's commits are by.
const (
	AuthorName  = "dabberz agent"
	AuthorEmail = "agent@dabberz.invalid"
)

// ConfigDirEnv points the agent at where it keeps its conversations.
//
// Resuming reads the conversation back from this directory, so it has to be
// somewhere that survives. Left to its default it lands under the home
// directory, which on a rebuilt machine is gone -- and a lost conversation is
// only noticed as a resume that quietly falls back to starting fresh, which
// is exactly the behaviour resuming was added to replace.
const ConfigDirEnv = "CLAUDE_CONFIG_DIR"

// escalationPattern is how an agent asks for the user without dabberz having
// to infer intent from prose. The agent is told to emit this marker when it
// hits something it must not decide alone.
var escalationPattern = regexp.MustCompile(`(?m)^DABBERZ-ESCALATE\[(conflicts_instructions|ambiguous)\]:\s*(.+)$`)

// Config configures the agent runner.
type Config struct {
	// Binary is the Claude Code executable inside the VM.
	Binary string `yaml:"binary" json:"binary"`
	// MCPConfig is the path, inside the VM, of the dabberz MCP server config
	// that gives the agent secrets, preview control and browser driving.
	MCPConfig string `yaml:"mcp_config" json:"mcp_config"`
	// Timeout bounds one run.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
	// PermissionMode is how the agent asks before acting.
	//
	// It has a default, and the default is permissive, because the alternative
	// is a product that does not work: a run with nothing set plans the change
	// correctly, reports that its commands were not approved, and changes
	// nothing -- there is no human at a terminal to approve them, and if there
	// were, approving each edit is not the thing being sold.
	//
	// What makes that safe is not the flag, it is where the run happens. The
	// agent works on its own machine, on a branch, against a copy of the data
	// forked a moment earlier, behind a preview nobody depends on, and a
	// person promotes. Those are the controls. A deployment whose driver does
	// not actually isolate -- the local one -- is giving the agent its host,
	// and says so at startup.
	PermissionMode string `yaml:"permission_mode" json:"permission_mode"`
	// ExtraArgs are appended to every invocation.
	ExtraArgs []string `yaml:"extra_args" json:"extra_args"`
	// SessionDir is where the agent keeps its conversations inside the VM. It
	// must be on storage that outlives a restart, or resuming silently
	// degrades to starting fresh every time.
	//
	// Empty leaves the agent's own default, which is correct only when the
	// machine's home directory is itself persistent.
	SessionDir string `yaml:"session_dir" json:"session_dir"`
}

func (c Config) withDefaults() Config {
	if c.Binary == "" {
		c.Binary = "claude"
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.PermissionMode == "" {
		c.PermissionMode = DefaultPermissionMode
	}
	return c
}

// Runner drives the coding agent inside VMs.
type Runner struct {
	driver vm.Driver
	creds  Credentials
	cfg    Config
}

// New returns a runner using driver to reach fork VMs and creds to decide
// whose Claude token each run uses.
//
// The credentials are an argument rather than an optional field so that a
// caller cannot build a runner without saying where tokens come from. A nil
// one is accepted and refuses every run, which is loud in the right place.
func New(driver vm.Driver, creds Credentials, cfg Config) *Runner {
	return &Runner{driver: driver, creds: creds, cfg: cfg.withDefaults()}
}

// Request is one agent invocation.
type Request struct {
	// UserID is the account the run belongs to: the owner of the workspace or
	// fork it is working in. It decides whose Claude token pays for the run,
	// and is required.
	UserID string
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
	// OnText receives the agent's answer as it is written, when the driver can
	// stream and the caller wants to show progress. Each call carries
	// everything written so far, not the increment: a consumer that renders it
	// does not have to accumulate, and one that misses a call is not left with
	// a hole in the middle of a sentence.
	//
	// It is called from the goroutine reading the command and must return
	// quickly -- a slow consumer stalls the agent's output.
	OnText func(text string)
	// Session carries the conversation across runs. When it names one, the
	// agent resumes it instead of starting from an empty context and
	// rebuilding its understanding from the prompt.
	//
	// It is mutated by Run: the conversation's identifier and what has been
	// accounted for it both belong to the caller, who persists them.
	Session *domain.AgentSession
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
	// UsageReported says whether the agent accounted for what the run cost.
	// When it is false the Usage figures are zero because nothing could be
	// parsed, not because the run was free -- and the cost and token
	// tripwires are therefore not counting this run. Callers surface that
	// rather than letting a budget silently stop accruing.
	UsageReported bool `json:"usage_reported"`
	// Resumed reports whether this run continued an existing conversation.
	Resumed bool `json:"resumed"`
	// SessionID is the conversation the run belonged to.
	SessionID string `json:"session_id,omitempty"`
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
	token, err := r.token(ctx, req.UserID)
	if err != nil {
		return nil, err
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.cfg.Timeout
	}

	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	// Set after the repo's secrets, so it always wins. A repo secret of the
	// same name would otherwise decide whose subscription pays, and a repo
	// secret is reachable by every service the repo runs, which is not where
	// a customer's Claude credential belongs.
	env[TokenEnv] = token
	if r.cfg.SessionDir != "" {
		env[ConfigDirEnv] = r.cfg.SessionDir
	}
	// The agent is told to commit as it goes, and git will not commit without
	// an identity. It used to borrow one from whatever account ran it -- root's
	// global config -- and running it as an account with a fresh home took
	// that away: the first real turn on dbz-alpha made its change and then
	// reported it could not commit. Named for what it is, so the history says
	// the agent wrote this rather than whoever owns the machine. A repo secret
	// setting these still wins.
	for name, value := range map[string]string{
		"GIT_AUTHOR_NAME": AuthorName, "GIT_AUTHOR_EMAIL": AuthorEmail,
		"GIT_COMMITTER_NAME": AuthorName, "GIT_COMMITTER_EMAIL": AuthorEmail,
	} {
		if _, set := env[name]; !set {
			env[name] = value
		}
	}

	resumeID := ""
	if req.Session.Started() {
		resumeID = req.Session.ID
	}
	// Streaming is only worth asking for when something is watching and the
	// driver can deliver it. Otherwise print mode stays, which is one result
	// object instead of a few thousand lines.
	streamer, canStream := r.driver.(vm.Streaming)
	streaming := canStream && req.OnText != nil

	started := time.Now()
	exec, err := r.invoke(ctx, req, env, resumeID, timeout, streaming, streamer)
	if err != nil {
		return nil, err
	}

	// A conversation can be gone: the machine was rebuilt, or the session
	// store was not persisted across a restart. Losing context is a setback;
	// failing the fork for it is not warranted when starting fresh is exactly
	// what would have happened before resuming existed.
	resumed := resumeID != ""
	if resumed && resumeFailed(exec) {
		resumed = false
		req.Session.ID = ""
		req.Session.Accounted = domain.Usage{}
		exec, err = r.invoke(ctx, req, env, "", timeout, streaming, streamer)
		if err != nil {
			return nil, err
		}
	}

	result := &Result{
		Raw:     exec.Stdout,
		Usage:   domain.Usage{Wall: time.Since(started)},
		Resumed: resumed,
	}
	if exec.TimedOut {
		result.Failed = true
		result.Output = fmt.Sprintf("the coding agent was still running after %s and was stopped", timeout)
		return result, nil
	}

	parsed := parseResult(exec.Stdout)
	if parsed != nil {
		result.UsageReported = true
		result.Output = parsed.Result
		result.Failed = parsed.IsError
		result.SessionID = parsed.SessionID

		// What the agent reports is the conversation's total, not this run's,
		// so the session turns it into the difference and remembers the new
		// total. Adding the reported figure directly would count every
		// earlier turn again on every resumed run.
		reported := domain.Usage{
			InputTokens:  parsed.Usage.InputTokens + parsed.Usage.CacheCreationInputTokens + parsed.Usage.CacheReadInputTokens,
			OutputTokens: parsed.Usage.OutputTokens,
			CostUSD:      parsed.TotalCostUSD,
			Wall:         result.Usage.Wall,
		}
		if req.Session != nil {
			result.Usage = req.Session.Turn(parsed.SessionID, reported)
		} else {
			result.Usage = reported
		}
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

// token resolves the run's Claude token, refusing a run that names no
// account or whose account has none.
//
// The error never carries the token, and is phrased for the person whose run
// was refused: it surfaces in their conversation or on their fork.
func (r *Runner) token(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", fmt.Errorf("agent: a run must name the account it belongs to")
	}
	if r.creds == nil {
		return "", fmt.Errorf("agent: this deployment has no source of agent credentials, so no agent can run")
	}
	credential, err := r.creds.AgentCredential(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("agent: %w", err)
	}
	// A Codex or Grok credential is the customer's real choice and is kept,
	// but this runner is Claude Code: passing it as a Claude token would fail
	// confusingly at best, and is not what they asked for.
	if credential.Provider != Provider {
		return "", fmt.Errorf("agent: %w", agentauth.Unsupported(credential.Provider))
	}
	token := credential.Secret
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("agent: the Claude token for this account is empty, so the agent cannot run")
	}
	return token, nil
}

// invoke runs the agent once, optionally resuming a conversation.
func (r *Runner) invoke(ctx context.Context, req Request, env map[string]string, resumeID string, timeout time.Duration, streaming bool, streamer vm.Streaming) (*vm.ExecResult, error) {
	cmd := vm.Command{
		Argv: r.argv(resumeID, streaming),
		Dir:  req.Dir,
		Env:  env,
		// The prompt goes over stdin rather than the command line: workstream
		// descriptions and verification reports are long and contain
		// characters no argv should have to carry.
		Stdin:   []byte(req.Prompt),
		Timeout: timeout,
	}

	var (
		exec *vm.ExecResult
		err  error
	)
	if streaming {
		reader := &streamReader{}
		exec, err = streamer.ExecStream(ctx, req.InstanceID, cmd, func(line []byte) {
			if reader.push(line) {
				req.OnText(reader.text())
			}
		})
	} else {
		exec, err = r.driver.Exec(ctx, req.InstanceID, cmd)
	}
	if err != nil {
		return nil, fmt.Errorf("agent: run in %s: %w", req.InstanceID, err)
	}
	return exec, nil
}

// argv builds the Claude Code invocation.
//
// The conversation identifier is read back from the result rather than chosen
// here. Choosing it would need the installed version to accept --session-id,
// and reading it back works with any version that can resume at all: if none
// comes back, each run simply starts fresh, which is what happened before.
func (r *Runner) argv(resumeID string, streaming bool) []string {
	argv := []string{
		r.cfg.Binary,
		// Print mode: run non-interactively and emit a machine readable
		// result rather than a TUI.
		"-p",
	}
	if streaming {
		// One JSON object per line as the run proceeds, the last of which is
		// the result print mode would have returned. --verbose is required
		// alongside it, and partial messages are what make the answer appear
		// as it is written rather than in finished paragraphs.
		argv = append(argv, "--output-format", "stream-json", "--verbose", "--include-partial-messages")
	} else {
		argv = append(argv, "--output-format", "json")
	}
	if resumeID != "" {
		// Continue the conversation instead of starting from an empty context
		// and rebuilding understanding from the prompt every cycle.
		argv = append(argv, "--resume", resumeID)
	}
	if r.cfg.MCPConfig != "" {
		argv = append(argv, "--mcp-config", r.cfg.MCPConfig)
	}
	if r.cfg.PermissionMode != "" {
		// Nothing is watching this run, so a prompt is a hang that reports
		// itself as a refusal. See Config.PermissionMode.
		argv = append(argv, "--permission-mode", r.cfg.PermissionMode)
	}
	return append(argv, r.cfg.ExtraArgs...)
}

// resumeFailedPattern recognises a conversation the agent cannot find.
var resumeFailedPattern = regexp.MustCompile(`(?i)(no (conversation|session) found|session .* not found|could not (find|resume) session|unknown session)`)

// resumeFailed reports whether a run failed because the conversation is gone,
// rather than because the work itself failed.
func resumeFailed(exec *vm.ExecResult) bool {
	if exec.OK() {
		return false
	}
	// Only the streams, not the parsed result: a conversation that cannot be
	// found produces no result object to parse.
	return resumeFailedPattern.MatchString(exec.Stderr) || resumeFailedPattern.MatchString(exec.Stdout)
}

// parseResult extracts Claude Code's JSON result from its output, tolerating
// leading log lines.
func parseResult(stdout string) *claudeResult {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return nil
	}

	// Only a result object counts. Stream mode emits many typed objects, and
	// accepting any of them would read an assistant turn as the result --
	// which carries no cost, so the run would silently look free.
	var direct claudeResult
	if err := json.Unmarshal([]byte(trimmed), &direct); err == nil && direct.Type == "result" {
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
