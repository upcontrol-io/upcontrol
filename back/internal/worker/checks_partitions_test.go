//go:build integration

// The checks-partitions roller's horizon: max(history_days)+1 day, and a
// NULL history_days anywhere (Self-hosted) means never drop. Own database.
package worker

import (
	"context"
	"testing"

	"go.upcontrol.io/back/internal/platform/app"
)

func TestRollCheckPartitionsHorizon(t *testing.T) {
	pool := newGateWorld(t)
	ctx := context.Background()
	deps := app.Deps{Logger: quietLogger}

	// Self-hosted ships history_days NULL: the tick skips entirely - a
	// day-named partition from far past the widest horizon must survive
	// (day-named, so the roller could read it; unreadable operator names are
	// never dropped by design).
	if _, err := pool.Raw().Exec(ctx,
		`CREATE TABLE checks_20250101 PARTITION OF checks FOR VALUES FROM ('2025-01-01') TO ('2025-01-02')`); err != nil {
		t.Fatalf("seed old partition: %v", err)
	}
	rollCheckPartitions(ctx, pool, deps)
	var old int
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM pg_inherits WHERE inhparent = 'checks'::regclass AND inhrelid = 'checks_20250101'::regclass`).Scan(&old); err != nil || old != 1 {
		t.Fatalf("the old partition survived under the NULL plan (rows = %d, err %v), want 1", old, err)
	}

	// With every plan bounded (max 365), the horizon is 366 days: the
	// 2025-01-01 partition is far past it and drops; recent days exist ahead.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE plan_entitlement SET history_days = 365 WHERE history_days IS NULL`); err != nil {
		t.Fatal(err)
	}
	rollCheckPartitions(ctx, pool, deps)
	var gone *string
	if err := pool.Raw().QueryRow(ctx,
		`SELECT to_regclass('checks_20250101')::text`).Scan(&gone); err != nil || gone != nil {
		t.Fatalf("the 400-day-old partition survived a bounded horizon (regclass = %v, err %v), want dropped", gone, err)
	}
	var ahead int
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		  WHERE i.inhparent = 'checks'::regclass AND c.relname >= 'checks_' || to_char(now() + interval '2 days', 'YYYYMMDD')`).Scan(&ahead); err != nil || ahead < 1 {
		t.Fatalf("partitions created ahead = %d (err %v), want at least one future day", ahead, err)
	}
}
