// Package cutoff computes the ring boundaries: cutoff_seq (visibility, N rows
// back) and retain_seq (deletion, N × R) from the tenant_line_ledger.
package cutoff

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/storage/pg"
)

// Entitlement is the plan's ring parameters.
type Entitlement struct {
	WindowLines int64   // N: how many lines are visible
	WindowHours int     // T: the time ceiling on the window
	RetainMult  float64 // R: retain_seq = N × R (R=2 for Free, 1 for paid)
}

// PlanEntitlements maps each plan to its ring parameters (mirrors the
// plan_entitlement table from db/postgres/001_init.sql).
var PlanEntitlements = map[string]Entitlement{
	"Free":   {WindowLines: 25000, WindowHours: 24, RetainMult: 2.0},
	"Indie":  {WindowLines: 150000, WindowHours: 48, RetainMult: 1.0},
	"Growth": {WindowLines: 3000000, WindowHours: 168, RetainMult: 1.0},
	"Agency": {WindowLines: 45000000, WindowHours: 720, RetainMult: 1.0},
}

// Result is what the recomputation produces.
type Result struct {
	CutoffSeq   int64
	RetainSeq   int64
	WindowHours float64
}

// Recompute walks the ledger backward for one project, deriving the two
// boundaries; idempotent and safe to run from either instance.
func Recompute(ctx context.Context, pool *pg.Pool, projectID int64, ent Entitlement) (*Result, error) {
	q := pool.Queries()

	// Walk the ledger backward from now, summing rows.
	buckets, err := q.LedgerBucketsBackward(ctx, sqlc.LedgerBucketsBackwardParams{
		ProjectID: projectID,
		Until:     pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		return nil, err
	}

	// Walk forward through the buckets (oldest first) to find the cutoff point
	// where cumulative rows reach WindowLines.
	var cutoffSeq, retainSeq int64
	var cumRows int64
	var windowHours float64
	cutoffFound := false
	retainFound := false

	retainLines := int64(float64(ent.WindowLines) * ent.RetainMult)

	for i := len(buckets) - 1; i >= 0; i-- {
		b := buckets[i]
		cumRows += b.Rows

		if !cutoffFound && cumRows >= ent.WindowLines {
			cutoffSeq = b.MinSeq
			cutoffFound = true
		}
		if !retainFound && cumRows >= retainLines {
			retainSeq = b.MinSeq
			retainFound = true
			// WindowHours: the span from the oldest bucket to the newest.
			if len(buckets) > 0 {
				oldest := buckets[len(buckets)-1].Bucket5m
				newest := buckets[0].Bucket5m
				if oldest.Valid && newest.Valid {
					windowHours = newest.Time.Sub(oldest.Time).Hours()
				}
			}
			break
		}
	}

	// If the ledger doesn't reach the window line count, the cutoff is 0
	// (everything is visible) and retain is 0 (nothing displaced).
	if !cutoffFound {
		cutoffSeq = 0
	}
	if !retainFound {
		retainSeq = 0
	}

	// Time ceiling: the window is min(N rows, T hours); clamp cutoff_seq to the
	// time boundary when the row-count walk reached back past WindowHours.
	cutoffSeq = applyTimeCeiling(buckets, cutoffSeq, ent.WindowHours, time.Now())

	return &Result{
		CutoffSeq:   cutoffSeq,
		RetainSeq:   retainSeq,
		WindowHours: windowHours,
	}, nil
}

// applyTimeCeiling implements the T-hours half of min(N rows, T hours); pure
// (clock injected) so the boundary logic is unit-tested directly.
func applyTimeCeiling(buckets []sqlc.TenantLineLedger, rowCutoffSeq int64, windowHours int, now time.Time) int64 {
	if windowHours <= 0 {
		return rowCutoffSeq
	}
	windowStart := now.Add(-time.Duration(windowHours) * time.Hour)
	var timeCutoffSeq int64
	for i := len(buckets) - 1; i >= 0; i-- { // oldest -> newest
		b := buckets[i]
		if b.Bucket5m.Valid && !b.Bucket5m.Time.Before(windowStart) {
			timeCutoffSeq = b.MinSeq // oldest bucket still inside the window
			break
		}
	}
	if timeCutoffSeq > rowCutoffSeq {
		return timeCutoffSeq
	}
	return rowCutoffSeq
}
