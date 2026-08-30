//go:build integration

// Integration test for the logs partition roll against a real Postgres with
// 001 applied, so logs_today and logs_tomorrow sit in the way exactly as they
// do on an install that has never seen 003.
// Run: go test -tags=integration ./internal/storage/pgstore/...
package pgstore

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestRollLogPartitionsCoversTheWindowAndDropsBehindIt(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	// A clock far from install day: 001 has just made logs_today over the real
	// today, and a second partition over the same day would be refused.
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	const keep = 48 * time.Hour

	created, dropped, err := s.RollLogPartitions(ctx, now, 3, keep)
	if err != nil {
		t.Fatalf("first roll: %v", err)
	}
	// Two days back (the keep window), the day itself, and three ahead.
	want := []string{
		"logs_20291230", "logs_20291231", "logs_20300101",
		"logs_20300102", "logs_20300103", "logs_20300104",
	}
	if !slices.Equal(created, want) {
		t.Fatalf("created %v, want %v", created, want)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped %v on a fresh roll, want none", dropped)
	}

	// The job runs hourly: the same day twice must be a no-op, not an error.
	created, dropped, err = s.RollLogPartitions(ctx, now, 3, keep)
	if err != nil {
		t.Fatalf("second roll: %v", err)
	}
	if len(created) != 0 || len(dropped) != 0 {
		t.Fatalf("second roll created %v and dropped %v, want neither", created, dropped)
	}

	// Five days on. Everything whose day ended before 2030-01-04 12:00 goes;
	// logs_20300104 ends at 01-05 00:00 and stays.
	created, dropped, err = s.RollLogPartitions(ctx, now.AddDate(0, 0, 5), 3, keep)
	if err != nil {
		t.Fatalf("third roll: %v", err)
	}
	wantDropped := []string{
		"logs_20291230", "logs_20291231", "logs_20300101",
		"logs_20300102", "logs_20300103",
	}
	if !slices.Equal(dropped, wantDropped) {
		t.Fatalf("dropped %v, want %v", dropped, wantDropped)
	}
	wantCreated := []string{
		"logs_20300105", "logs_20300106", "logs_20300107",
		"logs_20300108", "logs_20300109",
	}
	if !slices.Equal(created, wantCreated) {
		t.Fatalf("created %v, want %v", created, wantCreated)
	}

	// 001's own partitions are not ours to drop, however far behind they fall.
	names, err := s.logPartitions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !slices.Contains(names, "logs_today") || !slices.Contains(names, "logs_tomorrow") {
		t.Fatalf("partitions %v, want logs_today and logs_tomorrow untouched", names)
	}
}
