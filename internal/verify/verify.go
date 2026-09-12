// Package verify checks a fork's work by driving a real browser against its
// live preview.
//
// Two things make this different from a DOM or diff check. The browser is
// real and non-headless, so what is validated is actual behaviour. And the
// preview is reached over the network through the same reverse proxy an
// external user would use, rather than by a co-located shortcut, so routing
// and TLS are exercised too -- a preview that only works from inside the
// machine is not a working preview.
//
// One shared UI VM serves every verifier. It runs a single browser with N
// independently addressable profiles rather than N browsers, so concurrency is
// capped by that VM's capacity. When every profile is busy, jobs queue and
// wait; there is no auto-scaling to a second UI VM in v1.
package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dabbers/devex/internal/scheduler"
	"github.com/dabbers/devex/internal/vm"
)

// DefaultTimeout bounds one verification run.
const DefaultTimeout = 10 * time.Minute

// DefaultProfiles is how many browser profiles the shared UI VM serves.
const DefaultProfiles = 4

// Config configures the verifier.
type Config struct {
	// UIInstanceID is the shared UI VM every verification runs on.
	UIInstanceID string `yaml:"ui_instance_id" json:"ui_instance_id"`
	// Profiles is the number of independently addressable browser profiles the
	// UI VM can drive at once.
	Profiles int `yaml:"profiles" json:"profiles"`
	// DriverCommand is the browser-driving harness inside the UI VM. It is
	// handed a JSON job on stdin and is expected to emit a JSON report.
	DriverCommand []string `yaml:"driver_command" json:"driver_command"`
	// Timeout bounds one run.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
}

func (c Config) withDefaults() Config {
	if c.Profiles <= 0 {
		c.Profiles = DefaultProfiles
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if len(c.DriverCommand) == 0 {
		c.DriverCommand = []string{"dabberz-verify"}
	}
	return c
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if c.UIInstanceID == "" {
		return errors.New("verify: a shared UI VM instance is required")
	}
	return nil
}

// Request is one verification job.
type Request struct {
	ForkID string `json:"fork_id"`
	// PreviewURL is the public URL, reached the way a user would reach it.
	PreviewURL string `json:"preview_url"`
	// Intent is what the workstream was supposed to achieve, so the verifier
	// checks the right behaviour rather than just that the page loads.
	Intent string `json:"intent"`
	// Profile is filled in by the verifier from its pool.
	Profile string `json:"profile"`
}

// Check is one thing the verifier looked at.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
	// Screenshot is a path inside the UI VM, if one was captured.
	Screenshot string `json:"screenshot,omitempty"`
}

// Report is the verifier's verdict.
type Report struct {
	Passed  bool    `json:"passed"`
	Summary string  `json:"summary"`
	Checks  []Check `json:"checks,omitempty"`
	// Profile records which browser profile ran the job, for debugging.
	Profile string `json:"profile,omitempty"`
	// Duration is how long the run took.
	Duration time.Duration `json:"duration"`
}

// FeedbackText renders a report as the text fed back to the coding agent.
func (r *Report) FeedbackText() string {
	var b strings.Builder
	if r.Summary != "" {
		b.WriteString(r.Summary)
		b.WriteString("\n")
	}
	for _, check := range r.Checks {
		if check.Passed {
			continue
		}
		fmt.Fprintf(&b, "\n- FAILED %s", check.Name)
		if check.Detail != "" {
			fmt.Fprintf(&b, ": %s", check.Detail)
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		text = "Verification failed without reporting a reason."
	}
	return text
}

// Verifier runs verification jobs against the shared UI VM.
type Verifier struct {
	driver vm.Driver
	cfg    Config
	// gate caps concurrency at the UI VM's profile count. Jobs beyond that
	// queue and wait, the same policy the fork scheduler applies to machine
	// capacity.
	gate *scheduler.Gate

	mu   sync.Mutex
	free []string
}

// New returns a verifier driving the configured shared UI VM.
func New(driver vm.Driver, cfg Config) (*Verifier, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	gate, err := scheduler.NewGate(cfg.Profiles)
	if err != nil {
		return nil, err
	}

	free := make([]string, 0, cfg.Profiles)
	for i := range cfg.Profiles {
		free = append(free, fmt.Sprintf("verifier-%d", i))
	}
	return &Verifier{driver: driver, cfg: cfg, gate: gate, free: free}, nil
}

// Stats reports how busy the UI VM is, including how many jobs are queued.
func (v *Verifier) Stats() scheduler.Stats { return v.gate.Stats() }

// Verify drives a browser against a fork's preview and reports what it found.
//
// A failing verification is a Report, not an error: the fix loop needs to read
// it and feed it back. An error means the verification could not be run.
func (v *Verifier) Verify(ctx context.Context, req Request) (*Report, error) {
	if req.PreviewURL == "" {
		return nil, fmt.Errorf("verify: fork %s has no preview URL to check", req.ForkID)
	}

	// Wait for a free profile rather than starting more browsers.
	if err := v.gate.Acquire(ctx); err != nil {
		return nil, err
	}
	defer v.gate.Release()

	profile := v.takeProfile()
	defer v.returnProfile(profile)
	req.Profile = profile

	job, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("verify: encode job: %w", err)
	}

	started := time.Now()
	exec, err := v.driver.Exec(ctx, v.cfg.UIInstanceID, vm.Command{
		Argv:    v.cfg.DriverCommand,
		Stdin:   job,
		Timeout: v.cfg.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("verify: run on the UI VM: %w", err)
	}

	report := &Report{Profile: profile, Duration: time.Since(started)}
	if exec.TimedOut {
		report.Passed = false
		report.Summary = fmt.Sprintf("verification did not finish within %s", v.cfg.Timeout)
		return report, nil
	}

	if err := decodeReport(exec.Stdout, report); err != nil {
		// A harness that produced nothing usable has not verified anything, so
		// this is a failed verification rather than a pass by default.
		report.Passed = false
		report.Summary = fmt.Sprintf("the verifier produced no usable report: %v", err)
		if detail := strings.TrimSpace(exec.Stderr); detail != "" {
			report.Summary += "\n" + detail
		}
		return report, nil
	}
	return report, nil
}

// decodeReport parses the harness's JSON report, tolerating leading output.
func decodeReport(stdout string, report *Report) error {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return errors.New("no output")
	}

	type wire struct {
		Passed  bool    `json:"passed"`
		Summary string  `json:"summary"`
		Checks  []Check `json:"checks"`
	}

	decode := func(text string) (*wire, bool) {
		var w wire
		if err := json.Unmarshal([]byte(text), &w); err != nil {
			return nil, false
		}
		return &w, true
	}

	if w, ok := decode(trimmed); ok {
		report.Passed, report.Summary, report.Checks = w.Passed, w.Summary, w.Checks
		return nil
	}

	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if w, ok := decode(line); ok {
			report.Passed, report.Summary, report.Checks = w.Passed, w.Summary, w.Checks
			return nil
		}
	}
	return errors.New("output was not a JSON report")
}

// takeProfile claims a free browser profile. The gate guarantees one is
// available before this is called.
func (v *Verifier) takeProfile() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.free) == 0 {
		// Unreachable while the gate and the pool agree on their size, but a
		// synthesised name is better than blocking forever if they ever drift.
		return fmt.Sprintf("verifier-overflow-%d", time.Now().UnixNano())
	}
	profile := v.free[0]
	v.free = v.free[1:]
	return profile
}

// returnProfile puts a profile back in the pool.
func (v *Verifier) returnProfile(profile string) {
	if strings.HasPrefix(profile, "verifier-overflow-") {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.free = append(v.free, profile)
}
