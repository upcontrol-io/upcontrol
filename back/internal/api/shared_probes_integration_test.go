//go:build integration

// A check is a subscription (plan part 1): two tenants creating a monitor on
// one URL share one probe_target and one schedule row; a subscriber joining a
// down target gets its incident with the insert; the pulls; target and
// keyword are immutable. Needs Postgres: -tags=integration, UC_TEST_POSTGRES.
// Own database per test, like the host-page lane.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/detect/availability"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// newSharedWorld: a private migrated database, the monitors routes, and one
// account (tenant + project + owner session) per requested seat.
func newSharedWorld(t *testing.T, seats int) (*pg.Pool, http.Handler, []http.Cookie) {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping shared-probe integration test")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("uc_g2_shared_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(context.Background(), base.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	mine := *base
	mine.Path = "/" + name
	db, err := openGoose(mine.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	pool, err := pg.Open(context.Background(), mine.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sm := session.New(pool, session.DefaultTTL, nil)
	mon := NewMonitors(pool, pgstore.New(pool.Raw()), sm, "")
	wa := NewWriteAPI(pool, nil, sm, false, nil, nil, false, "")
	mux := http.NewServeMux()
	mux.Handle("POST /v1/monitors", mon)
	mux.Handle("PATCH /v1/monitors/{id}", mon)
	mux.Handle("DELETE /v1/monitors/{id}", mon)
	mux.Handle("GET /v1/monitors", mon)
	mux.Handle("POST /public/watch", wa)
	mux.Handle("GET /public/status/{slug}", wa)

	ctx := context.Background()
	var cookies []http.Cookie
	for i := 0; i < seats; i++ {
		var personID, tenantID, projectID int64
		if err := pool.Raw().QueryRow(ctx,
			`INSERT INTO person (public_id, email) VALUES (gen_random_uuid(), $1) RETURNING id`,
			fmt.Sprintf("seat-%d-%d@example.com", i, time.Now().UnixNano())).Scan(&personID); err != nil {
			t.Fatal(err)
		}
		if err := pool.Raw().QueryRow(ctx,
			`INSERT INTO tenant (public_id, name, owner_person_id) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
			fmt.Sprintf("seat-%d", i), personID).Scan(&tenantID); err != nil {
			t.Fatal(err)
		}
		if err := pool.Raw().QueryRow(ctx,
			`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, '') RETURNING id`,
			tenantID).Scan(&projectID); err != nil {
			t.Fatal(err)
		}
		token, err := sm.Create(ctx, personID, tenantID, &projectID)
		if err != nil {
			t.Fatal(err)
		}
		cookies = append(cookies, http.Cookie{Name: session.CookieName, Value: token})
	}
	return pool, mux, cookies
}

func openGoose(dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := goose.SetDialect("postgres"); err != nil {
		return nil, err
	}
	if err := goose.UpContext(context.Background(), db, "../../../db/postgres"); err != nil {
		return nil, err
	}
	return db, nil
}

// createMonitor drives POST /v1/monitors with a seat's cookie.
func createMonitor(t *testing.T, h http.Handler, cookie http.Cookie, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/monitors", strings.NewReader(body))
	r.AddCookie(&cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var resp map[string]any
	if w.Code == http.StatusCreated {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode create body %q: %v", w.Body.String(), err)
		}
	}
	return w, resp
}

// Two tenants create a monitor on the same URL: one probe_target, one
// target_schedule row, two monitor rows - and the second create is
// idempotent from the SAME project (same row back, no duplicate).
func TestTwoTenantsShareOneProbeTarget(t *testing.T) {
	pool, route, cookies := newSharedWorld(t, 2)
	target := fmt.Sprintf("https://shared-%d.example.com/checkout", time.Now().UnixNano()%100000)

	for i, c := range cookies {
		w, _ := createMonitor(t, route, c, `{"type":"website","name":"Shop","target":"`+target+`","interval":"5m"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %d = %d (%s), want 201", i, w.Code, w.Body.String())
		}
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM probe_target WHERE url = $1`, target); n != 1 {
		t.Fatalf("probe_target rows for %s = %d, want 1 (one URL, one probe)", target, n)
	}
	if n := oneInt(t, pool,
		`SELECT count(*) FROM target_schedule ts WHERE ts.target_id = (SELECT id FROM probe_target WHERE url = $1)`,
		target); n != 1 {
		t.Fatalf("target_schedule rows = %d, want 1", n)
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM monitor WHERE target = $1`, target); n != 2 {
		t.Fatalf("monitor rows = %d, want 2 (two subscriptions)", n)
	}

	// A second create of the same (project, target) answers the existing row:
	// recreating a check is free.
	w, resp := createMonitor(t, route, cookies[0], `{"type":"website","name":"Shop again","target":"`+target+`","interval":"5m"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("idempotent create = %d (%s), want 201", w.Code, w.Body.String())
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM monitor WHERE target = $1`, target); n != 2 {
		t.Fatalf("monitor rows after the idempotent create = %d, want still 2", n)
	}
	if resp["id"] == nil {
		t.Fatal("the idempotent create answered no id")
	}
	// Different spellings fold: www + trailing slash + uppercase host are the
	// same fetch.
	folded := strings.Replace(target, "https://shared-", "https://WWW.Shared-", 1) + "/"
	w, _ = createMonitor(t, route, cookies[0], `{"type":"website","name":"Folded","target":"`+folded+`","interval":"5m"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("folded create = %d (%s), want 201", w.Code, w.Body.String())
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM probe_target WHERE url = $1`, target); n != 1 {
		t.Fatalf("probe_target rows after the folded spelling = %d, want still 1", n)
	}
}

// A third project subscribing while the target's facts say down gets its
// incident at once (same transaction as the insert), and a paused subscriber
// that is unpaused onto the down target gets its own incident too.
func TestSubscribingOntoADownTargetOpensTheIncidentNow(t *testing.T) {
	pool, route, cookies := newSharedWorld(t, 2)
	ctx := context.Background()
	target := fmt.Sprintf("https://down-%d.example.com", time.Now().UnixNano()%100000)

	w, first := createMonitor(t, route, cookies[0], `{"type":"website","name":"First","target":"`+target+`","interval":"5m"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("first create = %d (%s)", w.Code, w.Body.String())
	}
	// The outage predates the second subscriber: facts down, newest row a
	// timeout taken at 300 s.
	var targetID int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT id FROM probe_target WHERE url = $1`, target).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO target_facts (target_id, status, consecutive_failures) VALUES ($1, 'down', 3)`, targetID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO checks (target_id, interval_sec, ts, region, ok, status_code, error_class, total_ms)
		 VALUES ($1, 300, now() - interval '2 minutes', 'default', false, 0, 'timeout', 8000)`, targetID); err != nil {
		t.Fatal(err)
	}

	w2, _ := createMonitor(t, route, cookies[1], `{"type":"website","name":"Second","target":"`+target+`","interval":"5m"}`)
	if w2.Code != http.StatusCreated {
		t.Fatalf("second create = %d (%s)", w2.Code, w2.Body.String())
	}
	var openIncidents int
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM incident i JOIN monitor m ON m.id = i.monitor_id
		  WHERE i.resolved_at IS NULL AND m.target_id = $1`, targetID).Scan(&openIncidents); err != nil {
		t.Fatal(err)
	}
	_ = first
	if openIncidents != 1 {
		t.Fatalf("open incidents after the late subscribe = %d, want 1 (the first subscribed while the target was still nodata)", openIncidents)
	}
	// Unpausing onto the down target opens the incident in the same
	// transaction: the first subscriber PAUSES, then comes back mid-outage.
	firstPub := first["id"].(string)
	if w := patchMonitor(t, route, cookies[0], firstPub, `{"paused":true}`); w.Code != http.StatusOK {
		t.Fatalf("pause first = %d (%s)", w.Code, w.Body.String())
	}
	if w := patchMonitor(t, route, cookies[0], firstPub, `{"paused":false}`); w.Code != http.StatusOK {
		t.Fatalf("unpause first onto the down target = %d (%s)", w.Code, w.Body.String())
	}
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM incident i JOIN monitor m ON m.id = i.monitor_id
		  WHERE i.resolved_at IS NULL AND m.target_id = $1`, targetID).Scan(&openIncidents); err != nil {
		t.Fatal(err)
	}
	if openIncidents != 2 {
		t.Fatalf("open incidents after the unpause-onto-down = %d, want 2 (both subscribers hold one)", openIncidents)
	}
	// The late subscriber's incident carries the outage's evidence in its
	// title (triage over the newest row: timeout).
	var title string
	if err := pool.Raw().QueryRow(ctx,
		`SELECT i.title FROM incident i JOIN monitor m ON m.id = i.monitor_id
		  WHERE i.resolved_at IS NULL AND m.target_id = $1 AND m.name = 'Second'`, targetID).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(title, "timed out") {
		t.Fatalf("late subscriber's incident title = %q, want the timeout verdict from the newest row", title)
	}
	// Both incidents record the effective interval at opening.
	var stamped int
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM incident i JOIN monitor m ON m.id = i.monitor_id
		  WHERE m.target_id = $1 AND i.effective_interval_sec = 300`, targetID).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != 2 {
		t.Fatalf("incidents stamped with the effective interval = %d, want 2", stamped)
	}
	// Recovery closes every subscription, paused or not (the fan-out is the
	// rpc's; here: close through the monitors' own paths).
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE target_facts SET status = 'ok', consecutive_failures = 0 WHERE target_id = $1`, targetID); err != nil {
		t.Fatal(err)
	}
	if err := closeIncidentsForTarget(ctx, pool, targetID); err != nil {
		t.Fatal(err)
	}
	var openAfter int
	_ = pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM incident i JOIN monitor m ON m.id = i.monitor_id
		  WHERE i.resolved_at IS NULL AND m.target_id = $1`, targetID).Scan(&openAfter)
	if openAfter != 0 {
		t.Fatalf("open incidents after recovery = %d, want 0", openAfter)
	}
}

// closeIncidentsForTarget mirrors the recovery close for this test's sake:
// every subscription on the target, paused or not.
func closeIncidentsForTarget(ctx context.Context, pool *pg.Pool, targetID int64) error {
	rows, err := pool.Raw().Query(ctx, `SELECT id FROM monitor WHERE target_id = $1`, targetID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if _, err := pool.Raw().Exec(ctx,
			`UPDATE incident SET resolved_at = now(), status = 'ok', close_reason = 'recovered'
			  WHERE monitor_id = $1 AND resolved_at IS NULL`, id); err != nil {
			return err
		}
	}
	return nil
}

// Subscribing pulls next_due_at to now; unpausing and lowering the interval
// do too; pausing does not.
func TestThePulls(t *testing.T) {
	pool, route, cookies := newSharedWorld(t, 1)
	ctx := context.Background()
	target := fmt.Sprintf("https://pull-%d.example.com", time.Now().UnixNano()%100000)

	var targetID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO probe_target (key, kind, url) VALUES ($1, 'website', $2) RETURNING id`,
		"website\x1f"+target+"\x1f", target).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO target_schedule (target_id, region, next_due_at)
		 VALUES ($1, 'default', now() + interval '14 minutes')`, targetID); err != nil {
		t.Fatal(err)
	}

	// Free's floor is 5m: the interval-lowering arm needs a paid floor, so the
	// seat rides Indie (min_interval 60).
	if _, err := pool.Raw().Exec(ctx, `UPDATE tenant SET plan = 'Indie'`); err != nil {
		t.Fatal(err)
	}

	patch := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "/v1/monitors/"+monitorPubIDFor(t, pool, target), nil)
		r.Body = http.NoBody
		r = httptest.NewRequest(http.MethodPatch, "/v1/monitors/"+monitorPubIDFor(t, pool, target), strings.NewReader(body))
		r.AddCookie(&cookies[0])
		w := httptest.NewRecorder()
		route.ServeHTTP(w, r)
		return w
	}
	dueIn := func() time.Duration {
		var due time.Time
		if err := pool.Raw().QueryRow(ctx,
			`SELECT next_due_at FROM target_schedule WHERE target_id = $1`, targetID).Scan(&due); err != nil {
			t.Fatal(err)
		}
		return time.Until(due)
	}

	// Subscribe: due in 14 minutes becomes due now.
	w, _ := createMonitor(t, route, cookies[0], `{"type":"website","name":"Pull","target":"`+target+`","interval":"5m"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	if d := dueIn(); d > time.Minute {
		t.Fatalf("after subscribe the target is due in %v, want now-ish", d)
	}
	// Push it back out by hand, then pause: no pull.
	pushOut := func() {
		if _, err := pool.Raw().Exec(ctx,
			`UPDATE target_schedule SET next_due_at = now() + interval '14 minutes' WHERE target_id = $1`, targetID); err != nil {
			t.Fatal(err)
		}
	}
	pushOut()
	if w := patch(`{"paused":true}`); w.Code != http.StatusOK {
		t.Fatalf("pause = %d (%s)", w.Code, w.Body.String())
	}
	if d := dueIn(); d < 10*time.Minute {
		t.Fatalf("a pause pulled the target due (due in %v, want ~14m)", d)
	}
	// Unpause: pulled.
	if w := patch(`{"paused":false}`); w.Code != http.StatusOK {
		t.Fatalf("unpause = %d (%s)", w.Code, w.Body.String())
	}
	if d := dueIn(); d > time.Minute {
		t.Fatalf("after unpause the target is due in %v, want now-ish", d)
	}
	// Interval lowered below the old value: pulled.
	pushOut()
	if w := patch(`{"interval":"1m"}`); w.Code != http.StatusOK {
		t.Fatalf("interval patch = %d (%s)", w.Code, w.Body.String())
	}
	if d := dueIn(); d > time.Minute {
		t.Fatalf("after lowering the interval the target is due in %v, want now-ish", d)
	}
	// Interval raised: NOT pulled.
	pushOut()
	if w := patch(`{"interval":"30m"}`); w.Code != http.StatusOK {
		t.Fatalf("interval patch = %d (%s)", w.Code, w.Body.String())
	}
	if d := dueIn(); d < 10*time.Minute {
		t.Fatalf("raising the interval pulled the target due (due in %v, want ~14m)", d)
	}
}

// PATCH target or keyword is refused with 400 target_immutable: the row
// would show one URL and measure another.
func TestPatchTargetAndKeywordAreImmutable(t *testing.T) {
	pool, route, cookies := newSharedWorld(t, 1)
	target := fmt.Sprintf("https://immutable-%d.example.com", time.Now().UnixNano()%100000)
	w, _ := createMonitor(t, route, cookies[0], `{"type":"website","name":"Fixed","target":"`+target+`","interval":"5m"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	pub := monitorPubIDFor(t, pool, target)
	patch := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "/v1/monitors/"+pub, strings.NewReader(body))
		r.AddCookie(&cookies[0])
		w := httptest.NewRecorder()
		route.ServeHTTP(w, r)
		return w
	}
	for _, body := range []string{
		`{"target":"https://elsewhere.example.com"}`,
		`{"keyword":"checkout"}`,
		`{"target":"https://elsewhere.example.com","keyword":"x"}`,
	} {
		w := patch(body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PATCH %s = %d (%s), want 400", body, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "target_immutable") {
			t.Fatalf("PATCH %s refusal = %s, want target_immutable", body, w.Body.String())
		}
	}
	// name, paused, interval keep working on the same row.
	if w := patch(`{"name":"Renamed"}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH name = %d (%s), want 200", w.Code, w.Body.String())
	}
}

// patchMonitor drives PATCH /v1/monitors/{id} with a seat's cookie.
func patchMonitor(t *testing.T, h http.Handler, cookie http.Cookie, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPatch, "/v1/monitors/"+id, strings.NewReader(body))
	r.AddCookie(&cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// monitorPubIDFor reads the monitor row's public id in the API's own shape
// (lowercase hex, no dashes).
func monitorPubIDFor(t *testing.T, pool *pg.Pool, target string) string {
	t.Helper()
	var pub string
	if err := pool.Raw().QueryRow(context.Background(),
		`SELECT replace(public_id::text, '-', '') FROM monitor WHERE target = $1 LIMIT 1`, target).Scan(&pub); err != nil {
		t.Fatalf("monitor pub id for %s: %v", target, err)
	}
	return pub
}

// The heartbeat's private target: kind heartbeat, key from the monitor's
// public id, never leased by the fleet (LeaseDueTargets filters the kind).
func TestHeartbeatCreateMintsAPrivateTarget(t *testing.T) {
	pool, route, cookies := newSharedWorld(t, 1)
	ctx := context.Background()
	w, resp := createMonitor(t, route, cookies[0], `{"type":"heartbeat","name":"Nightly","interval":"5m"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("heartbeat create = %d (%s)", w.Code, w.Body.String())
	}
	if resp["pingUrl"] == nil || resp["pingUrl"].(string) == "" {
		t.Fatalf("heartbeat create answered no pingUrl: %v", resp)
	}
	var kinds int
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM probe_target pt JOIN monitor m ON m.target_id = pt.id
		  WHERE pt.kind = 'heartbeat'`).Scan(&kinds); err != nil || kinds != 1 {
		t.Fatalf("private heartbeat targets = %d (err %v), want 1", kinds, err)
	}
	// The miss window opened at 2x the interval, in the future.
	var due time.Time
	if err := pool.Raw().QueryRow(ctx,
		`SELECT ts.next_due_at FROM target_schedule ts JOIN probe_target pt ON pt.id = ts.target_id
		  WHERE pt.kind = 'heartbeat'`).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if !due.After(time.Now().Add(5 * time.Minute)) {
		t.Fatalf("heartbeat first window = %v, want ~2x interval out", due)
	}
	// The target's url is the tokenless stable label ("heartbeat:<public
	// id>"), never the ping URL: nothing fetches it, and the URL's token is a
	// secret with no reader here.
	if n := oneInt(t, pool,
		`SELECT count(*) FROM probe_target pt JOIN monitor m ON m.target_id = pt.id
		  WHERE pt.kind = 'heartbeat' AND pt.url = 'heartbeat:' || m.public_id::text`); n != 1 {
		t.Fatal("the heartbeat target does not carry the stable heartbeat:<public id> label")
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM probe_target WHERE url LIKE '%/public/ping/%'`); n != 0 {
		t.Fatal("a heartbeat target carries a ping URL with its token")
	}
	_ = availability.StatusDown
}

// blocked_host binds only the anonymous mint doors: a signed-in owner
// creating a check on a domain whose eTLD+1 sits in blocked_host must
// succeed - the table guards the landing, never /v1/monitors.
func TestOwnerCreatesACheckOnABlockedHost(t *testing.T) {
	pool, route, cookies := newSharedWorld(t, 1)
	ctx := context.Background()
	// The eTLD+1 of the target the seat will watch, the exact spelling the
	// removal job writes into the table.
	family := fmt.Sprintf("blocked-%d.example.com", time.Now().UnixNano()%100000)
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO blocked_host (domain, reason) VALUES ($1, 'self-serve TXT removal')`, family); err != nil {
		t.Fatal(err)
	}
	w, _ := createMonitor(t, route, cookies[0],
		`{"type":"website","name":"Mine","target":"https://deep.`+family+`/checkout","interval":"5m"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create on a blocked host = %d (%s), want 201", w.Code, w.Body.String())
	}
}
