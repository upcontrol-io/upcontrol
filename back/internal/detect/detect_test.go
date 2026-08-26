package detect

import (
	"strings"
	"testing"
	"time"

	detector "go.upcontrol.io/back/internal/detect/detectors"
)

// decide is pure and every branch must be pinned, including combinations the
// live loop cannot produce (dedup suppresses a fire while an incident is open).
func TestDecide(t *testing.T) {
	tests := []struct {
		name       string
		fire       bool
		suppressed bool
		hasOpen    bool
		want       action
	}{
		{"fire clean opens", true, false, false, actOpen},
		{"fire clean with open defers to OpenDetect dedup", true, false, true, actOpen},
		{"fire suppressed does nothing", true, true, false, actNone},
		{"fire suppressed with open does nothing", true, true, true, actNone},
		{"no fire with open closes", false, false, true, actClose},
		{"no fire suppressed with open still closes", false, true, true, actClose},
		{"no fire nothing open does nothing", false, false, false, actNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := detector.Decision{Fire: tt.fire}
			if got := decide(dec, tt.suppressed, tt.hasOpen); got != tt.want {
				t.Errorf("decide(%+v) = %v, want %v", tt, got, tt.want)
			}
		})
	}
}

// windowBounds must cut at completed minutes: anything inside the running
// minute is not a finished window and must not be scored.
func TestWindowBounds(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 34, 56, 789123456, time.UTC)
	from, to := windowBounds(now)

	wantTo := time.Date(2026, 8, 18, 12, 34, 0, 0, time.UTC)
	wantFrom := wantTo.Add(-5 * time.Minute)
	if !to.Equal(wantTo) {
		t.Errorf("windowBounds to = %v, want %v", to, wantTo)
	}
	if !from.Equal(wantFrom) {
		t.Errorf("windowBounds from = %v, want %v", from, wantFrom)
	}
}

// baselineBounds ends exactly where the current window begins: overlap would
// let the spike being scored inflate its own baseline.
func TestBaselineBounds(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 34, 56, 789123456, time.UTC)
	from, to := baselineBounds(now)

	wantTo := time.Date(2026, 8, 18, 12, 34, 0, 0, time.UTC).Add(-5 * time.Minute)
	wantFrom := wantTo.Add(-7 * 24 * time.Hour)
	if !to.Equal(wantTo) {
		t.Errorf("baselineBounds to = %v, want %v", to, wantTo)
	}
	if !from.Equal(wantFrom) {
		t.Errorf("baselineBounds from = %v, want %v", from, wantFrom)
	}
}

// The alert quotes the baseline only when there was one: a young project
// fires on the flat 10% rule, and "weekly baseline is 0.0%" asserts a lie.
func TestErrorRateSummary_QuotesOnlyAMeasuredBaseline(t *testing.T) {
	const window = "in the last 5 minutes"

	withBaseline := errorRateSummary(30, 200, 0.012, 0.004)
	if !strings.Contains(withBaseline, "15.0% of the log stream "+window+".") {
		t.Errorf("measured share missing: %q", withBaseline)
	}
	if !strings.Contains(withBaseline, "The weekly baseline is 1.2%.") {
		t.Errorf("a measured baseline must be quoted: %q", withBaseline)
	}

	// MAD of zero is the no-history contract, not a measured steadiness the
	// detector scored against.
	noBaseline := errorRateSummary(30, 200, 0, 0)
	if strings.Contains(noBaseline, "baseline") {
		t.Errorf("an unmeasured baseline must not be quoted: %q", noBaseline)
	}
	if !strings.Contains(noBaseline, "15.0% of the log stream "+window+".") {
		t.Errorf("the measured half still stands alone: %q", noBaseline)
	}
}
