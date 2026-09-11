//go:build integration

// One plan-capacity run end to end: the budget pauses a down check and its
// incident closes as plan_paused, and a project the plan thaws delivers the
// outage it recorded while frozen. Run with -tags=integration,
// UC_TEST_POSTGRES set.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

func TestPlanCapacityClosesAndDelivers(t *testing.T) {
	pool := openReaperDB(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	scan := func(what, sql string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := pool.Raw().QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
		return id
	}
	tenantID := scan("tenant", `INSERT INTO tenant (public_id, name, plan) VALUES (gen_random_uuid(), $1, 'Free') RETURNING id`,
		fmt.Sprintf("capacity-%d", uniq))
	project := func(domain, frozenAt string) int64 {
		return scan("project", `INSERT INTO project (public_id, tenant_id, domain, created_at, frozen_at)
			VALUES (gen_random_uuid(), $1, $2, now() - interval '30 days', `+frozenAt+`) RETURNING id`, tenantID, domain)
	}
	live := project("live.example", "NULL")
	frozen := project("frozen.example", "now()")
	monitor := func(projectID int64, name string, createdAgo time.Duration) int64 {
		url := fmt.Sprintf("https://%s-%d.example", name, uniq)
		target := scan("target", `INSERT INTO probe_target (key, kind, url) VALUES ($1, 'website', $1) RETURNING id`, url)
		return scan("monitor", `INSERT INTO monitor (public_id, tenant_id, project_id, target_id, kind, name, target, interval_sec, created_at)
			VALUES (gen_random_uuid(), $1, $2, $3, 'http', $4, $5, 60, now() - make_interval(secs => $6)) RETURNING id`,
			tenantID, projectID, target, name, url, createdAgo.Seconds())
	}
	pgs := pgstore.New(pool.Raw())
	lc := incident.New(pool, pgs)
	open := func(monitorID int64) int64 {
		t.Helper()
		id, created, err := lc.Open(ctx, monitorID, "down", 0)
		if err != nil || !created {
			t.Fatalf("open: created=%v err=%v", created, err)
		}
		return id
	}
	run := func() { PlanCapacity(ctx, pgs, lc, slog.New(slog.DiscardHandler)) }

	// Free runs three checks: the newest of four is paused, and the outage it
	// was in the middle of closes as plan_paused, not as a recovery.
	for i := range 3 {
		monitor(live, fmt.Sprintf("kept%d", i), time.Duration(10-i)*time.Hour)
	}
	newest := monitor(live, "newest", time.Minute)
	downIncident := open(newest)
	run()
	var reason *string
	if err := pool.Raw().QueryRow(ctx,
		`SELECT close_reason FROM incident WHERE id = $1`, downIncident).Scan(&reason); err != nil ||
		reason == nil || *reason != incident.ReasonPlanPaused {
		t.Fatalf("the paused check's incident close_reason=%v err=%v, want %s", reason, err, incident.ReasonPlanPaused)
	}

	// The frozen project records an outage and delivers nothing; the plan
	// that carries it thaws it and the outage is delivered on that run.
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, project_id, kind, target, notify)
		 VALUES (gen_random_uuid(), $1, $2, 'email', $3, '{"websiteDown": true}')`,
		tenantID, frozen, fmt.Sprintf("capacity-%d@example.com", uniq)); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	frozenIncident := open(monitor(frozen, "frozen", time.Hour))
	queued := func() int {
		t.Helper()
		var n int
		if err := pool.Raw().QueryRow(ctx,
			`SELECT count(*) FROM delivery_queue WHERE incident_id = $1 AND class = 'page'`, frozenIncident).Scan(&n); err != nil {
			t.Fatalf("count queued: %v", err)
		}
		return n
	}
	run()
	if n := queued(); n != 0 {
		t.Fatalf("%d pages queued while the project is frozen", n)
	}
	if _, err := pool.Raw().Exec(ctx, `UPDATE tenant SET plan = 'Indie' WHERE id = $1`, tenantID); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	run()
	if n := queued(); n != 1 {
		t.Fatalf("%d pages queued after the thaw, want exactly 1", n)
	}
}
