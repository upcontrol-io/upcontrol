package api

import (
	"testing"
	"time"
)

// The window is what the account has actually measured: a day of grey bars on a
// page whose first check ran forty minutes ago is the one lie a monitoring
// product may not tell. Each rung is earned by filling the one below it.
func TestBarWindowClimbsTheLadderAsHistoryAccumulates(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		oldest time.Time
		want   time.Duration
	}{
		{"no checks at all — the monitor was created a minute ago", time.Time{}, time.Hour},
		{"forty minutes buys nothing yet", now.Add(-40 * time.Minute), time.Hour},
		{"an hour buys the two-hour window", now.Add(-time.Hour), 2 * time.Hour},
		{"ninety minutes is still on the same rung", now.Add(-90 * time.Minute), 2 * time.Hour},
		{"two hours buy four", now.Add(-2 * time.Hour), 4 * time.Hour},
		{"four buy eight", now.Add(-4 * time.Hour), 8 * time.Hour},
		{"eight buy sixteen", now.Add(-8 * time.Hour), 16 * time.Hour},
		{"sixteen buy the day", now.Add(-16 * time.Hour), 24 * time.Hour},
		{"a week of history is still a day: the ladder stops there", now.Add(-7 * 24 * time.Hour), 24 * time.Hour},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _, _ := barPlanFor(tc.oldest, 300, now); got != tc.want {
				t.Fatalf("barPlanFor() window = %v, want %v", got, tc.want)
			}
		})
	}
}

// Doubling the window doubles the bar count, so one bar keeps covering the same
// five minutes and simply gets thinner — until the count hits its ceiling and
// the bucket has to widen instead.
func TestBarCountDoublesWithTheWindowUntilTheCeiling(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		have       time.Duration
		wantWindow time.Duration
		wantBucket time.Duration
		wantCount  int
	}{
		{0, time.Hour, 5 * time.Minute, 12},
		{time.Hour, 2 * time.Hour, 5 * time.Minute, 24},
		{2 * time.Hour, 4 * time.Hour, 5 * time.Minute, 48},
		{4 * time.Hour, 8 * time.Hour, 5 * time.Minute, 96},
		// Past the ceiling: 16 h over 96 bars is ten minutes each, not 192 bars.
		{8 * time.Hour, 16 * time.Hour, 10 * time.Minute, 96},
		{16 * time.Hour, 24 * time.Hour, 15 * time.Minute, 96},
	}

	for _, tc := range cases {
		oldest := time.Time{}
		if tc.have > 0 {
			oldest = now.Add(-tc.have)
		}
		window, bucket, count := barPlanFor(oldest, 300, now)
		if window != tc.wantWindow || bucket != tc.wantBucket || count != tc.wantCount {
			t.Fatalf("%v of history: got (%v, %v, %d), want (%v, %v, %d)",
				tc.have, window, bucket, count, tc.wantWindow, tc.wantBucket, tc.wantCount)
		}
		if bucket*time.Duration(count) != window {
			t.Fatalf("%v of history: %d bars of %v do not add up to %v", tc.have, count, bucket, window)
		}
	}
}

// A bucket shorter than the check's own interval draws gaps that are not
// outages: most of those bars would hold no check at all.
func TestBucketNeverFallsBelowTheCheckInterval(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

	// A half-hour check on the top rung: 48 bars of 30 minutes, not 96 of 15.
	window, bucket, count := barPlanFor(now.Add(-20*time.Hour), 1800, now)
	if window != 24*time.Hour || bucket != 30*time.Minute || count != 48 {
		t.Fatalf("got (%v, %v, %d), want (24h, 30m, 48)", window, bucket, count)
	}

	// A minute check does NOT get 60 bars an hour: five minutes is the floor,
	// so the strip stays the same shape as everyone else's.
	if _, bucket, count = barPlanFor(time.Time{}, 60, now); bucket != 5*time.Minute || count != 12 {
		t.Fatalf("a 1m check got (%v, %d), want (5m, 12)", bucket, count)
	}

	// A row with no interval must not produce a zero-length bucket: that
	// divides by zero in the bucket maths.
	if _, bucket, count = barPlanFor(time.Time{}, 0, now); bucket != 5*time.Minute || count != 12 {
		t.Fatalf("a missing interval got (%v, %d), want (5m, 12)", bucket, count)
	}

	// An interval wider than the whole window: one honest bar, never zero bars.
	if _, bucket, count = barPlanFor(time.Time{}, 7200, now); count != 1 || bucket != time.Hour {
		t.Fatalf("a 2h check on the first rung got (%v, %d), want (1h, 1)", bucket, count)
	}
}
