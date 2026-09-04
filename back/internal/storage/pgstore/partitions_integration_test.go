//go:build integration

// Integration test for the logs partition roll against a real Postgres with
// 001 applied: its install-day partitions are ours by name and fall behind the
// window, while two an operator made by hand sit in the way.
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
	// Not ours by name: the roll must never touch them, however far behind they fall.
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS logs_today PARTITION OF logs FOR VALUES FROM ('2020-01-01') TO ('2020-01-02')`,
		`CREATE TABLE IF NOT EXISTS logs_tomorrow PARTITION OF logs FOR VALUES FROM ('2020-01-02') TO ('2020-01-03')`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("operator partition: %v", err)
		}
	}
	// A clock far from install day, so 001's own partitions are all behind the window.
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
	// What goes on the first roll is 001's install-day partitions and nothing newer.
	for _, name := range dropped {
		day, ok := partitionDay(name)
		if !ok || !day.Before(time.Date(2029, 12, 30, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("dropped %v on a fresh roll, want only the install-day partitions", dropped)
		}
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

	// The operator's partitions are not ours to drop, however far behind they fall.
	names, err := s.logPartitions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !slices.Contains(names, "logs_today") || !slices.Contains(names, "logs_tomorrow") {
		t.Fatalf("partitions %v, want logs_today and logs_tomorrow untouched", names)
	}
}
