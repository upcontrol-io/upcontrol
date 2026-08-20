package api

import (
	"testing"
	"time"
)

// A day is the wrong bucket for an account that started this morning: its check
// has been running for minutes, so six of the seven daily bars would read
// `nodata` for a week — on exactly the page a new owner shows people.
func TestBarSpanFollowsTheCheckIntervalUntilThereIsADayOfHistory(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		oldest   time.Time
		interval int32
		want     time.Duration
	}{
		{
			name:     "no checks at all — the monitor was created a minute ago",
			oldest:   time.Time{},
			interval: 300,
			want:     5 * time.Minute,
		},
		{
			name:     "an hour of history is still not a day",
			oldest:   now.Add(-time.Hour),
			interval: 300,
			want:     5 * time.Minute,
		},
		{
			name:     "the interval is the monitor's own, not a constant",
			oldest:   now.Add(-10 * time.Minute),
			interval: 60,
			want:     time.Minute,
		},
		{
			name:     "past a day, days win",
			oldest:   now.Add(-25 * time.Hour),
			interval: 300,
			want:     24 * time.Hour,
		},
		{
			name:     "exactly a day counts as a day",
			oldest:   now.Add(-24 * time.Hour),
			interval: 300,
			want:     24 * time.Hour,
		},
		{
			// A row with no interval on it (legacy, or a kind that does not carry
			// one) must not produce a zero-length bucket: that divides by zero in
			// the bucket maths and would draw an empty strip forever.
			name:     "a missing interval falls back to five minutes",
			oldest:   time.Time{},
			interval: 0,
			want:     5 * time.Minute,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := barSpanFor(tc.oldest, tc.interval, now); got != tc.want {
				t.Fatalf("barSpanFor() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The strip has to cover a stretch a new account can actually fill: twelve
// checks at the plan's floor is an hour, not a week.
func TestIntervalStripCoversTwelveChecks(t *testing.T) {
	span := barSpanFor(time.Time{}, 300, time.Now().UTC())
	if total := span * statusIntervalBars; total != time.Hour {
		t.Fatalf("a 5m check draws %v of history, want 1h", total)
	}
}
