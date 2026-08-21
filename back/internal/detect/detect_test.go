package detect

import (
	"testing"
	"time"

	detector "go.upcontrol.io/back/internal/detect/detectors"
)

// decide is pure and every branch reachable state must be pinned — including
// combinations the live loop cannot produce (dedup always suppresses a fire
// while an incident is open), because decide itself must not care.
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
