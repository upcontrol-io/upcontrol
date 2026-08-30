package pgstore

import (
	"testing"
	"time"
)

// partitionDay is what decides whether the roll job may drop a table. A name
// it reads too loosely takes somebody else's partition with it.
func TestPartitionDayReadsOnlyItsOwnNames(t *testing.T) {
	day, ok := partitionDay("logs_20260830")
	if !ok || !day.Equal(time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("logs_20260830: got %v, ok=%v", day, ok)
	}
	for _, name := range []string{
		"logs",           // the parent itself
		"logs_today",     // 001's install-time partitions
		"logs_tomorrow",  //
		"logs_2026083",   // one digit short
		"logs_20260830x", // trailing junk
		"logs_d20260830", // an operator's own naming
		"logs_20261332",  // month 13, day 32
		"events_20260830",
	} {
		if _, ok := partitionDay(name); ok {
			t.Errorf("%s: parsed as ours, want rejected", name)
		}
	}
}
