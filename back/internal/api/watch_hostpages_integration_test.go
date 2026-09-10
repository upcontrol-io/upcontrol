//go:build integration

// The watch vertical after migration 009 (plan parts 1-2): shared probes,
// host pages, canonical hosts, the alias fold, second pages, removal, the
// mint ceilings. Needs Postgres: -tags=integration with UC_TEST_POSTGRES.
// Each test builds its own database, so a failed run leaves nothing behind
// for the next one.
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
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// newHostWorld mints a private database (migrated) with a pool and the
// anonymous + status routes mounted exactly as cmd/ucapi mounts them.
func newHostWorld(t *testing.T) (*pg.Pool, http.Handler, *session.Manager) {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping host-page integration test")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("uc_g2_host_%d", time.Now().UnixNano())
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
	_ = db.Close()
	pool, err := pg.Open(context.Background(), mine.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sm := session.New(pool, session.DefaultTTL, nil)
	wa := NewWriteAPI(pool, nil, sm, false, nil, nil, false)
	mux := http.NewServeMux()
	mux.Handle("POST /public/watch", wa)
	mux.Handle("GET /public/status/{slug}", wa)
	mux.Handle("GET /public/status", wa)
	mux.Handle("DELETE /v1/monitors/{id}", NewMonitors(pool, pgstore.New(pool.Raw()), sm, ""))
	mux.Handle("GET /v1/monitors", NewMonitors(pool, pgstore.New(pool.Raw()), sm, ""))
	return pool, mux, sm
}

// watch posts a watch body; a unique X-Forwarded-For keeps the per-replica
// IP throttle and the per-IP mint ceiling out of the picture unless asked.
func watch(t *testing.T, h http.Handler, host string, ip string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/public/watch",
		strings.NewReader(`{"host":"`+host+`"}`))
	if ip == "" {
		ip = fmt.Sprintf("203.0.113.%d", time.Now().UnixNano()%254+1)
	}
	r.Header.Set("X-Forwarded-For", ip)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func watchBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode watch body %q: %v", w.Body.String(), err)
	}
	return body
}

func oneInt(t *testing.T, pool *pg.Pool, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.Raw().QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("one %q: %v", q, err)
	}
	return n
}

// www.example.com watches as example.com: one project domain, one target,
// the bare slug - and the www-shaped slug 301s to it.
func TestWatchCanonicalizesTheHostAndTheAliasSlugRedirects(t *testing.T) {
	pool, route, _ := newHostWorld(t)
	uniq := time.Now().UnixNano() % 100000
	host := fmt.Sprintf("example-%d.com", uniq)

	w := watch(t, route, "www."+host, "")
	if w.Code != http.StatusOK {
		t.Fatalf("watch www.%s = %d (%s), want 200", host, w.Code, w.Body.String())
	}
	body := watchBody(t, w)
	if body["slug"] != fmt.Sprintf("example-%d-com", uniq) {
		t.Fatalf("slug = %v, want example-%d-com (the canonical host's)", body["slug"], uniq)
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM project WHERE domain = $1`, host); n != 1 {
		t.Fatalf("projects on the canonical domain = %d, want 1", n)
	}
	if n := oneInt(t, pool,
		`SELECT count(*) FROM probe_target WHERE url = $1`, "https://"+host); n != 1 {
		t.Fatalf("root targets for https://%s = %d, want 1", host, n)
	}
	// The www-shaped slug folds to a 301 at the canonical page.
	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/public/status/www-example-%d-com", uniq), nil)
	rec := httptest.NewRecorder()
	route.ServeHTTP(rec, r)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("alias slug = %d (%s), want 301", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != fmt.Sprintf("/status/example-%d-com", uniq) {
		t.Fatalf("alias redirect = %q, want the canonical slug", loc)
	}
	// A second watch of the bare host reuses the same page (nothing new).
	before := oneInt(t, pool, `SELECT count(*) FROM status_page`)
	w2 := watch(t, route, host, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("second watch = %d, want 200", w2.Code)
	}
	if body2 := watchBody(t, w2); body2["slug"] != body["slug"] {
		t.Fatalf("second watch slug = %v, want the same %v", body2["slug"], body["slug"])
	}
	if after := oneInt(t, pool, `SELECT count(*) FROM status_page`); after != before {
		t.Fatalf("status_page rows %d -> %d on reuse, want unchanged", before, after)
	}
}

// Deleting the root monitor leaves the page alive: the host page's root
// component keeps drawing from the page's root_target_id.
func TestHostPageSurvivesRootMonitorDelete(t *testing.T) {
	pool, route, sm := newHostWorld(t)
	uniq := time.Now().UnixNano() % 100000
	host := fmt.Sprintf("survivor-%d.example.com", uniq)
	if _, err := pool.Raw().Exec(context.Background(),
		`INSERT INTO person (public_id, email) VALUES (gen_random_uuid(), $1)`,
		fmt.Sprintf("survivor-%d@example.com", uniq)); err != nil {
		t.Fatal(err)
	}
	w := watch(t, route, host, "")
	if w.Code != http.StatusOK {
		t.Fatalf("watch = %d (%s)", w.Code, w.Body.String())
	}
	slug := watchBody(t, w)["slug"].(string)
	var tenantID int64
	if err := pool.Raw().QueryRow(context.Background(),
		`SELECT tenant_id FROM project WHERE domain = $1`, host).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	// The claim turns the holder into an account the session can act as.
	if _, err := pool.Raw().Exec(context.Background(),
		`UPDATE tenant SET claim_token_hash = NULL, owner_person_id = (SELECT id FROM person WHERE email = $2) WHERE id = $1`,
		tenantID, fmt.Sprintf("survivor-%d@example.com", uniq)); err != nil {
		t.Fatal(err)
	}
	var personID int64
	_ = pool.Raw().QueryRow(context.Background(),
		`SELECT id FROM person WHERE email = $1`, fmt.Sprintf("survivor-%d@example.com", uniq)).Scan(&personID)
	token, err := sm.Create(context.Background(), personID, tenantID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// List, then delete every monitor through the API.
	list := httptest.NewRequest(http.MethodGet, "/v1/monitors", nil)
	list.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	lw := httptest.NewRecorder()
	route.ServeHTTP(lw, list)
	var mons []map[string]any
	if err := json.Unmarshal(lw.Body.Bytes(), &mons); err != nil || len(mons) != 1 {
		t.Fatalf("monitor list = %s", lw.Body.String())
	}
	del := httptest.NewRequest(http.MethodDelete, "/v1/monitors/"+mons[0]["id"].(string), nil)
	del.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	dw := httptest.NewRecorder()
	route.ServeHTTP(dw, del)
	if dw.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", dw.Code)
	}
	list2 := httptest.NewRequest(http.MethodGet, "/v1/monitors", nil)
	list2.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	lw2 := httptest.NewRecorder()
	route.ServeHTTP(lw2, list2)
	if strings.TrimSpace(lw2.Body.String()) != "[]" {
		t.Fatalf("monitors after delete = %s, want []", lw2.Body.String())
	}
	// The page still renders, with the root component first and its bars.
	page := httptest.NewRequest(http.MethodGet, "/public/status/"+slug, nil)
	pw := httptest.NewRecorder()
	route.ServeHTTP(pw, page)
	if pw.Code != http.StatusOK {
		t.Fatalf("page after delete = %d, want 200", pw.Code)
	}
	var pub map[string]any
	if err := json.Unmarshal(pw.Body.Bytes(), &pub); err != nil {
		t.Fatal(err)
	}
	comps, _ := pub["components"].([]any)
	if len(comps) != 1 {
		t.Fatalf("components after delete = %d, want the root one", len(comps))
	}
	comp := comps[0].(map[string]any)
	if comp["name"] != host {
		t.Fatalf("root component name = %v, want the host %q", comp["name"], host)
	}
	if comp["bars"] == nil {
		t.Fatal("root component has no bars")
	}
	if pub["hostPage"] != true {
		t.Fatalf("hostPage = %v, want true", pub["hostPage"])
	}
}

// A host with a CLAIMED page mints the visitor their own SUFFIXED page on
// the same root target; an unclaimed one is reused as-is.
func TestSecondPageForAHostWithAClaimedPage(t *testing.T) {
	pool, route, _ := newHostWorld(t)
	uniq := time.Now().UnixNano() % 100000
	host := fmt.Sprintf("claimed-%d.example.com", uniq)

	first := watch(t, route, host, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first watch = %d (%s)", first.Code, first.Body.String())
	}
	firstSlug := watchBody(t, first)["slug"].(string)
	var firstTenant int64
	_ = pool.Raw().QueryRow(context.Background(),
		`SELECT tenant_id FROM project WHERE domain = $1`, host).Scan(&firstTenant)
	// Claim the first page.
	if _, err := pool.Raw().Exec(context.Background(),
		`UPDATE tenant SET claim_token_hash = NULL WHERE id = $1`, firstTenant); err != nil {
		t.Fatal(err)
	}
	targetsBefore := oneInt(t, pool, `SELECT count(*) FROM probe_target`)

	second := watch(t, route, host, "")
	if second.Code != http.StatusOK {
		t.Fatalf("second watch = %d (%s)", second.Code, second.Body.String())
	}
	body := watchBody(t, second)
	secondSlug := body["slug"].(string)
	if secondSlug == firstSlug {
		t.Fatalf("the claimed page's slug was handed to a second visitor")
	}
	// A shared root: one probe, two pages.
	if n := oneInt(t, pool, `SELECT count(*) FROM probe_target`); n != targetsBefore {
		t.Fatalf("probe_target rows %d -> %d, want no new probe (shared root)", targetsBefore, n)
	}
	var roots int
	if err := pool.Raw().QueryRow(context.Background(),
		`SELECT count(DISTINCT root_target_id) FROM status_page WHERE root_target_id IS NOT NULL`).Scan(&roots); err != nil || roots != 1 {
		t.Fatalf("distinct root targets = %d (err %v), want 1", roots, err)
	}
	// The second page is NOT the host page.
	if isHost := oneInt(t, pool,
		`SELECT count(*) FROM status_page WHERE slug = $1 AND is_host_page`, secondSlug); isHost != 0 {
		t.Fatal("the suffixed second page is marked is_host_page")
	}
	if isHost := oneInt(t, pool,
		`SELECT count(*) FROM status_page WHERE slug = $1 AND is_host_page`, firstSlug); isHost != 1 {
		t.Fatal("the first page is not marked is_host_page")
	}
}

// A removed host is never minted again (blocked_host at the door) and its
// page answers 410 on the public read.
func TestRemovedHostIsRefusedAndItsPageIsGone(t *testing.T) {
	pool, route, _ := newHostWorld(t)
	uniq := time.Now().UnixNano() % 100000
	host := fmt.Sprintf("removed-%d.example.com", uniq)

	first := watch(t, route, host, "")
	if first.Code != http.StatusOK {
		t.Fatalf("watch = %d (%s)", first.Code, first.Body.String())
	}
	slug := watchBody(t, first)["slug"].(string)
	// The removal job's transaction, by hand: it is the worker's test too.
	var root *int64
	if err := pool.Raw().QueryRow(context.Background(),
		`UPDATE status_page SET removed_at = now(), root_target_id = NULL
		  WHERE slug = $1 RETURNING root_target_id`, slug).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Raw().Exec(context.Background(),
		`INSERT INTO blocked_host (domain, reason) VALUES ($1, 'self-serve TXT removal')`,
		"example.com"); err != nil { // the removal job writes the eTLD+1, not the host
		t.Fatal(err)
	}

	again := watch(t, route, host, "")
	if again.Code != http.StatusForbidden {
		t.Fatalf("watch on a removed host = %d (%s), want 403", again.Code, again.Body.String())
	}
	if !strings.Contains(again.Body.String(), "blocked_host") {
		t.Fatalf("refusal body = %s, want blocked_host", again.Body.String())
	}
	page := httptest.NewRequest(http.MethodGet, "/public/status/"+slug, nil)
	pw := httptest.NewRecorder()
	route.ServeHTTP(pw, page)
	if pw.Code != http.StatusGone {
		t.Fatalf("removed page = %d, want 410", pw.Code)
	}
	if !strings.Contains(pw.Body.String(), "page_removed") {
		t.Fatalf("removed page body = %s, want page_removed", pw.Body.String())
	}
}

// The mint ceilings: the N+1th page from one IP in a day answers 429 and
// creates nothing; the host-pages cap answers 429 the same way.
func TestMintCeilingsRefuseAndCreateNothing(t *testing.T) {
	pool, _, _ := newHostWorld(t)
	const ip = "198.51.100.77"
	wa := NewWriteAPI(pool, nil, session.New(pool, session.DefaultTTL, nil), false, nil, nil, false)
	wa.statusKnobs.MintPerIPPerDay = 2
	mux := http.NewServeMux()
	mux.Handle("POST /public/watch", wa)
	uniq := time.Now().UnixNano() % 100000
	for i := 0; i < 2; i++ {
		w := watch(t, mux, fmt.Sprintf("ceiling-%d-%d.example.com", uniq, i), ip)
		if w.Code != http.StatusOK {
			t.Fatalf("mint %d = %d (%s), want 200", i, w.Code, w.Body.String())
		}
		// Lift the per-replica cooldown between mints: this test prices the
		// per-IP daily ceiling, not the 3-second one.
		wa.checkMu.Lock()
		wa.checkSeenAt = map[string]time.Time{}
		wa.checkMu.Unlock()
	}
	before := oneInt(t, pool, `SELECT count(*) FROM status_page`)
	third := watch(t, mux, fmt.Sprintf("ceiling-%d-3.example.com", uniq), ip)
	if third.Code != http.StatusTooManyRequests {
		t.Fatalf("third mint from one IP = %d (%s), want 429", third.Code, third.Body.String())
	}
	if !strings.Contains(third.Body.String(), "mint_ip_ceiling") {
		t.Fatalf("refusal = %s, want mint_ip_ceiling", third.Body.String())
	}
	if after := oneInt(t, pool, `SELECT count(*) FROM status_page`); after != before {
		t.Fatalf("a refused mint created %d rows", after-before)
	}

	// The host-pages cap: count live unclaimed host pages up to the cap, and
	// the next fresh mint refuses.
	wa.statusKnobs.MintPerIPPerDay = 50
	wa.statusKnobs.HostPagesMax = int(oneInt(t, pool,
		`SELECT count(*) FROM status_page sp JOIN tenant t ON t.id = sp.tenant_id
		  WHERE sp.is_host_page AND sp.removed_at IS NULL AND t.claim_token_hash IS NOT NULL`))
	// Lift the per-replica cooldown again: the refusal under test is the cap.
	wa.checkMu.Lock()
	wa.checkSeenAt = map[string]time.Time{}
	wa.checkMu.Unlock()
	capped := watch(t, mux, fmt.Sprintf("capped-%d.example.com", uniq), ip)
	if capped.Code != http.StatusTooManyRequests {
		t.Fatalf("mint past the host cap = %d (%s), want 429", capped.Code, capped.Body.String())
	}
	if !strings.Contains(capped.Body.String(), "host_pages_ceiling") {
		t.Fatalf("refusal = %s, want host_pages_ceiling", capped.Body.String())
	}
}

// An IP literal is unmintable: no registrable domain names a page.
func TestWatchOnAnIPLiteralIsUnmintable(t *testing.T) {
	_, route, _ := newHostWorld(t)
	w := watch(t, route, "192.168.1.1", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("watch on an IP literal = %d (%s), want 400", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unmintable_host") {
		t.Fatalf("refusal = %s, want unmintable_host", w.Body.String())
	}
}

// UC_INDEX_DISABLED=1 empties the index without touching the stamps: pages
// keep indexed_at, and the public page's indexable flag reads false (the
// sitemap and the robots meta read the same flag).
func TestIndexDisabledHidesTheStampButKeepsIt(t *testing.T) {
	pool, route, _ := newHostWorld(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano() % 100000
	host := fmt.Sprintf("indexed-%d.example.com", uniq)

	w := watch(t, route, host, "")
	if w.Code != http.StatusOK {
		t.Fatalf("watch = %d (%s)", w.Code, w.Body.String())
	}
	slug := watchBody(t, w)["slug"].(string)
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET indexed_at = now() WHERE slug = $1`, slug); err != nil {
		t.Fatal(err)
	}

	// The normal door: a stamped page reads indexable.
	page := func() map[string]any {
		r := httptest.NewRequest(http.MethodGet, "/public/status/"+slug, nil)
		pw := httptest.NewRecorder()
		route.ServeHTTP(pw, r)
		if pw.Code != http.StatusOK {
			t.Fatalf("public status = %d (%s)", pw.Code, pw.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(pw.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if got := page()["indexable"]; got != true {
		t.Fatalf("indexable on a stamped page = %v, want true", got)
	}

	// The kill switch: the stamp stays, the flag flips.
	wa := NewWriteAPI(pool, nil, session.New(pool, session.DefaultTTL, nil), false, nil, nil, false)
	wa.statusKnobs.IndexDisabled = true
	r := httptest.NewRequest(http.MethodGet, "/public/status/"+slug, nil)
	pw := httptest.NewRecorder()
	wa.public(pw, r)
	if pw.Code != http.StatusOK {
		t.Fatalf("public status under the kill switch = %d", pw.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(pw.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["indexable"] != false {
		t.Fatalf("indexable under UC_INDEX_DISABLED = %v, want false", body["indexable"])
	}
	var stamped int
	_ = pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM status_page WHERE slug = $1 AND indexed_at IS NOT NULL`, slug).Scan(&stamped)
	if stamped != 1 {
		t.Fatal("the kill switch cleared the stamps: re-enabling would not restore the index")
	}
}

// The host page's state sentence: the plan part 3 forms, worded from the
// probe's point of view - ok names the answer, down names the outage's start
// and why, could_not_measure refuses to guess, nodata says the first check
// has not run.
func TestHostPageStateSentences(t *testing.T) {
	pool, route, _ := newHostWorld(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano() % 100000
	host := fmt.Sprintf("state-%d.example.com", uniq)

	w := watch(t, route, host, "")
	if w.Code != http.StatusOK {
		t.Fatalf("watch = %d (%s)", w.Code, w.Body.String())
	}
	slug := watchBody(t, w)["slug"].(string)
	var targetID int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT root_target_id FROM status_page WHERE slug = $1`, slug).Scan(&targetID); err != nil {
		t.Fatal(err)
	}

	get := func() map[string]any {
		r := httptest.NewRequest(http.MethodGet, "/public/status/"+slug, nil)
		pw := httptest.NewRecorder()
		route.ServeHTTP(pw, r)
		if pw.Code != http.StatusOK {
			t.Fatalf("public status = %d (%s)", pw.Code, pw.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(pw.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	seedCheck := func(ok bool, errClass string, code, ageMinutes int) {
		if _, err := pool.Raw().Exec(ctx,
			`INSERT INTO checks (target_id, interval_sec, ts, region, ok, status_code, error_class, total_ms)
			 VALUES ($1, 300, now() - make_interval(mins => $2::int), 'default', $3, $4, $5, 230)`,
			targetID, ageMinutes, ok, code, errClass); err != nil {
			t.Fatal(err)
		}
	}

	// No facts yet: nodata's fixed sentence.
	state, _ := get()["state"].(map[string]any)
	if state == nil || state["kind"] != "nodata" {
		t.Fatalf("state before any facts = %v, want kind nodata", state)
	}
	if state["sentence"] != "No data yet: the first check runs at the next probe cycle, usually within a few minutes." {
		t.Fatalf("nodata sentence = %q", state["sentence"])
	}

	// ok: "As of HH:MM UTC, host answered HTTP 200 in 230 ms from our check."
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO target_facts (target_id, status) VALUES ($1, 'ok')`, targetID); err != nil {
		t.Fatal(err)
	}
	seedCheck(true, "", 200, 2)
	state, _ = get()["state"].(map[string]any)
	if state["kind"] != "ok" {
		t.Fatalf("ok state kind = %v", state)
	}
	okSentence, _ := state["sentence"].(string)
	if !strings.HasPrefix(okSentence, "As of ") || !strings.HasSuffix(okSentence,
		fmt.Sprintf(", %s answered HTTP 200 in 230 ms from our check.", host)) || !strings.Contains(okSentence, " UTC") {
		t.Fatalf("ok sentence = %q, want the plan's answered form", okSentence)
	}

	// down: the outage began at the last ok, and the parenthetical words the
	// timeout in the plan's terms.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE target_facts SET status = 'down', consecutive_failures = 3 WHERE target_id = $1`, targetID); err != nil {
		t.Fatal(err)
	}
	seedCheck(false, "timeout", 0, 1)
	state, _ = get()["state"].(map[string]any)
	if state["kind"] != "down" {
		t.Fatalf("down state kind = %v", state)
	}
	downSentence, _ := state["sentence"].(string)
	if !strings.HasPrefix(downSentence, fmt.Sprintf("Our check has had no answer from %s since ", host)) ||
		!strings.HasSuffix(downSentence, " (connection timed out).") || !strings.Contains(downSentence, " UTC") {
		t.Fatalf("down sentence = %q, want the plan's no-answer form with the timeout phrasing", downSentence)
	}

	// could_not_measure: the refusal sentence, no guessing.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE target_facts SET status = 'could_not_measure' WHERE target_id = $1`, targetID); err != nil {
		t.Fatal(err)
	}
	state, _ = get()["state"].(map[string]any)
	if state["kind"] != "could_not_measure" {
		t.Fatalf("could_not_measure state kind = %v", state)
	}
	if state["sentence"] != fmt.Sprintf("%s refuses automated checks from our location; we cannot measure it.", host) {
		t.Fatalf("could_not_measure sentence = %q", state["sentence"])
	}
}
