// Package tripwire bounds how far a fork may go before it must ask for help.
//
// The verify/fix loop is fully automatic: failures feed back to the coding
// agent with no human involvement. A tripwire is what stops that loop running
// forever when an agent is stuck beyond its own ability to resolve. It is a
// combination of cycle count, spend and wall-clock time, and the thresholds
// are global rather than per-task, so there is one set of numbers to reason
// about rather than one per workstream.
package tripwire

import (
	"fmt"
	"time"

	"github.com/dabbers/devex/internal/domain"
)

// Kind names which threshold was crossed.
type Kind string

// Threshold kinds.
const (
	KindCycles    Kind = "cycles"
	KindTokens    Kind = "tokens"
	KindCost      Kind = "cost"
	KindWallClock Kind = "wall_clock"
)

// Thresholds are the global limits. A zero value on any axis means that axis
// is unbounded.
type Thresholds struct {
	// MaxCycles bounds completed verify/fix rounds.
	MaxCycles int `yaml:"max_cycles" json:"max_cycles"`
	// MaxTokens bounds total tokens, input plus output.
	MaxTokens int64 `yaml:"max_tokens" json:"max_tokens"`
	// MaxCostUSD bounds spend.
	MaxCostUSD float64 `yaml:"max_cost_usd" json:"max_cost_usd"`
	// MaxWallClock bounds time spent working, excluding time queued or paused
	// waiting on the user.
	MaxWallClock time.Duration `yaml:"max_wall_clock" json:"max_wall_clock"`
}

// Defaults returns thresholds suitable for a single-machine v1 deployment.
func Defaults() Thresholds {
	return Thresholds{
		MaxCycles:    8,
		MaxTokens:    4_000_000,
		MaxCostUSD:   25,
		MaxWallClock: 4 * time.Hour,
	}
}

// Breach describes a crossed threshold in terms the user can act on.
type Breach struct {
	Kind Kind `json:"kind"`
	// Limit and Actual are rendered for display rather than compared, since
	// each kind has its own units.
	Limit   string `json:"limit"`
	Actual  string `json:"actual"`
	Message string `json:"message"`
}

// Error implements error so a breach can flow through error paths.
func (b *Breach) Error() string { return b.Message }

// Check reports the first threshold usage has crossed, or nil.
//
// Checks run in order of how clearly they indicate a stuck agent: a cycle
// count says the loop is not converging, whereas a cost ceiling may simply
// mean the work is large.
func (t Thresholds) Check(usage domain.Usage) *Breach {
	if t.MaxCycles > 0 && usage.Cycles >= t.MaxCycles {
		return &Breach{
			Kind:    KindCycles,
			Limit:   fmt.Sprint(t.MaxCycles),
			Actual:  fmt.Sprint(usage.Cycles),
			Message: fmt.Sprintf("stopped after %d verify/fix cycles without converging (limit %d)", usage.Cycles, t.MaxCycles),
		}
	}
	if tokens := usage.InputTokens + usage.OutputTokens; t.MaxTokens > 0 && tokens >= t.MaxTokens {
		return &Breach{
			Kind:    KindTokens,
			Limit:   fmt.Sprint(t.MaxTokens),
			Actual:  fmt.Sprint(tokens),
			Message: fmt.Sprintf("stopped after %d tokens (limit %d)", tokens, t.MaxTokens),
		}
	}
	if t.MaxCostUSD > 0 && usage.CostUSD >= t.MaxCostUSD {
		return &Breach{
			Kind:    KindCost,
			Limit:   fmt.Sprintf("$%.2f", t.MaxCostUSD),
			Actual:  fmt.Sprintf("$%.2f", usage.CostUSD),
			Message: fmt.Sprintf("stopped after spending $%.2f (limit $%.2f)", usage.CostUSD, t.MaxCostUSD),
		}
	}
	if t.MaxWallClock > 0 && usage.Wall >= t.MaxWallClock {
		return &Breach{
			Kind:    KindWallClock,
			Limit:   t.MaxWallClock.String(),
			Actual:  usage.Wall.Round(time.Second).String(),
			Message: fmt.Sprintf("stopped after %s of work (limit %s)", usage.Wall.Round(time.Second), t.MaxWallClock),
		}
	}
	return nil
}

// Remaining reports how much headroom is left on each bounded axis, for
// display alongside a running fork.
type Remaining struct {
	Cycles    int           `json:"cycles,omitempty"`
	Tokens    int64         `json:"tokens,omitempty"`
	CostUSD   float64       `json:"cost_usd,omitempty"`
	WallClock time.Duration `json:"wall_clock,omitempty"`
}

// Remaining computes the headroom left under these thresholds.
func (t Thresholds) Remaining(usage domain.Usage) Remaining {
	var r Remaining
	if t.MaxCycles > 0 {
		r.Cycles = max(t.MaxCycles-usage.Cycles, 0)
	}
	if t.MaxTokens > 0 {
		r.Tokens = max(t.MaxTokens-(usage.InputTokens+usage.OutputTokens), 0)
	}
	if t.MaxCostUSD > 0 {
		r.CostUSD = max(t.MaxCostUSD-usage.CostUSD, 0)
	}
	if t.MaxWallClock > 0 {
		r.WallClock = max(t.MaxWallClock-usage.Wall, 0)
	}
	return r
}
