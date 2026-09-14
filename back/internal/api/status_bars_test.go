package api

import (
	"testing"
	"time"
)

// The window is what the target has actually measured: age climbs 24h - 48h -
// 7d - 30d and the bucket widens at each rung while the bar count is
// 96/96/84/60 (owner decision, 2026-09-14: the 15-minute UTC slot is
// the colour unit at every rung, so the bucket only ever groups whole slots).
func TestStripPlanClimbsTheWindowWithAge(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	const interval300 = int32(300) // below every rung's cadence floor: no change

	cases := []struct {
		name       string
		oldest     time.Time
		wantBucket time.Duration
		wantCount  int
	}{
		{"no checks at all — zero oldest", time.Time{}, 15 * time.Minute, 96},
		{"23 hours is still the first rung", now.Add(-23 * time.Hour), 15 * time.Minute, 96},
		{"exactly 24 hours buys the second rung", now.Add(-24 * time.Hour), 30 * time.Minute, 96},
		{"47 hours is still the second rung", now.Add(-47 * time.Hour), 30 * time.Minute, 96},
		{"48 hours buys the third rung", now.Add(-48 * time.Hour), 2 * time.Hour, 84},
		{"6 days 23 hours is still the third rung", now.Add(-(6*24 + 23) * time.Hour), 2 * time.Hour, 84},
		{"7 days buys the fourth rung", now.Add(-7 * 24 * time.Hour), 12 * time.Hour, 60},
		{"29 days is still the fourth rung", now.Add(-29 * 24 * time.Hour), 12 * time.Hour, 60},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, count := stripPlanFor(tc.oldest, now, interval300)
			if bucket != tc.wantBucket || count != tc.wantCount {
				t.Fatalf("stripPlanFor() = (%v, %d), want (%v, %d)", bucket, count, tc.wantBucket, tc.wantCount)
			}
		})
	}
}

// The CADENCE FLOOR widens the bucket so a bar is never narrower than 2x the
// target's own slowest interval_sec: a host probed hourly must not draw three
// grey bars out of four that were never actually missed.
func TestStripPlanCadenceFloorWidensTheBucket(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		oldest      time.Time
		intervalSec int32
		wantBucket  time.Duration
		wantCount   int
	}{
		{"900s interval on the 24h rung steps to 30m x 48", now.Add(-1 * time.Hour), 900, 30 * time.Minute, 48},
		{"3600s interval on the 24h rung steps to 2h x 12", now.Add(-1 * time.Hour), 3600, 2 * time.Hour, 12},
		{"3600s interval on the 48h rung steps to 2h x 24", now.Add(-30 * time.Hour), 3600, 2 * time.Hour, 24},
		{"3600s interval on the 7d rung is already 2h x 84 — no change", now.Add(-3 * 24 * time.Hour), 3600, 2 * time.Hour, 84},
		{"60s interval never widens anything — no change", now.Add(-1 * time.Hour), 60, 15 * time.Minute, 96},
		{"zero interval (no rows) leaves the base bucket alone", now.Add(-1 * time.Hour), 0, 15 * time.Minute, 96},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, count := stripPlanFor(tc.oldest, now, tc.intervalSec)
			if bucket != tc.wantBucket || count != tc.wantCount {
				t.Fatalf("stripPlanFor() = (%v, %d), want (%v, %d)", bucket, count, tc.wantBucket, tc.wantCount)
			}
		})
	}
}

// An empty map draws the first rung's shape with nothing measured: no history
// is not an error, it is the honest starting state.
func TestBuildStripWithNoSlotsDrawsAllNodata(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	bars, bucket, ok, total := buildStrip(nil, now)
	if bucket != 15*time.Minute || len(bars) != 96 {
		t.Fatalf("got bucket=%v, %d bars, want 15m, 96 bars", bucket, len(bars))
	}
	for i, b := range bars {
		if b != "nodata" {
			t.Fatalf("bar %d = %q, want nodata", i, b)
		}
	}
	if ok != 0 || total != 0 {
		t.Fatalf("ok=%d total=%d, want 0, 0", ok, total)
	}
}

// A bar takes the WORST slot inside it rather than averaging: one bad slot
// among many clean ones in the same 12h bar must still read down.
func TestBuildStripBarTakesTheWorstSlotInsideIt(t *testing.T) {
	now := time.Date(2026, 9, 14, 23, 45, 0, 0, time.UTC)
	oldest := now.Add(-29 * 24 * time.Hour) // forces the 12h x 60 rung

	todayBar := now.Truncate(12 * time.Hour) // the 12:00-24:00 bar
	slots := map[int64]slotCounts{
		oldest.Truncate(15 * time.Minute).Unix(): {ok: 0, total: 0, intervalSec: 300}, // just to age the rung
	}
	// Fill every 15-minute slot in today's bar clean, then knock one down.
	for i := 0; i < 48; i++ {
		s := todayBar.Add(time.Duration(i) * 15 * time.Minute)
		slots[s.Unix()] = slotCounts{ok: 20, total: 20, intervalSec: 300}
	}
	// 1 of 3 ok is well below the 95% check threshold.
	slots[todayBar.Add(47*15*time.Minute).Unix()] = slotCounts{ok: 1, total: 3, intervalSec: 300}

	bars, bucket, _, _ := buildStrip(slots, now)
	if bucket != 12*time.Hour {
		t.Fatalf("bucket = %v, want 12h", bucket)
	}
	if last := bars[len(bars)-1]; last != "down" {
		t.Fatalf("newest bar = %q, want down (one bad slot must not average away)", last)
	}
}

// A 57/60 slot lands exactly on the check threshold (95%), not down; a bar
// holding both a check-tier and a down-tier slot must read down regardless.
func TestBuildStripNinetyFivePercentIsCheckDownOutranksCheck(t *testing.T) {
	now := time.Date(2026, 9, 14, 23, 45, 0, 0, time.UTC)

	t.Run("57 of 60 is check, not down", func(t *testing.T) {
		slots := map[int64]slotCounts{now.Truncate(15 * time.Minute).Unix(): {ok: 57, total: 60, intervalSec: 300}}
		bars, _, _, _ := buildStrip(slots, now)
		if last := bars[len(bars)-1]; last != "check" {
			t.Fatalf("newest bar = %q, want check", last)
		}
	})

	t.Run("check plus down in one bar reads down", func(t *testing.T) {
		oldest := now.Add(-29 * 24 * time.Hour)
		todayBar := now.Truncate(12 * time.Hour)
		slots := map[int64]slotCounts{
			oldest.Truncate(15 * time.Minute).Unix():  {ok: 0, total: 0, intervalSec: 300},
			todayBar.Add(2 * 15 * time.Minute).Unix(): {ok: 57, total: 60, intervalSec: 300}, // check tier
			todayBar.Add(3 * 15 * time.Minute).Unix(): {ok: 1, total: 12, intervalSec: 300},  // down tier
		}
		bars, bucket, _, _ := buildStrip(slots, now)
		if bucket != 12*time.Hour {
			t.Fatalf("bucket = %v, want 12h", bucket)
		}
		if last := bars[len(bars)-1]; last != "down" {
			t.Fatalf("newest bar = %q, want down (down must outrank check within one bar)", last)
		}
	})
}

// An unmeasured-only slot ages the rung — it counts as the oldest slot
// present — but draws nothing: an unreadable target is not a down one.
func TestBuildStripUnmeasuredSlotAgesTheRungButDrawsNodata(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-8 * 24 * time.Hour) // past the 7-day rung on its own

	slots := map[int64]slotCounts{
		oldest.Truncate(15 * time.Minute).Unix(): {ok: 0, total: 0, intervalSec: 300},
		now.Truncate(15 * time.Minute).Unix():    {ok: 10, total: 10, intervalSec: 300},
	}
	bars, bucket, ok, total := buildStrip(slots, now)
	if bucket != 12*time.Hour || len(bars) != 60 {
		t.Fatalf("bucket=%v bars=%d, want 12h, 60 (unmeasured slot must still age the rung)", bucket, len(bars))
	}
	for i, b := range bars[:len(bars)-1] {
		if b != "nodata" {
			t.Fatalf("bar %d = %q, want nodata (an unmeasured-only slot must not draw a status)", i, b)
		}
	}
	if last := bars[len(bars)-1]; last != "ok" {
		t.Fatalf("newest bar = %q, want ok", last)
	}
	if ok != 10 || total != 10 {
		t.Fatalf("ok=%d total=%d, want 10, 10 (an unmeasured slot must not enter the sums)", ok, total)
	}
}

// The current slot always lands in the last bar.
func TestBuildStripCurrentSlotLandsInTheLastBar(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 7, 0, 0, time.UTC)
	slots := map[int64]slotCounts{now.Truncate(15 * time.Minute).Unix(): {ok: 5, total: 5, intervalSec: 300}}

	bars, _, _, _ := buildStrip(slots, now)
	if last := bars[len(bars)-1]; last != "ok" {
		t.Fatalf("newest bar = %q, want ok", last)
	}
	for i := 0; i < len(bars)-1; i++ {
		if bars[i] != "nodata" {
			t.Fatalf("bar %d = %q, want nodata (only the current slot has data)", i, bars[i])
		}
	}
}

// The oldest bar the window admits (idx 0) is included, and the slot one
// bucket further back is not: an off-by-one on the idx bound would silently
// drop the oldest bar on every rung, and its counts must not leak into the sums.
func TestBuildStripOldestBarIsIncludedOneBucketEarlierIsNot(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	currentStart := now.Truncate(12 * time.Hour)
	oldestBarStart := currentStart.Add(-59 * 12 * time.Hour) // idx 0 on the 60-bar rung
	beforeWindow := currentStart.Add(-60 * 12 * time.Hour)   // one bucket earlier, idx -1

	slots := map[int64]slotCounts{
		oldestBarStart.Unix(): {ok: 1, total: 1, intervalSec: 300},
		beforeWindow.Unix():   {ok: 0, total: 5, intervalSec: 300}, // would read down if it leaked into the window
	}
	bars, bucket, ok, total := buildStrip(slots, now)
	if bucket != 12*time.Hour || len(bars) != 60 {
		t.Fatalf("bucket=%v bars=%d, want 12h, 60", bucket, len(bars))
	}
	if bars[0] != "ok" {
		t.Fatalf("bars[0] = %q, want ok (the oldest bar in the window must not be dropped)", bars[0])
	}
	if ok != 1 || total != 1 {
		t.Fatalf("ok=%d total=%d, want 1, 1 (the slot one bucket before the window must not leak in)", ok, total)
	}
}

// A slot older than the shown window is ignored even though it is the very
// slot that picked the (catch-all, 30-day) rung: the widest rung still caps
// at 30 days no matter how old the oldest slot actually is. Its interval_sec
// is deliberately huge to show it cannot leak into the cadence floor either,
// though this particular rung can't show the floor actually MOVING back down
// (12h is already the widest step on the ladder) — for that,
// TestBuildStripCadenceFloorIgnoresOutOfWindowRow below uses the 48h rung,
// where an out-of-window row's huge interval is observably NOT applied.
func TestBuildStripSlotOlderThanTheWindowIsExcluded(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	tooOld := now.Add(-40 * 24 * time.Hour).Truncate(15 * time.Minute) // forces the widest rung, itself 10 days past its 30-day reach
	current := now.Truncate(15 * time.Minute)

	slots := map[int64]slotCounts{
		tooOld.Unix():  {ok: 0, total: 4, intervalSec: 999999}, // would read as down (and could not float the floor further) if it counted
		current.Unix(): {ok: 4, total: 4, intervalSec: 300},
	}
	bars, bucket, ok, total := buildStrip(slots, now)
	if bucket != 12*time.Hour || len(bars) != 60 {
		t.Fatalf("bucket=%v bars=%d, want 12h, 60", bucket, len(bars))
	}
	if ok != 4 || total != 4 {
		t.Fatalf("ok=%d total=%d, want 4, 4 (the out-of-window slot must not enter the sums)", ok, total)
	}
	for i, b := range bars {
		if b == "down" {
			t.Fatalf("bar %d = down, the too-old slot leaked into the window", i)
		}
	}
	if last := bars[len(bars)-1]; last != "ok" {
		t.Fatalf("newest bar = %q, want ok", last)
	}
}

// ok/total sum across every bar in the window, not just the last one.
func TestBuildStripSumsOkAndTotalAcrossAllBars(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	slots := map[int64]slotCounts{
		now.Truncate(15 * time.Minute).Unix():                        {ok: 10, total: 10, intervalSec: 300},
		now.Add(-15 * time.Minute).Truncate(15 * time.Minute).Unix(): {ok: 5, total: 10, intervalSec: 300},
	}
	_, _, ok, total := buildStrip(slots, now)
	if ok != 15 || total != 20 {
		t.Fatalf("ok=%d total=%d, want 15, 20", ok, total)
	}
}

// Bars align to the UTC clock, not backwards from now: at 14:40 UTC on a 2h
// bucket the current bar is 14:00-16:00, so a 13:45 slot (in the prior
// 12:00-14:00 bar) lands two bars back, not one.
func TestBuildStripTwoHourBucketsAlignToTheUTCClock(t *testing.T) {
	now := time.Date(2026, 9, 14, 14, 40, 0, 0, time.UTC)
	oldest := now.Add(-3 * 24 * time.Hour) // forces the 2h x 84 rung

	slots := map[int64]slotCounts{
		oldest.Truncate(15 * time.Minute).Unix():              {ok: 0, total: 0, intervalSec: 300},
		time.Date(2026, 9, 14, 13, 45, 0, 0, time.UTC).Unix(): {ok: 3, total: 3, intervalSec: 300},
	}
	bars, bucket, _, _ := buildStrip(slots, now)
	if bucket != 2*time.Hour {
		t.Fatalf("bucket = %v, want 2h", bucket)
	}
	want := len(bars) - 2
	if bars[want] != "ok" {
		t.Fatalf("bar %d = %q, want ok (the 13:45 slot belongs to the 12:00-14:00 bucket)", want, bars[want])
	}
	if bars[len(bars)-1] != "nodata" {
		t.Fatalf("last bar = %q, want nodata (nothing measured in the current 14:00-16:00 bucket)", bars[len(bars)-1])
	}
}

// The server-rendered page names the window its uptime covers, in the fronts' words.
func TestStripWindowLabelNamesEveryRung(t *testing.T) {
	cases := []struct {
		bucket time.Duration
		count  int
		want   string
	}{
		{15 * time.Minute, 96, "24 h"},
		{30 * time.Minute, 48, "24 h"}, // 900s cadence floor on the 24h rung
		{2 * time.Hour, 12, "24 h"},    // 3600s cadence floor on the 24h rung
		{30 * time.Minute, 96, "48 h"},
		{2 * time.Hour, 84, "7 days"},
		{12 * time.Hour, 60, "30 days"},
	}
	for _, tc := range cases {
		if got := stripWindowLabel(tc.bucket, tc.count); got != tc.want {
			t.Errorf("stripWindowLabel(%v, %d) = %q, want %q", tc.bucket, tc.count, got, tc.want)
		}
	}
}

// The cadence floor above is exercised only through stripPlanFor's own
// intervalSec argument; buildStrip is what actually reads a target's widest
// interval off its own slots (the maxInterval loop in write_api.go), and no
// test called that loop with anything but a uniform 300s that never widens
// anything. These pin the loop itself: the widest interval among the
// target's slots must reach stripPlanFor and actually widen the bucket.
func TestBuildStripCadenceFloorFromSlotsWidensTheBucket(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	t.Run("900s interval on a young target gives 30m x 48", func(t *testing.T) {
		slots := map[int64]slotCounts{
			now.Truncate(15 * time.Minute).Unix(): {ok: 1, total: 1, intervalSec: 900},
		}
		bars, bucket, _, _ := buildStrip(slots, now)
		if bucket != 30*time.Minute || len(bars) != 48 {
			t.Fatalf("bucket=%v bars=%d, want 30m, 48", bucket, len(bars))
		}
	})

	t.Run("3600s interval on a young target gives 2h x 12", func(t *testing.T) {
		slots := map[int64]slotCounts{
			now.Truncate(15 * time.Minute).Unix(): {ok: 1, total: 1, intervalSec: 3600},
		}
		bars, bucket, _, _ := buildStrip(slots, now)
		if bucket != 2*time.Hour || len(bars) != 12 {
			t.Fatalf("bucket=%v bars=%d, want 2h, 12", bucket, len(bars))
		}
	})

	t.Run("the widest interval among several slots wins, not the narrowest", func(t *testing.T) {
		slots := map[int64]slotCounts{
			now.Truncate(15 * time.Minute).Unix():                        {ok: 1, total: 1, intervalSec: 300},
			now.Add(-30 * time.Minute).Truncate(15 * time.Minute).Unix(): {ok: 1, total: 1, intervalSec: 3600},
		}
		bars, bucket, _, _ := buildStrip(slots, now)
		if bucket != 2*time.Hour || len(bars) != 12 {
			t.Fatalf("bucket=%v bars=%d, want 2h, 12 (the widest interval among the slots must win, a min instead of a max would leave this at 300s)", bucket, len(bars))
		}
	})
}

// An out-of-window row must not move the floor even though it is old enough
// to have picked the rung: the 48h rung (not the terminal 30-day one) is
// where this is actually observable, because 30m is not already the widest
// step on the ladder. Counter-example: a slot at 2026-09-12 12:15 with
// intervalSec 3600 sits at age 47h55m (still the 48h rung) but before that
// rung's own windowStart (2026-09-12 12:30) — dropping the window filter on
// the maxInterval loop widens 30m x 96 to 2h x 24.
func TestBuildStripCadenceFloorIgnoresOutOfWindowRow(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 10, 0, 0, time.UTC)
	oldest := time.Date(2026, 9, 12, 12, 15, 0, 0, time.UTC)

	slots := map[int64]slotCounts{
		oldest.Unix():                         {ok: 1, total: 1, intervalSec: 3600}, // ages the rung, but sits before windowStart
		now.Truncate(15 * time.Minute).Unix(): {ok: 1, total: 1, intervalSec: 300},
	}
	bars, bucket, _, _ := buildStrip(slots, now)
	if bucket != 30*time.Minute || len(bars) != 96 {
		t.Fatalf("bucket=%v bars=%d, want 30m, 96 (the out-of-window row must not widen the bucket)", bucket, len(bars))
	}
}
