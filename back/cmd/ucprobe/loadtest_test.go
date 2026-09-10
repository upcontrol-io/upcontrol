//go:build integration

// Load test for the probe worker pool (plan part 7): 500 targets seeded over
// direct SQL, every 10th hanging past the check timeout, driven through the
// SAME lease-execute-submit cycle ucprobe runs, against a real ProbeService
// over a fresh throwaway Postgres. Asserts zero lost batches (one checks row
// per leased target, the hangs measured as timeouts) and the stale-lease
// invariant (no lease held whose lease_until is past once the run settles).
// Run with -tags=integration and UC_TEST_POSTGRES set. It lives beside the
// code it proves: probeCycle, nodeAuth and the pool constants are unexported
// members of this package.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	_ "github.com/jackc/pgx/v5/stdlib"

	probev1connect "go.upcontrol.io/back/gen/rpc/probe/v1/probev1connect"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/probe/executor"
	"go.upcontrol.io/back/internal/rpc"
	"go.upcontrol.io/back/internal/storage/pg"      //nolint:depguard // test-only harness: stands up the server side; an integration test never links into the probe binary
	"go.upcontrol.io/back/internal/storage/pgstore" //nolint:depguard // same harness exception as the line above
)

const loadNodeToken = "fire7-node-token"

func TestProbePoolLoad(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping load test")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}

	// A fresh fire7_* database per run, dropped in cleanup.
	admin, err := sql.Open("pgx", base.String())
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("fire7_ucprobe_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
		_ = admin.Close()
	})
	mine := *base
	mine.Path = "/" + name
	dbURL := mine.String()

	if err := migrate.Run(context.Background(), dbURL, "../../../db/postgres"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pg.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	// The fleet every 10th target hangs on: /slow holds the request past the
	// 10 s check timeout (the executor aborts at the timeout and records it),
	// /ok answers immediately. The executor runs without the SSRF guard, the
	// way executor_test.go runs it: the guard refuses loopback, and the thing
	// under proof here is the pool and the submit path, not the guard.
	fleet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/slow") {
			select {
			case <-r.Context().Done():
			case <-time.After(12 * time.Second):
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fleet.Close)
	fleetURL, err := url.Parse(fleet.URL)
	if err != nil {
		t.Fatal(err)
	}

	// The probe API as a real connect server, so the test drives the same
	// wire path (client, node auth interceptor, probeCycle) the production
	// loop runs.
	pgs := pgstore.New(pool.Raw())
	svc := rpc.NewProbeService(pool, pgs, incident.New(pool, pgs), loadNodeToken)
	mux := http.NewServeMux()
	rpcPath, rpcHandler := probev1connect.NewProbeServiceHandler(svc)
	mux.Handle(rpcPath, rpcHandler)
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	client := probev1connect.NewProbeServiceClient(api.Client(), api.URL,
		connect.WithInterceptors(&nodeAuth{token: loadNodeToken}))

	// 500 targets, every 10th a /slow hang (10%), and one unpaused
	// subscription each so the lease query finds every target due (a target
	// with neither an unpaused subscriber nor a host page is never due).
	// Direct SQL: the API doors are not under test. The slow targets are the
	// MOST overdue, so the first lease gets all 50 at once: three waves of
	// hangs against the pool is a stronger concurrency proof than five hangs
	// scattered over ten batches, and it keeps the clock down (the pool is
	// the thing being proved, not the clock).
	const totalTargets = 500
	seed := `
WITH tn AS (
    INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), 'fire7-load') RETURNING id
), pr AS (
    INSERT INTO project (public_id, tenant_id, domain)
    SELECT gen_random_uuid(), id, 'load.test' FROM tn RETURNING id, tenant_id
), tg AS (
    INSERT INTO probe_target (key, kind, url)
    SELECT 'website' || chr(31) || u || chr(31), 'website', u
      FROM generate_series(1, ` + fmt.Sprint(totalTargets) + `) g,
           LATERAL (SELECT 'http://127.0.0.1:' || $1::text ||
                    CASE WHEN g % 10 = 0 THEN '/slow/' ELSE '/ok/' END || g::text AS u) x
    RETURNING id, url
), sc AS (
    INSERT INTO target_schedule (target_id, region, next_due_at)
    SELECT id, 'default',
           now() - (CASE WHEN url LIKE '%/slow/%' THEN interval '10 minutes'
                         ELSE interval '0' END)
      FROM tg
)
INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec, target_id)
SELECT gen_random_uuid(), pr.tenant_id, pr.id, 'website', 'fire7-' || tg.id::text, 'x', 300, tg.id
  FROM tg CROSS JOIN pr`
	if _, err := pool.Raw().Exec(context.Background(), seed, fleetURL.Port()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Drive the same cycle until the queue drains: one batch of all 50 slow
	// targets, nine fast batches, then an empty lease. The server paces a full
	// batch at 2 s (the queue-depth policy) and an empty lease at 5 s, and the
	// probe sleeps what it was told.
	log := slog.Default()
	var leasedTotal, fullBatches, cycles int
	for {
		cycles++
		if cycles > 30 {
			t.Fatalf("queue did not drain in 30 cycles (leased %d of %d)", leasedTotal, totalTargets)
		}
		ctx, cancel := context.WithTimeout(context.Background(), batchBudget)
		leased, next, err := probeCycle(ctx, client, &executor.Executor{}, "fire7-node", "default", log)
		cancel()
		if err != nil {
			t.Fatalf("cycle %d: %v", cycles, err)
		}
		leasedTotal += leased
		if leased == leaseCapacity {
			fullBatches++
			if next != 2*time.Second {
				t.Fatalf("cycle %d: full batch paced at %v, want the server's 2 s", cycles, next)
			}
		}
		if leased == 0 {
			if next != 5*time.Second {
				t.Fatalf("cycle %d: empty lease paced at %v, want the server's 5 s", cycles, next)
			}
			break
		}
	}
	if leasedTotal != totalTargets || fullBatches != totalTargets/leaseCapacity {
		t.Fatalf("leased %d targets in %d full batches, want %d in %d",
			leasedTotal, fullBatches, totalTargets, totalTargets/leaseCapacity)
	}

	queryInt := func(label, q string) int64 {
		t.Helper()
		var n int64
		if err := pool.Raw().QueryRow(context.Background(), q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		return n
	}

	// Zero lost batches: every one of the 500 leased targets holds exactly
	// one checks row.
	if n := queryInt("checks rows", `SELECT count(*) FROM checks`); n != totalTargets {
		t.Fatalf("checks rows = %d, want %d", n, totalTargets)
	}
	if n := queryInt("targets with a row", `SELECT count(DISTINCT target_id) FROM checks`); n != totalTargets {
		t.Fatalf("targets with a checks row = %d, want %d (batches were lost)", n, totalTargets)
	}
	// The hangs were measured, not lost: every /slow target timed out and the
	// rest answered 200.
	if n := queryInt("timeout rows", `SELECT count(*) FROM checks WHERE error_class = 'timeout'`); n != totalTargets/10 {
		t.Fatalf("timeout checks rows = %d, want %d", n, totalTargets/10)
	}
	if n := queryInt("ok rows", `SELECT count(*) FROM checks WHERE ok`); n != totalTargets-totalTargets/10 {
		t.Fatalf("ok checks rows = %d, want %d", n, totalTargets-totalTargets/10)
	}

	// Stale-lease invariant (plan part 7), in its stronger form: every leased
	// target was submitted and cleared, so no lease is held at all, stale or
	// not, and every target was rescheduled at its derived cadence.
	if n := queryInt("held leases", `SELECT count(*) FROM target_schedule WHERE leased_by IS NOT NULL`); n != 0 {
		t.Fatalf("held leases = %d, want 0 (every leased target was submitted)", n)
	}
	if n := queryInt("due after run", `SELECT count(*) FROM target_schedule WHERE next_due_at <= now()`); n != 0 {
		t.Fatalf("targets still due = %d, want 0 (each was rescheduled)", n)
	}
}
