//go:build integration

// The plan-capacity sweepers (docs/plans/trial-and-freeze.md) against a real
// Postgres: FreezeSweep keeps the most recently active plan's-worth of
// projects live and freezes the rest, restores them when the plan lifts, and
// MonitorBudgetSweep pauses HTTP monitors past http_checks (oldest kept,
// owner's own pauses untouched, heartbeat exempt). UC_TEST_POSTGRES runs the
// same statements the hourly jobs run without a container; unset, it falls
// back to the package's testcontainers harness.

package pgstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.upcontrol.io/back/internal/migrate"
)

// sweepStore applies migrations on the env-provided database when present,
// else the container harness. One database per package test run is enough:
// every test seeds its own uniquely-named tenant.
func sweepStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	if dsn := os.Getenv("UC_TEST_POSTGRES"); dsn != "" {
		// goose, not applyMigration: the env database is shared with the api
		// package's runs and already carries the ladder, and goose brings it
		// to head idempotently where applyMigration would re-CREATE.
		if err := migrate.Run(context.Background(), dsn, "../../../../db/postgres"); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Fatalf("pgxpool.New: %v", err)
		}
		t.Cleanup(pool.Close)
		if err := pool.Ping(context.Background()); err != nil {
			t.Fatalf("ping: %v", err)
		}
		return New(pool), pool
	}
	return openStore(t)
}

// seedSweepTenant: a tenant on plan with projects holding the given domains.
func seedSweepTenant(t *testing.T, pool *pgxpool.Pool, plan string, domains ...string) int64 {
	t.Helper()
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	var ownerID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Sweep') RETURNING id`,
		fmt.Sprintf("sweep-%d@example.com", uniq)).Scan(&ownerID); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	var tenantID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, plan, owner_person_id) VALUES (gen_random_uuid(), $1, $2, $3) RETURNING id`,
		fmt.Sprintf("sweep-%d", uniq), plan, ownerID).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	for _, d := range domains {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2)`,
			tenantID, d); err != nil {
			t.Fatalf("seed project %s: %v", d, err)
		}
	}
	return tenantID
}

// touchProject writes one event row: the sweep's activity signal.
func touchProject(t *testing.T, pool *pgxpool.Pool, tenantID int64, domain string, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO events (tenant_id, project_id, ts, name, labels)
		 SELECT $1, p.id, $3, 'sweep_touch', '{}'::jsonb FROM project p
		  WHERE p.tenant_id = $1 AND p.domain = $2`,
		tenantID, domain, at)
	if err != nil {
		t.Fatalf("touch %s: %v", domain, err)
	}
}

// frozenDomains: the tenant's frozen project domains, in id order.
func frozenDomains(t *testing.T, pool *pgxpool.Pool, tenantID int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT domain FROM project WHERE tenant_id = $1 AND frozen_at IS NOT NULL ORDER BY id`, tenantID)
	if err != nil {
		t.Fatalf("read frozen: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, d)
	}
	return out
}

// A Free tenant with three projects keeps the one with the newest activity
// live and freezes the other two; promotion by activity, not by age, is the
// rule — the pet project created first is not the one that stays up.
func TestFreezeSweepKeepsMostActiveProject(t *testing.T) {
	s, pool := sweepStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	uniq := time.Now().UnixNano()
	old := fmt.Sprintf("old-%d.example.com", uniq)
	pet := fmt.Sprintf("pet-%d.example.com", uniq)
	main := fmt.Sprintf("main-%d.example.com", uniq)
	tenant := seedSweepTenant(t, pool, "Free", old, pet, main)
	touchProject(t, pool, tenant, old, now.Add(-72*time.Hour))
	touchProject(t, pool, tenant, main, now.Add(-1*time.Hour))

	if _, err := s.FreezeSweep(ctx); err != nil {
		t.Fatalf("FreezeSweep: %v", err)
	}
	got := frozenDomains(t, pool, tenant)
	if len(got) != 2 || got[0] != old || got[1] != pet {
		t.Fatalf("frozen = %v, want [old pet] with main live", got)
	}

	// The live project's deletion promotes the next most active snapshot.
	if _, err := pool.Exec(ctx,
		`DELETE FROM project WHERE tenant_id = $1 AND domain = $2`, tenant, main); err != nil {
		t.Fatalf("delete live project: %v", err)
	}
	if _, err := s.FreezeSweep(ctx); err != nil {
		t.Fatalf("FreezeSweep promote: %v", err)
	}
	got = frozenDomains(t, pool, tenant)
	if len(got) != 1 || got[0] != old {
		t.Fatalf("frozen after promote = %v, want [old] with pet live", got)
	}

	// The plan lifting the cap thaws every snapshot.
	if _, err := pool.Exec(ctx, `UPDATE tenant SET plan = 'Growth' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("upgrade tenant: %v", err)
	}
	if _, err := s.FreezeSweep(ctx); err != nil {
		t.Fatalf("FreezeSweep thaw: %v", err)
	}
	if got = frozenDomains(t, pool, tenant); len(got) != 0 {
		t.Fatalf("frozen after upgrade = %v, want none", got)
	}
}

// The budget sweep pauses past http_checks oldest-first, never touches an
// owner's own pause, skips heartbeats, and unpauses exactly its own markers
// when the plan lifts.
func TestMonitorBudgetSweepPausesAndRestores(t *testing.T) {
	s, pool := sweepStore(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	domain := fmt.Sprintf("budget-%d.example.com", uniq)
	// probe_target keys are globally unique: carry the run's uniq so the
	// shared test database survives reruns.
	tenant := seedSweepTenant(t, pool, "Free", domain)
	var projectID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM project WHERE tenant_id = $1 AND domain = $2`, tenant, domain).Scan(&projectID); err != nil {
		t.Fatalf("read project: %v", err)
	}

	// Five monitors created in order: m1..m5, each on its own probe_target
	// (0.28.0: a monitor is a subscription). m2 is the owner's own pause, hb a
	// heartbeat (never counted). Free runs 3.
	for i := 1; i <= 5; i++ {
		if _, err := pool.Exec(ctx,
			`WITH tgt AS (INSERT INTO probe_target (key, kind, url)
			               VALUES ($5, 'website', $5) RETURNING id)
			 INSERT INTO monitor (public_id, tenant_id, project_id, target_id, kind, name, target, interval_sec, created_at)
			 SELECT gen_random_uuid(), $1, $2, tgt.id, 'website', $3, $5, 60, now() - ($4 || ' minutes')::interval FROM tgt`,
			tenant, projectID, fmt.Sprintf("m%d", i), fmt.Sprint(100-i), fmt.Sprintf("https://x%d-%d.example.com", i, uniq)); err != nil {
			t.Fatalf("seed monitor m%d: %v", i, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`WITH tgt AS (INSERT INTO probe_target (key, kind, url)
		               VALUES ($3, 'heartbeat', 'hb') RETURNING id)
		 INSERT INTO monitor (public_id, tenant_id, project_id, target_id, kind, name, target, interval_sec)
		 SELECT gen_random_uuid(), $1, $2, tgt.id, 'heartbeat', 'hb', 'hb', 60 FROM tgt`,
		tenant, projectID, fmt.Sprintf("hb-%d", uniq)); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}
	// The owner's own pause: paused before the sweep ever ran.
	if _, err := pool.Exec(ctx,
		`UPDATE monitor SET paused = true WHERE tenant_id = $1 AND name = 'm2'`, tenant); err != nil {
		t.Fatalf("owner pause: %v", err)
	}

	if _, err := s.MonitorBudgetSweep(ctx); err != nil {
		t.Fatalf("MonitorBudgetSweep: %v", err)
	}
	assertPaused := func(want map[string]string, why string) {
		t.Helper()
		rows, err := pool.Query(ctx,
			`SELECT name, paused::text, COALESCE(paused_by, 'owner') FROM monitor
			  WHERE tenant_id = $1 ORDER BY name`, tenant)
		if err != nil {
			t.Fatalf("read monitors: %v", err)
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var name, paused, by string
			if err := rows.Scan(&name, &paused, &by); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[name] = paused + "/" + by
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d rows, want %d", why, len(got), len(want))
		}
		for name, w := range want {
			if got[name] != w {
				t.Fatalf("%s: %s = %s, want %s (all: %v)", why, name, got[name], w, got)
			}
		}
	}
	// Oldest kept running: m1, m2 (owner-paused, still holding its slot),
	// m3. The sweep pauses m4 and m5; the heartbeat is out of the axis.
	assertPaused(map[string]string{
		"m1": "false/owner", "m2": "true/owner", "m3": "false/owner",
		"m4": "true/plan", "m5": "true/plan", "hb": "false/owner",
	}, "Free budget pause")

	// The plan lifting the cap unpauses exactly the sweeper's own markers.
	if _, err := pool.Exec(ctx, `UPDATE tenant SET plan = 'Agency' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("upgrade tenant: %v", err)
	}
	if _, err := s.MonitorBudgetSweep(ctx); err != nil {
		t.Fatalf("MonitorBudgetSweep restore: %v", err)
	}
	assertPaused(map[string]string{
		"m1": "false/owner", "m2": "true/owner", "m3": "false/owner",
		"m4": "false/owner", "m5": "false/owner", "hb": "false/owner",
	}, "Agency restore keeps the owner's pause")
}

// A frozen project's rollup rows survive TrimHistory — the snapshot promise.
func TestTrimHistorySparesFrozenProjects(t *testing.T) {
	s, pool := sweepStore(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	liveD := fmt.Sprintf("trim-live-%d.example.com", uniq)
	frozenD := fmt.Sprintf("trim-frozen-%d.example.com", uniq)
	tenant := seedSweepTenant(t, pool, "Free", liveD, frozenD)
	touchProject(t, pool, tenant, liveD, time.Now())
	touchProject(t, pool, tenant, frozenD, time.Now())
	if _, err := pool.Exec(ctx,
		`UPDATE project SET frozen_at = now() WHERE tenant_id = $1 AND domain = $2`, tenant, frozenD); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	oldHour := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	for _, d := range []string{liveD, frozenD} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO series_1h (tenant_id, project_id, hour, service, level, fingerprint, lines)
			 SELECT $1, p.id, $2, 'api', 'info', 0, 1 FROM project p WHERE p.tenant_id = $1 AND p.domain = $3`,
			tenant, oldHour, d); err != nil {
			t.Fatalf("seed series_1h %s: %v", d, err)
		}
	}
	if _, err := s.TrimHistory(ctx); err != nil {
		t.Fatalf("TrimHistory: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM series_1h s JOIN project p ON p.id = s.project_id
		  WHERE p.tenant_id = $1 AND p.domain = $2`, tenant, frozenD).Scan(&n); err != nil || n != 1 {
		t.Fatalf("frozen rollup rows = %d (err %v), want 1: the snapshot must not trim", n, err)
	}
}
