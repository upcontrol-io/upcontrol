//go:build integration

// The owner's status-page settings after part 4: indexOptIn persists, the
// verification token is issued on read and stays stable until verified, the
// answer carries rootPageUrl - and releaseProject never clears removed_at
// and the page keeps its root_target_id. Needs Postgres: -tags=integration,
// UC_TEST_POSTGRES.
package api

import (
	"context"
	"encoding/json"
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

func putStatus(t *testing.T, h http.Handler, cookie http.Cookie, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/v1/status-page", strings.NewReader(body))
	r.AddCookie(&cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var resp map[string]any
	if w.Code == http.StatusOK {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}
	return w.Code, resp
}

// indexOptIn persists through PUT and returns on both reads; the
// verification token is issued on read while unverified, stays STABLE across
// reads, and the answer carries rootPageUrl.
func TestStatusPageSettingsCarryTheIndexFacts(t *testing.T) {
	pool, _, cookie := newSettingsWorld(t)
	ctx := context.Background()
	sm := session.New(pool, session.DefaultTTL, nil)
	wa := NewWriteAPI(pool, nil, sm, false, nil, nil, false)
	mux := http.NewServeMux()
	mux.Handle("PUT /v1/status-page", wa)
	mux.Handle("GET /v1/status-page", wa)

	if code, _ := putStatus(t, mux, cookie, `{"title":"Mine","indexOptIn":true}`); code != http.StatusOK {
		t.Fatalf("PUT with indexOptIn = %d, want 200", code)
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
	// The token is idempotent until verified: the same token on re-read.
	r2 := httptest.NewRequest(http.MethodGet, "/v1/status-page", nil)
	r2.AddCookie(&cookie)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)
	var resp2 map[string]any
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if resp2["verificationToken"] != token {
		t.Fatalf("verificationToken changed on re-read: %v, want the same %q", resp2["verificationToken"], token)
	}
	// Verified clears the token and reports the stamp.
	var projectID int64
	_ = pool.Raw().QueryRow(ctx, `SELECT id FROM project LIMIT 1`).Scan(&projectID)
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET host_verified_at = now(), verification_token = NULL WHERE project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	r3 := httptest.NewRequest(http.MethodGet, "/v1/status-page", nil)
	r3.AddCookie(&cookie)
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, r3)
	var resp3 map[string]any
	_ = json.Unmarshal(w3.Body.Bytes(), &resp3)
	if resp3["verificationToken"] != nil {
		t.Fatalf("a verified page still offers a token: %v", resp3["verificationToken"])
	}
	if resp3["hostVerifiedAt"] == nil {
		t.Fatal("a verified page reports no hostVerifiedAt")
	}
}

// releaseProject (DELETE /v1/project on the last project) keeps the page's
// root_target_id and never clears removed_at: the page outlives the project
// and a removed page stays removed.
func TestReleaseKeepsTheRootReferenceAndNeverUnremoves(t *testing.T) {
	pool, route, cookie := newSettingsWorld(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano() % 100000
	host := fmt.Sprintf("release-%d.example.com", uniq)

	// A watch-minted host page on the owner's project.
	w := watch(t, route, host, "")
	if w.Code != http.StatusOK {
		t.Fatalf("watch = %d (%s)", w.Code, w.Body.String())
	}
	_ = cookie
	slug := watchBody(t, w)["slug"].(string)
	// The project is the caller's (the watch fixture's single seat is signed
	// out, so this was the anonymous mint: adopt it by claiming).
	var tenantID, projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT tenant_id, id FROM project WHERE domain = $1`, host).Scan(&tenantID, &projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE tenant SET claim_token_hash = NULL WHERE id = $1`, tenantID); err != nil {
		t.Fatal(err)
	}
	var root int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT root_target_id FROM status_page WHERE slug = $1`, slug).Scan(&root); err != nil {
		t.Fatal(err)
	}

	// Removal first: a removed page must survive release still removed.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET removed_at = now() WHERE slug = $1`, slug); err != nil {
		t.Fatal(err)
	}
	// Release the project the way the API does.
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
	var rootAfter *int64
	var removedAfter *time.Time
	if err := pool.Raw().QueryRow(ctx,
		`SELECT root_target_id, removed_at FROM status_page WHERE slug = $1`, slug).Scan(&rootAfter, &removedAfter); err != nil {
		t.Fatal(err)
	}
	if rootAfter == nil || *rootAfter != root {
		t.Fatalf("release dropped the page's root_target_id: %v, want %d", rootAfter, root)
	}
	if removedAfter == nil {
		t.Fatal("release cleared removed_at: a removed page came back to life")
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
