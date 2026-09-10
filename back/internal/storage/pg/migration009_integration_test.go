//go:build integration

// Migration 009 (shared probes) against a real Postgres, on a database of its
// own: a database seeded at 008 with 400-day-old checks rows and a live
// heartbeat must come through the migration with every row accounted for.
// Run with -tags=integration, UC_TEST_POSTGRES set.
package pg

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// newTestDB creates a database of the test's own (parallel tests never
// collide) and returns its DSN; dropped on cleanup.
func newTestDB(t *testing.T) string {
	t.Helper()
	dsn := startPostgres(t)
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	name := fmt.Sprintf("uc_g1_%d", time.Now().UnixNano())
	admin, err := sql.Open("pgx", base.String())
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
		_ = admin.Close()
	})
	mine := *base
	mine.Path = "/" + name
	return mine.String()
}

// applyUpTo applies every migration up to and including version.
func applyUpTo(t *testing.T, dsn string, version int64) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(context.Background(), db, "../../../../db/postgres", version); err != nil {
		t.Fatalf("goose up to %d: %v", version, err)
	}
}

func applyAll(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpContext(context.Background(), db, "../../../../db/postgres"); err != nil {
		t.Fatalf("goose up: %v", err)
	}
}

// The scenario the plan's acceptance names: checks rows from 400 days ago and
// a live heartbeat both survive 009; the per-monitor tables are gone; every
// monitor points at a target; keys are unique.
func TestMigration009SurvivesOldChecksAndHeartbeat(t *testing.T) {
	dsn := newTestDB(t)
	applyUpTo(t, dsn, 8)
	pool := openPool(t, dsn)
	ctx := context.Background()

	seed := []string{
		`INSERT INTO tenant (public_id, name) VALUES ('018f00a1-0000-7000-8000-000000000001', 'fourhundred')`,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES ('018f00a1-0000-7000-8000-000000000002', 1, 'fourhundred.example')`,
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec)
		 VALUES ('018f00a1-0000-7000-8000-000000000003', 1, 1, 'website', 'Old', 'https://fourhundred.example', 300)`,
		`INSERT INTO monitor_schedule (monitor_id, region, next_due_at) VALUES (1, 'default', now())`,
		`INSERT INTO monitor_facts (monitor_id, status, consecutive_failures) VALUES (1, 'ok', 0)`,
		`INSERT INTO checks (tenant_id, monitor_id, ts, region, ok, status_code, error_class)
		 VALUES (1, 1, now() - interval '400 days', 'default', true, 200, 'none'),
		        (1, 1, now() - interval '2 hours',  'default', true, 200, 'none')`,
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec, grace_sec, ping_token)
		 VALUES ('018f00a1-0000-7000-8000-000000000004', 1, 1, 'heartbeat', 'Cron', 'heartbeat', 300, 300, 'tok-400d')`,
		`INSERT INTO monitor_schedule (monitor_id, region, next_due_at) VALUES (2, 'default', now() - interval '1 hour')`,
		`INSERT INTO monitor_facts (monitor_id, status) VALUES (2, 'ok')`,
	}
	for _, stmt := range seed {
		if _, err := pool.db.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	// The migration itself.
	applyAll(t, dsn)

	assert := func(query string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := pool.db.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("assert %q: %v", query, err)
		}
		return n
	}

	// Both checks rows survived, mapped to the website target, interval 300.
	if n := assert(`SELECT count(*) FROM checks`); n != 2 {
		t.Fatalf("checks rows = %d, want 2 (all survived)", n)
	}
	if n := assert(`SELECT count(*) FROM checks WHERE target_id IS NULL OR interval_sec IS NULL OR interval_sec <> 300`); n != 0 {
		t.Fatalf("checks rows unmapped or unlabelled = %d, want 0", n)
	}
	websiteTarget := assert(`SELECT target_id FROM monitor WHERE id = 1`)
	if n := assert(`SELECT count(*) FROM checks WHERE target_id = $1`, websiteTarget); n != 2 {
		t.Fatalf("checks rows on the website target = %d, want 2", n)
	}
	// The 400-day-old row has a partition of its own, and the default
	// partition exists.
	if n := assert(`SELECT count(*) FROM pg_class WHERE relname = 'checks_' || to_char((now() - interval '400 days')::date, 'YYYYMMDD')`); n != 1 {
		t.Fatalf("the 400-day-old day has no partition")
	}
	if n := assert(`SELECT count(*) FROM pg_class WHERE relname = 'checks_default'`); n != 1 {
		t.Fatalf("checks_default is missing")
	}

	// The heartbeat got a private target; its schedule and facts moved.
	heartbeatTarget := assert(`SELECT target_id FROM monitor WHERE id = 2`)
	if n := assert(`SELECT count(*) FROM probe_target WHERE id = $1 AND kind = 'heartbeat'
	                 AND key = 'heartbeat' || chr(31) || '018f00a1-0000-7000-8000-000000000004'`,
		heartbeatTarget); n != 1 {
		t.Fatalf("the heartbeat's private probe_target is missing or mis-keyed")
	}
	if n := assert(`SELECT count(*) FROM target_schedule WHERE target_id = $1 AND next_due_at < now() - interval '30 minutes'`,
		heartbeatTarget); n != 1 {
		t.Fatalf("the heartbeat's miss window did not survive")
	}
	if n := assert(`SELECT count(*) FROM target_facts WHERE target_id = $1 AND status = 'ok'`, heartbeatTarget); n != 1 {
		t.Fatalf("the heartbeat's facts did not survive")
	}

	// The website target got facts and a schedule with the lease cleared.
	if n := assert(`SELECT count(*) FROM target_facts WHERE target_id = $1 AND status = 'ok'`, websiteTarget); n != 1 {
		t.Fatalf("the website target's facts did not survive")
	}
	if n := assert(`SELECT count(*) FROM target_schedule WHERE target_id = $1 AND leased_by IS NULL`, websiteTarget); n != 1 {
		t.Fatalf("the website target's schedule did not survive with the lease cleared")
	}

	// The per-monitor tables are gone; keys are unique; every monitor points
	// somewhere.
	for _, gone := range []string{"monitor_schedule", "monitor_facts"} {
		if n := assert(`SELECT count(*) FROM pg_class WHERE relname = $1`, gone); n != 0 {
			t.Fatalf("%s still exists", gone)
		}
	}
	if n := assert(`SELECT count(*) FROM probe_target`); n != 2 {
		t.Fatalf("probe_target rows = %d, want 2", n)
	}
	if n := assert(`SELECT count(DISTINCT key) FROM probe_target`); n != 2 {
		t.Fatalf("probe_target keys are not unique: %d distinct of 2", n)
	}
	if n := assert(`SELECT count(*) FROM monitor WHERE target_id IS NULL`); n != 0 {
		t.Fatalf("a monitor has no target")
	}
}

// The same-spelling fold: www and a bare trailing slash are one target, and
// the status_page backfills find the host page.
func TestMigration009FoldsSpellingsAndBackfillsPages(t *testing.T) {
	dsn := newTestDB(t)
	applyUpTo(t, dsn, 8)
	pool := openPool(t, dsn)
	ctx := context.Background()

	seed := []string{
		`INSERT INTO tenant (public_id, name) VALUES ('018f00b2-0000-7000-8000-000000000001', 'fold')`,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES ('018f00b2-0000-7000-8000-000000000002', 1, 'www.fold.example')`,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES ('018f00b2-0000-7000-8000-000000000005', 1, 'other.fold.example')`,
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec)
		 VALUES ('018f00b2-0000-7000-8000-000000000003', 1, 1, 'website', 'A', 'fold.example', 300)`,
		// The same URL under a second spelling in a SECOND project: one project
		// may not subscribe twice (the UNIQUE), two projects share one probe.
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec)
		 VALUES ('018f00b2-0000-7000-8000-000000000004', 1, 2, 'website', 'B', 'https://WWW.Fold.example/', 300)`,
		`INSERT INTO monitor_schedule (monitor_id, region, next_due_at) VALUES (1, 'default', now()), (2, 'default', now())`,
		`INSERT INTO status_page (tenant_id, project_id, slug, title) VALUES (1, 1, 'fold-example', 'Fold')`,
	}
	for _, stmt := range seed {
		if _, err := pool.db.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	applyAll(t, dsn)

	var n int64
	var oneTarget int64
	// Two spellings, one target, one unique key.
	if err := pool.db.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT target_id) FROM monitor`).Scan(&n, &oneTarget); err != nil {
		t.Fatal(err)
	}
	if n != 2 || oneTarget != 1 {
		t.Fatalf("monitors = %d on %d targets, want 2 on 1", n, oneTarget)
	}
	// The page found its root target and is marked the host page.
	var rootID int64
	if err := pool.db.QueryRow(ctx,
		`SELECT root_target_id FROM status_page WHERE slug = 'fold-example'`).Scan(&rootID); err != nil {
		t.Fatalf("root_target_id not backfilled: %v", err)
	}
	var mTarget int64
	if err := pool.db.QueryRow(ctx, `SELECT target_id FROM monitor WHERE id = 1`).Scan(&mTarget); err != nil {
		t.Fatal(err)
	}
	if rootID != mTarget {
		t.Fatalf("root_target_id = %d, want the monitors' target %d", rootID, mTarget)
	}
	var isHost bool
	if err := pool.db.QueryRow(ctx,
		`SELECT is_host_page FROM status_page WHERE slug = 'fold-example'`).Scan(&isHost); err != nil {
		t.Fatal(err)
	}
	if !isHost {
		t.Fatal("the dashed-host slug should be marked is_host_page")
	}
}
