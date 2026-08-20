package cutoff

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
)

func bucket(at time.Time, minSeq, rows int64) sqlc.TenantLineLedger {
	return sqlc.TenantLineLedger{
		Bucket5m: pgtype.Timestamptz{Time: at, Valid: true},
		MinSeq:   minSeq,
		Rows:     rows,
	}
}

func TestApplyTimeCeiling_RowBoundaryBinds(t *testing.T) {
	// 6 buckets of 5m within the last 30 min — all inside a 24h window, so the
	// time ceiling must NOT clamp; the row-count cutoff (seq 100) stands.
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	bs := []sqlc.TenantLineLedger{
		bucket(now.Add(-5*time.Minute), 500, 100),
		bucket(now.Add(-10*time.Minute), 400, 100),
		bucket(now.Add(-15*time.Minute), 300, 100),
		bucket(now.Add(-20*time.Minute), 200, 100),
		bucket(now.Add(-25*time.Minute), 100, 100), // row cutoff lands here
		bucket(now.Add(-30*time.Minute), 1, 100),
	}
	if got := applyTimeCeiling(bs, 100, 24, now); got != 100 {
		t.Fatalf("row-boundary case: got cutoff %d, want 100", got)
	}
}

func TestApplyTimeCeiling_TimeBoundaryClamps(t *testing.T) {
	// The row cutoff reaches back 48h, but WindowHours is 24h: the time ceiling
	// must clamp cutoff forward to the oldest bucket still inside 24h (seq 200).
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	bs := []sqlc.TenantLineLedger{
		bucket(now.Add(-1*time.Hour), 500, 50),
		bucket(now.Add(-12*time.Hour), 400, 50), // inside 24h
		bucket(now.Add(-20*time.Hour), 200, 50), // inside 24h -> time boundary
		bucket(now.Add(-30*time.Hour), 100, 50), // outside 24h
		bucket(now.Add(-48*time.Hour), 1, 50),   // row cutoff was here (seq 1)
	}
	if got := applyTimeCeiling(bs, 1, 24, now); got != 200 {
		t.Fatalf("time-clamp case: got cutoff %d, want 200", got)
	}
}

func TestApplyTimeCeiling_NoWindow(t *testing.T) {
	// WindowHours 0 disables the ceiling entirely.
	now := time.Now()
	bs := []sqlc.TenantLineLedger{bucket(now.Add(-100*time.Hour), 1, 1)}
	if got := applyTimeCeiling(bs, 1, 0, now); got != 1 {
		t.Fatalf("disabled ceiling: got %d, want 1", got)
	}
}

func TestApplyTimeCeiling_AllInsideKeepsRowCutoff(t *testing.T) {
	// Everything within the window: time ceiling never exceeds the row cutoff.
	now := time.Now()
	bs := []sqlc.TenantLineLedger{
		bucket(now.Add(-1*time.Minute), 90, 10),
		bucket(now.Add(-2*time.Minute), 10, 10),
	}
	// row cutoff 10; oldest inside-24h bucket is seq 10 -> time boundary 10.
	if got := applyTimeCeiling(bs, 10, 24, now); got != 10 {
		t.Fatalf("all-inside case: got %d, want 10", got)
	}
}
