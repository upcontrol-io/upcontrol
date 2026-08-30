//go:build integration

// The watch door reads the session (docs/plans/projects-axis.md Decision 7)
// and the public status page says whose it is (T8): a signed-in visitor with
// room gets the project in their own tenant — with the session's pick set to
// it (Decision 18) — at the limit or signed out they still get the anonymous
// demo mint, and publicStatus answers mine only for the owner.
// public_watch_test.go is the unit lane; these need the real routes, a real
// session cookie and real Postgres. -tags=integration with UC_TEST_POSTGRES.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// watchFixture: a signed-in Free account (tenant, person, login member, no
// projects) and the two public routes mounted exactly as cmd/ucapi mounts
// them.
type watchFixture struct {
	pool        *pg.Pool
	tenantID    int64
	personID    int64
	sess        *session.Manager
	route       http.Handler
	ownerCookie *http.Cookie
}

func newWatchFixture(t *testing.T) *watchFixture {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping public watch integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "../../../db/postgres"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	uniq := time.Now().UnixNano()
	f := &watchFixture{pool: pool}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("watcher-%d", uniq)).Scan(&f.tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Owner') RETURNING id`,
		fmt.Sprintf("watcher-%d@example.com", uniq)).Scan(&f.personID); err != nil {
		t.Fatalf("person: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, 'login', 'active')`,
		f.tenantID, f.personID); err != nil {
		t.Fatalf("tenant_member: %v", err)
	}
	f.sess = session.New(pool, session.DefaultTTL, nil)
	token, err := f.sess.Create(ctx, f.personID, f.tenantID)
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	f.ownerCookie = &http.Cookie{Name: session.CookieName, Value: token}
	wa := NewWriteAPI(pool, nil, f.sess, false, nil, nil, false)
	mux := http.NewServeMux()
	mux.Handle("POST /public/watch", wa)
	mux.Handle("GET /public/status/{slug}", wa)
	f.route = mux
	return f
}

// watch posts a watch body for a fresh host; a unique X-Forwarded-For keeps
// the per-replica IP throttle out of the picture.
func (f *watchFixture) watch(t *testing.T, host string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/public/watch",
		strings.NewReader(`{"host":"`+host+`"}`))
	r.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", time.Now().UnixNano()%254+1))
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	f.route.ServeHTTP(w, r)
	return w
}

func (f *watchFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.Raw().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (f *watchFixture) unclaimedCount(t *testing.T) int64 {
	return f.count(t, `SELECT count(*) FROM tenant WHERE claim_token_hash IS NOT NULL`)
}

// Signed in with room and a host nobody holds: the project is created in the
// CALLER's tenant — the full provisioning triple, monitors and status page on
// the caller's ids from the first check — and no unclaimed tenant appears.
func TestWatchSignedInWithRoomCreatesTheProjectInTheCallersTenant(t *testing.T) {
	f := newWatchFixture(t)
	host := fmt.Sprintf("room-%d.example.com", time.Now().UnixNano())
	before := f.unclaimedCount(t)

	w := f.watch(t, host, f.ownerCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("watch signed-in with room = %d (%s), want 200", w.Code, w.Body.String())
	}
	if after := f.unclaimedCount(t); after != before {
		t.Fatalf("unclaimed tenants %d -> %d, want unchanged (no demo mint for a caller with room)", before, after)
	}

	ctx := context.Background()
	// The project — exactly one on the caller, named after the host, with the
	// whole provisioning triple newUnclaimedTenant mints.
	var projectID int64
	if n := f.count(t, `SELECT count(*) FROM project WHERE tenant_id = $1`, f.tenantID); n != 1 {
		t.Fatalf("caller projects = %d, want exactly 1", n)
	}
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT id FROM project WHERE tenant_id = $1`, f.tenantID).Scan(&projectID); err != nil {
		t.Fatalf("caller project: %v", err)
	}
	var domain string
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT domain FROM project WHERE id = $1`, projectID).Scan(&domain); err != nil || domain != host {
		t.Fatalf("project domain = %q (err %v), want the watched %q", domain, err, host)
	}
	if n := f.count(t, `SELECT count(*) FROM project_seq WHERE project_id = $1`, projectID); n != 1 {
		t.Fatalf("project_seq rows = %d, want 1 (the provisioning triple)", n)
	}
	if n := f.count(t, `SELECT count(*) FROM api_key WHERE tenant_id = $1 AND project_id = $2`, f.tenantID, projectID); n != 1 {
		t.Fatalf("api_key rows = %d, want 1 (the provisioning triple)", n)
	}
	// Monitors and the status page land on the caller's ids, not a demo page.
	if n := f.count(t, `SELECT count(*) FROM monitor WHERE tenant_id = $1 AND project_id = $2`, f.tenantID, projectID); n != 1 {
		t.Fatalf("monitors on the caller = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM status_page WHERE tenant_id = $1 AND project_id = $2`, f.tenantID, projectID); n != 1 {
		t.Fatalf("status_page rows on the caller = %d, want 1", n)
	}
	// Decision 18: the session's pick now points at the new project, so
	// /v1/me opens on what the caller just created (createProject parity).
	var pick int64
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT project_id FROM session WHERE person_id = $1`, f.personID).Scan(&pick); err != nil || pick != projectID {
		t.Fatalf("session project_id = %d (err %v), want the new project %d", pick, err, projectID)
	}
}

// Signed in at the Free limit: the wall is on the claim button, never on the
// check — the demo mint answers exactly as before, an unclaimed tenant with
// the host, and the caller gains nothing.
func TestWatchSignedInAtTheLimitStillMintsTheDemoTenant(t *testing.T) {
	f := newWatchFixture(t)
	ctx := context.Background()
	var used int64
	if err := f.pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'used.example.com') RETURNING id`,
		f.tenantID).Scan(&used); err != nil {
		t.Fatalf("claimer project: %v", err)
	}
	host := fmt.Sprintf("demo-%d.example.com", time.Now().UnixNano())
	before := f.unclaimedCount(t)

	w := f.watch(t, host, f.ownerCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("watch signed-in at limit = %d (%s), want 200", w.Code, w.Body.String())
	}
	if after := f.unclaimedCount(t); after != before+1 {
		t.Fatalf("unclaimed tenants %d -> %d, want before+1 (the demo mint)", before, after)
	}
	// The demo page belongs to a NEW unclaimed tenant, not the caller.
	var demoTenant int64
	var unclaimedFlag bool
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT p.tenant_id, (t.claim_token_hash IS NOT NULL)
		   FROM project p JOIN tenant t ON t.id = p.tenant_id
		  WHERE p.domain = $1`, host).Scan(&demoTenant, &unclaimedFlag); err != nil || !unclaimedFlag {
		t.Fatalf("demo project holder read err %v, unclaimed flag %v, want a read with the flag set", err, unclaimedFlag)
	}
	if demoTenant == f.tenantID {
		t.Fatal("the at-limit watch must mint a demo tenant, not touch the caller's")
	}
	if n := f.count(t, `SELECT count(*) FROM monitor WHERE tenant_id = $1`, demoTenant); n != 1 {
		t.Fatalf("monitors on the demo tenant = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE tenant_id = $1`, f.tenantID); n != 1 {
		t.Fatalf("caller projects = %d, want still exactly the used one", n)
	}
}

// Signed out with a brand-new host: no cookie, so the door never consults
// the caller — the anonymous demo mint answers end-to-end (200, an
// unclaimed tenant appears) exactly as before this axis existed.
func TestWatchSignedOutMintsTheDemoTenant(t *testing.T) {
	f := newWatchFixture(t)
	host := fmt.Sprintf("anon-%d.example.com", time.Now().UnixNano())
	before := f.unclaimedCount(t)

	w := f.watch(t, host, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("watch signed-out = %d (%s), want 200", w.Code, w.Body.String())
	}
	if after := f.unclaimedCount(t); after != before+1 {
		t.Fatalf("unclaimed tenants %d -> %d, want before+1 (the demo mint)", before, after)
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE tenant_id = $1`, f.tenantID); n != 0 {
		t.Fatalf("caller projects = %d, want 0 (no session, no own-tenant mint)", n)
	}
}

// publicStatus says mine only to the owner: the page owner's session reads
// mine: true; another account's session, and no session at all, read no mine
// field whatsoever (absent = not the viewer's page).
func TestPublicStatusAnswersMineOnlyForTheOwner(t *testing.T) {
	f := newWatchFixture(t)
	ctx := context.Background()
	var projectID int64
	if err := f.pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'mine.example.com') RETURNING id`,
		f.tenantID).Scan(&projectID); err != nil {
		t.Fatalf("owner project: %v", err)
	}
	slug := fmt.Sprintf("mine-%d", time.Now().UnixNano())
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title) VALUES ($1, $2, $3, 'Mine')`,
		f.tenantID, projectID, slug); err != nil {
		t.Fatalf("status_page: %v", err)
	}

	get := func(cookie *http.Cookie) map[string]any {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/public/status/"+slug, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		f.route.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("public status = %d (%s), want 200", w.Code, w.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		return resp
	}

	if resp := get(f.ownerCookie); resp["mine"] != true {
		t.Fatalf("owner's mine = %v, want true (body %v)", resp["mine"], resp)
	}

	// hasCustomDomain rides the same visibility rule as mine, and answers a different
	// question: the owner's banner on OUR link sells the custom domain, and must stop
	// selling one they already have. No domain stored yet, so the field is absent.
	if resp := get(f.ownerCookie); resp["hasCustomDomain"] != nil {
		t.Fatalf("owner with no domain reads hasCustomDomain = %v, want the field absent",
			resp["hasCustomDomain"])
	}
	if _, err := f.pool.Raw().Exec(ctx,
		`UPDATE status_page SET domain = $2 WHERE slug = $1`,
		slug, fmt.Sprintf("status.mine-%d.example.com", time.Now().UnixNano())); err != nil {
		t.Fatalf("set domain: %v", err)
	}
	// Stored but NOT verified, which is the state a customer sits in while DNS
	// propagates: they have bought the address, so the offer is already wrong.
	if resp := get(f.ownerCookie); resp["hasCustomDomain"] != true {
		t.Fatalf("owner with a domain reads hasCustomDomain = %v, want true",
			resp["hasCustomDomain"])
	}

	// A different account's session: same public answer, no mine field.
	uniq := time.Now().UnixNano()
	var otherTenant, otherPerson int64
	if err := f.pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("other-%d", uniq)).Scan(&otherTenant); err != nil {
		t.Fatalf("other tenant: %v", err)
	}
	if err := f.pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Other') RETURNING id`,
		fmt.Sprintf("other-%d@example.com", uniq)).Scan(&otherPerson); err != nil {
		t.Fatalf("other person: %v", err)
	}
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, 'login', 'active')`,
		otherTenant, otherPerson); err != nil {
		t.Fatalf("other tenant_member: %v", err)
	}
	otherToken, err := f.sess.Create(ctx, otherPerson, otherTenant)
	if err != nil {
		t.Fatalf("mint other session: %v", err)
	}
	otherCookie := &http.Cookie{Name: session.CookieName, Value: otherToken}
	resp := get(otherCookie)
	if _, ok := resp["mine"]; ok {
		t.Fatalf("another account reads mine = %v, want the field absent", resp["mine"])
	}
	// The domain set above is the owner's business. A stranger learning this page has one
	// would also silence the pitch that stranger is meant to see.
	if _, ok := resp["hasCustomDomain"]; ok {
		t.Fatalf("another account reads hasCustomDomain = %v, want the field absent",
			resp["hasCustomDomain"])
	}
	resp = get(nil)
	if _, ok := resp["mine"]; ok {
		t.Fatalf("a signed-out visitor reads mine = %v, want the field absent", resp["mine"])
	}
	if _, ok := resp["hasCustomDomain"]; ok {
		t.Fatalf("a signed-out visitor reads hasCustomDomain = %v, want the field absent",
			resp["hasCustomDomain"])
	}
}
