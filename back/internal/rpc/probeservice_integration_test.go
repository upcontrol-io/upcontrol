//go:build integration

// ProbeService.SubmitResults against a real Postgres, on a database of its
// own: shared-target checks rows, the incident fan-out by subscription set,
// results without target_id dropped and counted, could-not-measure, and the
// refusal backoff that follows it, plus Lease's pace. Run with -tags=integration,
// UC_TEST_POSTGRES set.
package rpc

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

	"connectrpc.com/connect"

	probev1 "go.upcontrol.io/back/gen/rpc/probe/v1"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

const nodeToken = "test-node-token"

type svcWorld struct {
	svc  *ProbeService
	pool *pg.Pool
	db   *sql.DB
}

func newSvcWorld(t *testing.T) *svcWorld {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping rpc integration test")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("uc_g1_rpc_%d", time.Now().UnixNano())
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
	target := mine.String()

	db, err := sql.Open("pgx", target)
	if err != nil {
		t.Fatal(err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpContext(context.Background(), db, "../../../db/postgres"); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	pool, err := pg.Open(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	pgs := pgstore.New(pool.Raw())
	lc := incident.New(pool, pgs)
	return &svcWorld{svc: NewProbeService(pool, pgs, lc, nodeToken), pool: pool, db: db}
}

func (w *svcWorld) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := w.db.Exec(query, args...); err != nil {
		t.Fatalf("exec: %v\n%s", err, query)
	}
}

func (w *svcWorld) queryInt(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := w.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// seedSubscriber: one tenant+project with a website monitor on the target.
func (w *svcWorld) seedSubscriber(t *testing.T, targetID int64, name string) int64 {
	t.Helper()
	var tenant, project, monitor int64
	if err := w.db.QueryRow(
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`, name,
	).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := w.db.QueryRow(
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'x.example') RETURNING id`, tenant,
	).Scan(&project); err != nil {
		t.Fatal(err)
	}
	if err := w.db.QueryRow(
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec, target_id)
		 VALUES (gen_random_uuid(), $1, $2, 'website', $3, 'x', 300, $4) RETURNING id`,
		tenant, project, name, targetID).Scan(&monitor); err != nil {
		t.Fatal(err)
	}
	return monitor
}

func (w *svcWorld) seedTarget(t *testing.T, url string) int64 {
	t.Helper()
	var id int64
	if err := w.db.QueryRow(
		`INSERT INTO probe_target (key, kind, url) VALUES ('website' || chr(31) || $1 || chr(31), 'website', $1) RETURNING id`,
		url).Scan(&id); err != nil {
		t.Fatal(err)
	}
	w.exec(t, `INSERT INTO target_schedule (target_id, region, next_due_at) VALUES ($1, 'default', now())`, id)
	return id
}

func (w *svcWorld) submit(t *testing.T, results ...*probev1.CheckResult) *probev1.SubmitResultsResponse {
	t.Helper()
	req := connect.NewRequest(&probev1.SubmitResultsRequest{
		NodeId: "node-1", Region: "default", Results: results,
	})
	req.Header().Set("Authorization", "Bearer "+nodeToken)
	resp, err := w.svc.SubmitResults(context.Background(), req)
	if err != nil {
		t.Fatalf("SubmitResults: %v", err)
	}
	return resp.Msg
}

func failResult(targetID uint64, n int) *probev1.CheckResult {
	return &probev1.CheckResult{
		CheckId: fmt.Sprintf("chk-%d-%d", targetID, n), TargetId: targetID,
		Ok: false, ErrorClass: probev1.ErrorClass_ERROR_CLASS_CONNECT,
	}
}

// Two subscribers on one target: one checks row per result, incidents open
// for both after the threshold, recovery closes both including the one paused
// during the outage, and a result without target_id is dropped and counted.
func TestSubmitResultsFanOutBySubscriptionSet(t *testing.T) {
	w := newSvcWorld(t)
	target := w.seedTarget(t, "https://fanout.example")
	m1 := w.seedSubscriber(t, target, "alpha")
	m2 := w.seedSubscriber(t, target, "beta")

	before := resultsDroppedNoTarget.Load()
	resp := w.submit(t,
		failResult(uint64(target), 1),
		&probev1.CheckResult{CheckId: "no-target", Ok: true}, // pre-migration wire shape: dropped
		failResult(uint64(target), 2),
		failResult(uint64(target), 3),
	)
	if resp.Accepted != 3 {
		t.Fatalf("accepted = %d, want 3 (the targetless result is not accepted)", resp.Accepted)
	}
	if after := resultsDroppedNoTarget.Load(); after != before+1 {
		t.Fatalf("dropped counter = %d, want %d", after, before+1)
	}

	// One checks row per accepted result, on the target, at the derived
	// cadence (both subscribers sit at 300 s).
	if n := w.queryInt(t, `SELECT count(*) FROM checks WHERE target_id = $1 AND interval_sec = 300 AND error_class = 'connect'`, target); n != 3 {
		t.Fatalf("checks rows = %d, want 3", n)
	}
	// Third failure crossed the threshold: both subscribers hold an open
	// incident, each stamped with the effective interval.
	if n := w.queryInt(t, `SELECT count(*) FROM incident WHERE status = 'down' AND resolved_at IS NULL AND effective_interval_sec = 300`); n != 2 {
		t.Fatalf("open incidents = %d, want 2 (one per subscriber)", n)
	}
	if n := w.queryInt(t, `SELECT count(*) FROM incident WHERE monitor_id = ANY($1::bigint[])`, fmt.Sprintf("{%d,%d}", m1, m2)); n != 2 {
		t.Fatalf("incidents on the two monitors = %d, want 2", n)
	}

	// A fourth failure while down re-opens nothing (Open is idempotent).
	w.submit(t, failResult(uint64(target), 4))
	if n := w.queryInt(t, `SELECT count(*) FROM incident WHERE resolved_at IS NULL`); n != 2 {
		t.Fatalf("open incidents after another failure = %d, want still 2", n)
	}

	// Pause beta during the outage; recovery still closes both.
	w.exec(t, `UPDATE monitor SET paused = true WHERE id = $1`, m2)
	w.submit(t, &probev1.CheckResult{
		CheckId: fmt.Sprintf("chk-%d-ok", target), TargetId: uint64(target),
		Ok: true, ErrorClass: probev1.ErrorClass_ERROR_CLASS_NONE, StatusCode: 200,
	})
	if n := w.queryInt(t, `SELECT count(*) FROM incident WHERE resolved_at IS NULL`); n != 0 {
		t.Fatalf("open incidents after recovery = %d, want 0 (paused subscribers close too)", n)
	}
	if n := w.queryInt(t, `SELECT count(*) FROM incident WHERE close_reason = 'recovered'`); n != 2 {
		t.Fatalf("recovered incidents = %d, want 2", n)
	}
	// The ok stamped first_ok_at.
	if n := w.queryInt(t, `SELECT count(*) FROM probe_target WHERE id = $1 AND first_ok_at IS NOT NULL`, target); n != 1 {
		t.Fatal("first_ok_at was not stamped by the ok result")
	}
}

// The server sets the pace from the batch itself: a full batch comes back in
// 2 s, a partial one waits 30 s, an empty queue 5 s.
func TestLeasePacesFromQueueDepth(t *testing.T) {
	w := newSvcWorld(t)
	for _, u := range []string{"https://a.example", "https://b.example", "https://c.example"} {
		w.seedSubscriber(t, w.seedTarget(t, u), u)
	}
	for _, want := range []struct {
		checks int
		pace   uint32
	}{{2, 2000}, {1, 30000}, {0, 5000}} {
		req := connect.NewRequest(&probev1.LeaseRequest{NodeId: "node-1", Region: "default", Capacity: 2})
		req.Header().Set("Authorization", "Bearer "+nodeToken)
		resp, err := w.svc.Lease(context.Background(), req)
		if err != nil {
			t.Fatalf("Lease: %v", err)
		}
		if got := len(resp.Msg.Checks); got != want.checks || resp.Msg.NextLeaseAfterMs != want.pace {
			t.Fatalf("lease = %d checks at %d ms, want %d at %d",
				got, resp.Msg.NextLeaseAfterMs, want.checks, want.pace)
		}
	}
}

// A 403 is could-not-measure: no incident, the honest facts status, a checks
// row that uptime queries exclude, first_ok_at untouched, and a refusal
// backoff that holds the target out of the lease until it expires.
func TestSubmitResultsUnmeasuredBacksOff(t *testing.T) {
	w := newSvcWorld(t)
	target := w.seedTarget(t, "https://walled.example")
	w.seedSubscriber(t, target, "walled")

	refused := func(n int) *probev1.CheckResult {
		return &probev1.CheckResult{
			CheckId: fmt.Sprintf("chk-403-%d", n), TargetId: uint64(target),
			Ok: false, StatusCode: 403, ErrorClass: probev1.ErrorClass_ERROR_CLASS_STATUS,
		}
	}
	w.submit(t, refused(1), refused(2), refused(3))

	if n := w.queryInt(t, `SELECT count(*) FROM incident`); n != 0 {
		t.Fatalf("unmeasured results opened %d incidents, want 0", n)
	}
	if n := w.queryInt(t, `SELECT count(*) FROM target_facts WHERE target_id = $1 AND status = 'could_not_measure' AND consecutive_unmeasured = 3 AND consecutive_failures = 0`, target); n != 1 {
		t.Fatal("the unmeasured streak did not land in target_facts")
	}
	if n := w.queryInt(t, `SELECT count(*) FROM probe_target WHERE id = $1 AND first_ok_at IS NULL`, target); n != 1 {
		t.Fatal("an unmeasured result must never stamp first_ok_at")
	}
	// The raw rows exist (ok=false, status as measured) and are excludable.
	if n := w.queryInt(t, `SELECT count(*) FROM checks WHERE target_id = $1 AND error_class = 'status' AND status_code = 403`, target); n != 3 {
		t.Fatalf("unmeasured checks rows = %d, want 3", n)
	}
	if n := w.queryInt(t, `SELECT count(*) FROM checks WHERE target_id = $1 AND `+pgstore.MeasurableSQL, target); n != 0 {
		t.Fatal("the measurable predicate should exclude every row of this batch")
	}

	// Backoff: refusals doubled and persisted, the target held out of the
	// lease. Pull the schedule due first so the ONLY thing excluding it is the
	// backoff.
	if n := w.queryInt(t, `SELECT count(*) FROM target_facts WHERE target_id = $1 AND consecutive_refusals = 3 AND backoff_until > now()`, target); n != 1 {
		t.Fatal("the refusal backoff was not persisted")
	}
	w.exec(t, `UPDATE target_schedule SET next_due_at = now() - interval '1 second' WHERE target_id = $1`, target)
	lease := func() int {
		rows, err := w.pool.Queries().LeaseDueTargets(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == target {
				return 1
			}
		}
		return 0
	}
	if lease() != 0 {
		t.Fatal("a target in refusal backoff was leased")
	}
	w.exec(t, `UPDATE target_facts SET backoff_until = now() - interval '1 second' WHERE target_id = $1`, target)
	if lease() != 1 {
		t.Fatal("the target did not return to the lease after the backoff expired")
	}

	// A measurable result resets the backoff. A served Retry-After floors the
	// delay above the doubling.
	throttled := &probev1.CheckResult{
		CheckId: "chk-429", TargetId: uint64(target),
		Ok: false, StatusCode: 429, ErrorClass: probev1.ErrorClass_ERROR_CLASS_STATUS,
		RetryAfterSec: 120,
	}
	w.submit(t, throttled)
	if n := w.queryInt(t, `SELECT count(*) FROM target_facts WHERE target_id = $1 AND backoff_until >= now() + interval '119 seconds'`, target); n != 1 {
		t.Fatal("Retry-After did not floor the backoff")
	}
	w.exec(t, `UPDATE target_facts SET backoff_until = NULL, consecutive_refusals = 0 WHERE target_id = $1`, target)
	w.submit(t, &probev1.CheckResult{
		CheckId: "chk-real-fail", TargetId: uint64(target),
		Ok: false, ErrorClass: probev1.ErrorClass_ERROR_CLASS_CONNECT,
	})
	if n := w.queryInt(t, `SELECT count(*) FROM target_facts WHERE target_id = $1 AND consecutive_refusals = 0 AND backoff_until IS NULL`, target); n != 1 {
		t.Fatal("a measurable result must reset the refusal backoff")
	}
}

// A bot filter's challenge is could-not-measure at any status (an AWS WAF
// CAPTCHA answers 405): no incident, the honest facts status, the 403's
// refusal backoff, and a checks row the uptime query leaves out, while an ok
// row whose error_class is NULL still counts as measured.
func TestSubmitResultsChallengeIsUnmeasured(t *testing.T) {
	w := newSvcWorld(t)
	target := w.seedTarget(t, "https://challenged.example")
	monitor := w.seedSubscriber(t, target, "challenged")

	challenged := func(n int) *probev1.CheckResult {
		return &probev1.CheckResult{
			CheckId: fmt.Sprintf("chk-405-%d", n), TargetId: uint64(target),
			Ok: false, StatusCode: 405, ErrorClass: probev1.ErrorClass_ERROR_CLASS_CHALLENGE,
		}
	}
	w.submit(t, challenged(1), challenged(2), challenged(3))

	if n := w.queryInt(t, `SELECT count(*) FROM incident`); n != 0 {
		t.Fatalf("challenge results opened %d incidents, want 0", n)
	}
	if n := w.queryInt(t, `SELECT count(*) FROM target_facts WHERE target_id = $1 AND status = 'could_not_measure' AND consecutive_unmeasured = 3 AND consecutive_failures = 0`, target); n != 1 {
		t.Fatal("the challenge streak did not land in target_facts as could_not_measure")
	}
	// The 403's doubling: 300 s * 2^3 after the third refusal.
	if n := w.queryInt(t, `SELECT count(*) FROM target_facts WHERE target_id = $1 AND consecutive_refusals = 3
		AND backoff_until BETWEEN now() + interval '2390 seconds' AND now() + interval '2410 seconds'`, target); n != 1 {
		t.Fatal("a challenge must back off exactly like a 403")
	}
	if n := w.queryInt(t, `SELECT count(*) FROM checks WHERE target_id = $1 AND NOT ok AND error_class = 'challenge' AND status_code = 405`, target); n != 3 {
		t.Fatalf("challenge checks rows = %d, want 3 (status as measured)", n)
	}

	// An ok row stored before error_class was always written: NULL, and it
	// must stay measured (a plain <> 'challenge' would drop it).
	w.exec(t, `INSERT INTO checks (target_id, interval_sec, ts, region, ok, status_code, error_class)
		VALUES ($1, 300, now(), 'default', true, 200, NULL)`, target)
	tenant := w.queryInt(t, `SELECT tenant_id FROM monitor WHERE id = $1`, monitor)
	from := time.Now().Add(-time.Hour)
	buckets, err := w.svc.pgs.CheckBuckets(context.Background(), tenant, monitor, from, time.Now().Add(time.Minute), 7200)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || buckets[0].OK != 1 || buckets[0].Total != 1 {
		t.Fatalf("uptime buckets = %+v, want one bucket of 1 ok / 1 measured (the NULL-class ok row, no challenge)", buckets)
	}
}
