//go:build integration

// The heartbeat path after migration 009, on a database of its own: the miss
// sweep and the ping both run through the heartbeat's private probe_target,
// and the acceptance sentence holds — a missed ping opens an incident and a
// ping closes it. Run with -tags=integration, UC_TEST_POSTGRES set.
package heartbeat

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

const pingToken = "tok-integration"

func newHeartbeatWorld(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping heartbeat integration test")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("uc_g1_hb_%d", time.Now().UnixNano())
	admin, err := sql.Open("pgx", base.String())
	if err != nil {
		t.Fatal(err)
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

	db, err := sql.Open("pgx", mine.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpContext(context.Background(), db, "../../../db/postgres"); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	pool, err := pg.Open(context.Background(), mine.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	pgs := pgstore.New(pool.Raw())
	lc := incident.New(pool, pgs)
	return New(pool, pgs, lc), db
}

// seedHeartbeat builds the post-migration shape by hand: one heartbeat
// monitor on its private target, the window closed, the state ok.
func seedHeartbeat(t *testing.T, db *sql.DB) (monitorID, targetID int64) {
	t.Helper()
	stmts := []string{
		`INSERT INTO tenant (public_id, name) VALUES ('018f00d4-0000-7000-8000-000000000001', 'hb')`,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES ('018f00d4-0000-7000-8000-000000000002', 1, 'hb.example')`,
		`INSERT INTO probe_target (id, key, kind, url)
		 VALUES (101, 'heartbeat' || chr(31) || '018f00d4-0000-7000-8000-000000000003', 'heartbeat', 'hb')`,
		`INSERT INTO monitor (id, public_id, tenant_id, project_id, kind, name, target, interval_sec, grace_sec, ping_token, target_id)
		 VALUES (100, '018f00d4-0000-7000-8000-000000000003', 1, 1, 'heartbeat', 'Nightly build', 'hb', 300, 300, '` + pingToken + `', 101)`,
		`INSERT INTO target_schedule (target_id, region, next_due_at) VALUES (101, 'default', now() - interval '5 minutes')`,
		`INSERT INTO target_facts (target_id, status) VALUES (101, 'ok')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
	return 100, 101
}

func TestMissedPingOpensAndPingCloses(t *testing.T) {
	svc, db := newHeartbeatWorld(t)
	monitorID, targetID := seedHeartbeat(t, db)
	ctx := context.Background()

	// The window closed with no ping: the sweep records a miss through the
	// private target and opens the incident.
	if err := svc.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	var open int
	if err := db.QueryRow(
		`SELECT count(*) FROM incident WHERE monitor_id = $1 AND resolved_at IS NULL`, monitorID).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 1 {
		t.Fatalf("open incidents after a miss = %d, want 1", open)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM target_facts WHERE target_id = $1`, targetID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "down" {
		t.Fatalf("target_facts.status after a miss = %q, want down (threshold 1)", status)
	}
	var rows int
	if err := db.QueryRow(
		`SELECT count(*) FROM checks WHERE target_id = $1 AND NOT ok AND error_class = 'missed' AND interval_sec = 300`,
		targetID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("missed checks rows = %d, want 1", rows)
	}
	// The miss pushed the window one interval out.
	var due time.Time
	if err := db.QueryRow(`SELECT next_due_at FROM target_schedule WHERE target_id = $1`, targetID).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if due.Before(time.Now()) {
		t.Fatalf("miss window not pushed out: %v", due)
	}

	// A ping is a pass: the state recovers, the incident closes, a checks
	// row lands, and the window opens again for interval + grace.
	found, err := svc.Ping(ctx, pingToken)
	if err != nil || !found {
		t.Fatalf("Ping: found=%v err=%v", found, err)
	}
	if err := db.QueryRow(`SELECT status FROM target_facts WHERE target_id = $1`, targetID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ok" {
		t.Fatalf("status after ping = %q, want ok", status)
	}
	var closed int
	if err := db.QueryRow(
		`SELECT count(*) FROM incident WHERE monitor_id = $1 AND resolved_at IS NOT NULL AND close_reason = 'recovered'`,
		monitorID).Scan(&closed); err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("recovered incidents after ping = %d, want 1", closed)
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM checks WHERE target_id = $1 AND ok`, targetID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("ok checks rows after ping = %d, want 1", rows)
	}
	if err := db.QueryRow(`SELECT next_due_at FROM target_schedule WHERE target_id = $1`, targetID).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if want := time.Now().Add(590 * time.Second); due.Before(want) {
		t.Fatalf("ping window = %v, want at least interval+grace (300+300) out", due)
	}
}

// An unknown token is a 404, never a hint: nothing is recorded anywhere.
func TestPingUnknownToken(t *testing.T) {
	svc, db := newHeartbeatWorld(t)
	seedHeartbeat(t, db)
	found, err := svc.Ping(context.Background(), "no-such-token")
	if err != nil || found {
		t.Fatalf("unknown token: found=%v err=%v, want false/nil", found, err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM checks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("unknown token recorded %d rows", n)
	}
}
