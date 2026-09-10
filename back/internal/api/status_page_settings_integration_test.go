//go:build integration

// The owner's status-page settings after part 4: indexOptIn persists into
// the column the gate reads, the verification token is issued on read and
// stays stable until verified, verificationRecord is composed server-side,
// the answer carries rootPageUrl - and a removed page refuses release
// (releaseProject) and the project delete (409 page_removed). Needs
// Postgres: -tags=integration, UC_TEST_POSTGRES.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/storage/pg"
)

// newSettingsWorld: one seat, its session cookie, and the world's pool.
func newSettingsWorld(t *testing.T) (*pg.Pool, http.Handler, http.Cookie) {
	t.Helper()
	pool, route, cookies := newSharedWorld(t, 1)
	return pool, muxOf(route), cookies[0]
}

// muxOf is a no-op kept for the signature (route is used by callers that
// watch; the settings tests drive their own mux).
func muxOf(h http.Handler) http.Handler { return h }

func putStatus(t *testing.T, h http.Handler, cookie http.Cookie, body string) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/v1/status-page", strings.NewReader(body))
	r.AddCookie(&cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// indexOptIn persists through PUT - into the COLUMN the gate reads, not
// just the config blob - and returns on both reads; the verification token
// is issued on read while unverified, stays STABLE across reads, the answer
// carries rootPageUrl, and verificationRecord is composed server-side from
// the PROJECT's domain with the same eTLD+1 the worker resolves.
func TestStatusPageSettingsCarryTheIndexFacts(t *testing.T) {
	pool, _, cookie := newSettingsWorld(t)
	ctx := context.Background()
	sm := session.New(pool, session.DefaultTTL, nil)
	wa := NewWriteAPI(pool, nil, sm, false, nil, nil, false, "")
	mux := http.NewServeMux()
	mux.Handle("PUT /v1/status-page", wa)
	mux.Handle("GET /v1/status-page", wa)

	if code := putStatus(t, mux, cookie, `{"title":"Mine","indexOptIn":true}`); code != http.StatusOK {
		t.Fatalf("PUT with indexOptIn = %d, want 200", code)
	}
	var projectID int64
	_ = pool.Raw().QueryRow(ctx, `SELECT id FROM project LIMIT 1`).Scan(&projectID)
	// The switch is real where the gate reads it: the COLUMN, never only the
	// config blob (indexCandidates reads status_page.index_opt_in).
	if n := oneInt(t, pool, `SELECT count(*) FROM status_page WHERE project_id = $1 AND index_opt_in`, projectID); n != 1 {
		t.Fatal("PUT {indexOptIn:true} did not write the status_page.index_opt_in column the gate reads")
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/status-page", nil)
	r.AddCookie(&cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d (%s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["indexOptIn"] != true {
		t.Fatalf("indexOptIn = %v, want true", resp["indexOptIn"])
	}
	token, _ := resp["verificationToken"].(string)
	if len(token) != 32 {
		t.Fatalf("verificationToken = %q, want 32 hex chars", token)
	}
	if resp["rootPageUrl"] == nil {
		t.Fatal("the answer carries no rootPageUrl")
	}
	// verificationRecord is composed server-side from the PROJECT's domain
	// (the host the worker's dns-tokens job reduces), with the www label
	// folded away by the same eTLD+1 helper.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE project SET domain = 'www.example.com' WHERE id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		rr := httptest.NewRequest(http.MethodGet, "/v1/status-page", nil)
		rr.AddCookie(&cookie)
		ww := httptest.NewRecorder()
		mux.ServeHTTP(ww, rr)
		var body map[string]any
		if err := json.Unmarshal(ww.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if got := read()["verificationRecord"]; got != "_upcontrol-verify.example.com" {
		t.Fatalf("verificationRecord = %v, want _upcontrol-verify.example.com", got)
	}
	// A project domain the eTLD+1 helper cannot reduce omits the field:
	// there is no record to publish.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE project SET domain = 'localhost' WHERE id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	if got := read()["verificationRecord"]; got != nil {
		t.Fatalf("verificationRecord on an unregistrable domain = %v, want omitted", got)
	}
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE project SET domain = 'www.example.com' WHERE id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	// The token is idempotent until verified: the same token on re-read.
	resp2 := read()
	if resp2["verificationToken"] != token {
		t.Fatalf("verificationToken changed on re-read: %v, want the same %q", resp2["verificationToken"], token)
	}
	// Verified clears the token, stops the record and reports the stamp.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET host_verified_at = now(), verification_token = NULL WHERE project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	resp3 := read()
	if resp3["verificationToken"] != nil {
		t.Fatalf("a verified page still offers a token: %v", resp3["verificationToken"])
	}
	if resp3["verificationRecord"] != nil {
		t.Fatalf("a verified page still offers the record: %v", resp3["verificationRecord"])
	}
	if resp3["hostVerifiedAt"] == nil {
		t.Fatal("a verified page reports no hostVerifiedAt")
	}
	// Turning the switch OFF writes the column too: the gate must see the
	// owner's NO, not a stale blob. And an already indexed page leaves at
	// once: the gate stops reading a page that is no longer opted in, so its
	// stamp would otherwise keep the robots meta and the sitemap saying yes.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET indexed_at = now() WHERE project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	if code := putStatus(t, mux, cookie, `{"title":"Mine","indexOptIn":false}`); code != http.StatusOK {
		t.Fatalf("PUT indexOptIn off = %d, want 200", code)
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM status_page WHERE project_id = $1 AND NOT index_opt_in`, projectID); n != 1 {
		t.Fatal("PUT {indexOptIn:false} did not clear the column the gate reads")
	}
	// The page was first saved while the project had no domain, so it is
	// stored as prj-N; the project has gained one since. That address is
	// the page's for good: a later save updates it and never mints a
	// second page under the domain's slug.
	if n := oneInt(t, pool, `SELECT count(*) FROM status_page WHERE project_id = $1`, projectID); n != 1 {
		t.Fatalf("the project holds %d status pages after a re-save, want 1", n)
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM status_page WHERE project_id = $1 AND indexed_at IS NOT NULL`, projectID); n != 0 {
		t.Fatal("PUT {indexOptIn:false} left the page's index stamp: it stays in the sitemap and says index, follow")
	}
}

// A removed page stays removed (plan part 2): releaseProject REFUSES it
// (errPageRemoved) and leaves the page exactly as it was - root reference
// kept, removed_at kept. The live-page half of release (root kept, page
// alive) is TestReleasedHostPageKeepsMeasuringAndSurvivesTheReaper below.
func TestReleaseRefusesARemovedPage(t *testing.T) {
	pool, route, _ := newSettingsWorld(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano() % 100000
	host := fmt.Sprintf("release-%d.example.com", uniq)

	// A watch-minted host page on the owner's project.
	w := watch(t, route, host, "")
	if w.Code != http.StatusOK {
		t.Fatalf("watch = %d (%s)", w.Code, w.Body.String())
	}
	slug := watchBody(t, w)["slug"].(string)
	var projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT id FROM project WHERE domain = $1`, host).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	var root int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT root_target_id FROM status_page WHERE slug = $1`, slug).Scan(&root); err != nil {
		t.Fatal(err)
	}

	// Removal first: a removed page must not come back as ownerless-and-live.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET removed_at = now() WHERE slug = $1`, slug); err != nil {
		t.Fatal(err)
	}
	// Release the project the way the API does: refused, nothing moved.
	tx, err := pool.Raw().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseProject(ctx, tx, projectID); !errors.Is(err, errPageRemoved) {
		tx.Rollback(ctx)
		t.Fatalf("releaseProject on a removed page = %v, want errPageRemoved", err)
	}
	_ = tx.Rollback(ctx)
	var rootAfter *int64
	var removedAfter *time.Time
	if err := pool.Raw().QueryRow(ctx,
		`SELECT root_target_id, removed_at FROM status_page WHERE slug = $1`, slug).Scan(&rootAfter, &removedAfter); err != nil {
		t.Fatal(err)
	}
	if rootAfter == nil || *rootAfter != root {
		t.Fatalf("the refused release dropped the page's root_target_id: %v, want %d", rootAfter, root)
	}
	if removedAfter == nil {
		t.Fatal("the refused release un-removed the page")
	}
}

// DELETE /v1/project maps the refusal to 409 page_removed and the project
// survives; a clean page releases exactly as today (200, the project now
// ownerless, the page alive).
func TestDeleteProjectRefusesARemovedPageAndReleasesACleanOne(t *testing.T) {
	pool, _, cookies := newSharedWorld(t, 2)
	ctx := context.Background()
	sm := session.New(pool, session.DefaultTTL, nil)
	wa := NewWriteAPI(pool, nil, sm, false, nil, nil, false, "")
	mux := http.NewServeMux()
	mux.Handle("DELETE /v1/project", wa)
	mux.Handle("PUT /v1/status-page", wa)

	put := func(cookie http.Cookie, title string) {
		t.Helper()
		if code := putStatus(t, mux, cookie, `{"title":"`+title+`"}`); code != http.StatusOK {
			t.Fatalf("PUT %s = %d, want 200", title, code)
		}
	}
	deleteProject := func(cookie http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodDelete, "/v1/project", nil)
		r.AddCookie(&cookie)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	// Seat 0: a removed page refuses the delete, and the project survives.
	put(cookies[0], "Removed")
	var project0 int64
	_ = pool.Raw().QueryRow(ctx, `SELECT id FROM project ORDER BY id LIMIT 1`).Scan(&project0)
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET removed_at = now() WHERE project_id = $1`, project0); err != nil {
		t.Fatal(err)
	}
	w := deleteProject(cookies[0])
	if w.Code != http.StatusConflict {
		t.Fatalf("DELETE with a removed page = %d (%s), want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "page_removed") {
		t.Fatalf("refusal body = %s, want page_removed", w.Body.String())
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM project WHERE id = $1`, project0); n != 1 {
		t.Fatal("the refused delete removed the project anyway")
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM status_page WHERE project_id = $1 AND removed_at IS NOT NULL`, project0); n != 1 {
		t.Fatal("the refused delete lost the page's removed_at")
	}

	// Seat 1: a clean page releases as today - 200, account closed (the
	// last project), the project itself now ownerless, its page alive.
	put(cookies[1], "Clean")
	var project1 int64
	_ = pool.Raw().QueryRow(ctx, `SELECT id FROM project WHERE id <> $1 ORDER BY id LIMIT 1`, project0).Scan(&project1)
	w2 := deleteProject(cookies[1])
	if w2.Code != http.StatusOK {
		t.Fatalf("DELETE on a clean page = %d (%s), want 200", w2.Code, w2.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w2.Body.Bytes(), &body)
	if body["accountDeleted"] != true {
		t.Fatalf("accountDeleted = %v, want true (the seat's only project)", body["accountDeleted"])
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM project WHERE id = $1`, project1); n != 1 {
		t.Fatal("the clean release deleted the project itself")
	}
	if n := oneInt(t, pool, `SELECT count(*) FROM status_page WHERE project_id = $1 AND removed_at IS NULL`, project1); n != 1 {
		t.Fatal("the clean release did not leave a live ownerless page")
	}
}

// A released LIVE host page keeps measuring: the root target stays due
// through the page's reference (no unpaused subscriber left), and once it
// has answered once the reaper spares the ownerless tenant forever.
func TestReleasedHostPageKeepsMeasuringAndSurvivesTheReaper(t *testing.T) {
	pool, route, _ := newSharedWorld(t, 1)
	ctx := context.Background()
	uniq := time.Now().UnixNano() % 100000
	host := fmt.Sprintf("live-release-%d.example.com", uniq)

	w := watch(t, route, host, "")
	if w.Code != http.StatusOK {
		t.Fatalf("watch = %d (%s)", w.Code, w.Body.String())
	}
	slug := watchBody(t, w)["slug"].(string)
	var tenantID, projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT tenant_id, id FROM project WHERE domain = $1`, host).Scan(&tenantID, &projectID); err != nil {
		t.Fatal(err)
	}
	var root int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT root_target_id FROM status_page WHERE slug = $1`, slug).Scan(&root); err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Raw().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseProject(ctx, tx, projectID); err != nil {
		t.Fatalf("releaseProject: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Every subscription is paused by the release; the target stays due only
	// through the page's root reference. Give it its first answer, age the
	// ownerless tenant past the reaper's window, and it must be spared.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE probe_target SET first_ok_at = now() WHERE id = $1`, root); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE tenant SET created_at = now() - interval '8 days' WHERE id = $1`, tenantID); err != nil {
		t.Fatal(err)
	}
	if err := reapForTest(ctx, pool); err != nil {
		t.Fatalf("reap: %v", err)
	}
	var pages int
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM status_page WHERE slug = $1`, slug).Scan(&pages); err != nil || pages != 1 {
		t.Fatalf("the released host page survived the reaper (rows = %d, err %v), want 1", pages, err)
	}
	var rootStill *int64
	_ = pool.Raw().QueryRow(ctx,
		`SELECT root_target_id FROM status_page WHERE slug = $1`, slug).Scan(&rootStill)
	if rootStill == nil || *rootStill != root {
		t.Fatalf("the spared page lost its root reference: %v", rootStill)
	}
}

// reapForTest runs the reaper's statement set from the api package's test
// (the worker's own lane covers it in depth).
func reapForTest(ctx context.Context, pool *pg.Pool) error {
	if _, err := pool.Raw().Exec(ctx, `UPDATE monitor SET paused = true WHERE NOT paused AND tenant_id IN (SELECT id FROM tenant WHERE claim_token_hash IS NOT NULL AND created_at < now() - interval '24 hours')`); err != nil {
		return err
	}
	_, err := pool.Raw().Exec(ctx,
		`DELETE FROM tenant t
		  WHERE t.claim_token_hash IS NOT NULL
		    AND t.created_at < now() - interval '7 days'
		    AND NOT EXISTS (SELECT 1 FROM project p
		                    JOIN project_seq ps ON ps.project_id = p.id
		                    JOIN api_key ak    ON ak.project_id = p.id
		                   WHERE p.tenant_id = t.id AND ps.next > 1)
		    AND NOT EXISTS (SELECT 1 FROM status_page sp
		                    JOIN probe_target pt ON pt.id = sp.root_target_id
		                   WHERE sp.tenant_id = t.id AND sp.is_host_page
		                     AND sp.removed_at IS NULL AND sp.root_target_id IS NOT NULL
		                     AND pt.first_ok_at IS NOT NULL)`)
	return err
}
