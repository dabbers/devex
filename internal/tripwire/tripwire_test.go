package tripwire

import (
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
)

func TestZeroThresholdsNeverFire(t *testing.T) {
	var unlimited Thresholds
	huge := domain.Usage{Cycles: 1000, InputTokens: 1 << 40, CostUSD: 1e6, Wall: 30 * 24 * time.Hour}
	if breach := unlimited.Check(huge); breach != nil {
		t.Fatalf("an unset threshold fired: %+v", breach)
	}
}

func TestEachThresholdFires(t *testing.T) {
	thresholds := Thresholds{MaxCycles: 5, MaxTokens: 1000, MaxCostUSD: 10, MaxWallClock: time.Hour}

	tests := map[Kind]domain.Usage{
		KindCycles:    {Cycles: 5},
		KindTokens:    {InputTokens: 600, OutputTokens: 400},
		KindCost:      {CostUSD: 10},
		KindWallClock: {Wall: time.Hour},
	}
	for kind, usage := range tests {
		breach := thresholds.Check(usage)
		if breach == nil {
			t.Errorf("%s threshold did not fire for %+v", kind, usage)
			continue
		}
		if breach.Kind != kind {
			t.Errorf("usage %+v fired %s, want %s", usage, breach.Kind, kind)
		}
		if breach.Message == "" || breach.Error() != breach.Message {
			t.Errorf("%s breach has no usable message: %+v", kind, breach)
		}
	}
}

func TestThresholdsAreInclusive(t *testing.T) {
	thresholds := Thresholds{MaxCycles: 3}
	if thresholds.Check(domain.Usage{Cycles: 2}) != nil {
		t.Error("below the limit should not fire")
	}
	// Reaching the limit is the stopping point, not exceeding it: a ninth
	// cycle with a limit of eight has already spent the budget.
	if thresholds.Check(domain.Usage{Cycles: 3}) == nil {
		t.Error("reaching the limit should fire")
	}
}

func TestCycleBreachIsReportedFirst(t *testing.T) {
	thresholds := Thresholds{MaxCycles: 2, MaxTokens: 10, MaxCostUSD: 1, MaxWallClock: time.Minute}
	// Everything is over at once; the cycle count is the clearest signal that
	// the loop is not converging, so it should be what the user is told.
	breach := thresholds.Check(domain.Usage{Cycles: 9, InputTokens: 900, CostUSD: 99, Wall: time.Hour})
	if breach == nil || breach.Kind != KindCycles {
		t.Fatalf("breach = %+v, want the cycle threshold", breach)
	}
}

func TestTokenThresholdCountsBothDirections(t *testing.T) {
	thresholds := Thresholds{MaxTokens: 100}
	// Neither side alone crosses the limit, but together they do.
	if thresholds.Check(domain.Usage{InputTokens: 60, OutputTokens: 60}) == nil {
		t.Fatal("input and output tokens should be counted together")
	}
	if thresholds.Check(domain.Usage{InputTokens: 40, OutputTokens: 40}) != nil {
		t.Fatal("under the combined limit should not fire")
	}
}

func TestRemainingReportsHeadroom(t *testing.T) {
	thresholds := Thresholds{MaxCycles: 8, MaxTokens: 1000, MaxCostUSD: 10, MaxWallClock: time.Hour}
	got := thresholds.Remaining(domain.Usage{Cycles: 3, InputTokens: 300, OutputTokens: 100, CostUSD: 2.5, Wall: 15 * time.Minute})

	if got.Cycles != 5 || got.Tokens != 600 || got.CostUSD != 7.5 || got.WallClock != 45*time.Minute {
		t.Fatalf("Remaining = %+v", got)
	}

	// Headroom never goes negative, which would read as a bonus budget.
	over := thresholds.Remaining(domain.Usage{Cycles: 100, InputTokens: 99999, CostUSD: 999, Wall: 24 * time.Hour})
	if over.Cycles != 0 || over.Tokens != 0 || over.CostUSD != 0 || over.WallClock != 0 {
		t.Fatalf("Remaining past the limits = %+v, want zeroes", over)
	}
}

func TestDefaultsAreBounded(t *testing.T) {
	d := Defaults()
	if d.MaxCycles <= 0 || d.MaxTokens <= 0 || d.MaxCostUSD <= 0 || d.MaxWallClock <= 0 {
		t.Fatalf("defaults leave an axis unbounded: %+v", d)
	}
}
