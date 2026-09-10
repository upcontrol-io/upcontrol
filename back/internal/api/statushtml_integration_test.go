//go:build integration

// The crawler surfaces (plan part 4) against a real database: the HTML
// door's parity with the JSON door (the state sentence, the title, the
// robots meta), the 404/410/301 answers, the directory and sitemap
// predicates, the OG image, and the two static pages. -tags=integration
// with UC_TEST_POSTGRES.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/storage/pg"
)

// newSurfacesWorld: a private migrated database, the write API, and every
// Group 3 route mounted exactly as cmd/ucapi mounts them. The returned
// write API is the one the handlers hold, so a test may flip a knob and
// re-issue a request through the same mux.
func newSurfacesWorld(t *testing.T) (*pg.Pool, http.Handler, *writeAPI) {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping surfaces integration test")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("uc_g3_surfaces_%d", time.Now().UnixNano())
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
	wa := NewWriteAPI(pool, nil, sm, false, nil, nil, false, "")
	sh := NewStatusPages(wa)
	mux := http.NewServeMux()
	mux.Handle("GET /status/{slug}", sh)
	mux.Handle("GET /status", sh)
	mux.Handle("GET /sitemap-status.xml", sh)
	mux.Handle("GET /public/status/{slug}/og.png", sh)
	mux.Handle("GET /public/status/{slug}", wa)
	mux.Handle("POST /internal/seed-host", NewSeedDoor(wa, "test-node-token"))
	mux.Handle("POST /public/status/{slug}/remove-token", NewRemoveTokenDoor(wa))
	return pool, mux, wa
}

// get runs one GET through the world's mux.
func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// seedHostPage mints a host page through the seed door and returns its slug.
func seedHostPage(t *testing.T, h http.Handler, host string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/internal/seed-host", strings.NewReader(`{"host":"`+host+`"}`))
	r.Header.Set("Authorization", "Bearer test-node-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("seed %s: %d %s", host, w.Code, w.Body.String())
	}
	var body struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Slug
}

// rootTargetOf reads the page's root target id.
func rootTargetOf(t *testing.T, pool *pg.Pool, slug string) int64 {
	t.Helper()
	var root int64
	if err := pool.Raw().QueryRow(context.Background(),
		`SELECT root_target_id FROM status_page WHERE slug = $1`, slug).Scan(&root); err != nil {
		t.Fatalf("root target of %s: %v", slug, err)
	}
	return root
}

// seedOkChecks gives the target an ok facts row and a few ok checks, so the
// page has a state sentence and uptime to render.
func seedOkChecks(t *testing.T, pool *pg.Pool, targetID int64, ms int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO target_facts (target_id, status, last_check_at) VALUES ($1, 'ok', now())
		 ON CONFLICT (target_id) DO UPDATE SET status = 'ok', last_check_at = now()`, targetID); err != nil {
		t.Fatalf("target_facts: %v", err)
	}
	for _, mins := range []int{2, 7, 12, 17, 22} {
		if _, err := pool.Raw().Exec(ctx,
			`INSERT INTO checks (ts, region, ok, status_code, error_class, total_ms, target_id, interval_sec)
			 VALUES (now() - make_interval(mins => $1), 'test', true, 200, '', $2, $3, 300)`, mins, ms, targetID); err != nil {
			t.Fatalf("checks: %v", err)
		}
	}
}

// robotsLine pulls the robots meta out of a page body.
func robotsLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, `name="robots"`) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// TestHTMLDoorParityPinsTheStateSentence: the sentence in the HTML door's
// output equals state.sentence from the JSON door, the title follows the
// pinned format, and the copy carries no em-dash.
func TestHTMLDoorParityPinsTheStateSentence(t *testing.T) {
	pool, mux, _ := newSurfacesWorld(t)
	host := fmt.Sprintf("parity-%d.example.com", time.Now().UnixNano())
	slug := seedHostPage(t, mux, host)
	seedOkChecks(t, pool, rootTargetOf(t, pool, slug), 230)

	w := get(t, mux, "/public/status/"+slug)
	if w.Code != http.StatusOK {
		t.Fatalf("json door: %d", w.Code)
	}
	var js struct {
		State struct {
			Sentence string `json:"sentence"`
		} `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &js); err != nil {
		t.Fatal(err)
	}
	if js.State.Sentence == "" {
		t.Fatal("the JSON door returned no state sentence")
	}

	w2 := get(t, mux, "/status/"+slug)
	if w2.Code != http.StatusOK {
		t.Fatalf("html door: %d %s", w2.Code, w2.Body.String())
	}
	html := w2.Body.String()
	if !strings.Contains(html, js.State.Sentence) {
		t.Fatalf("the HTML door does not carry the JSON door's sentence %q", js.State.Sentence)
	}
	wantTitle := "<title>" + host + " status: is " + host + " answering right now?</title>"
	if !strings.Contains(html, wantTitle) {
		t.Fatalf("title not in the pinned format; want %q", wantTitle)
	}
	if strings.Contains(html, "—") {
		t.Fatal("the HTML door's copy contains an em-dash")
	}
	// The measured-answer section repeats the sentence under its own heading.
	if !strings.Contains(html, "<h3>Is "+host+" answering right now?</h3>") {
		t.Fatal("the measured-answer heading is missing")
	}
	// The footer's fixed sentences and doors.
	for _, want := range []string{
		"Measured from one location outside " + host + " by UpControl. Not affiliated with " + host + ". Created automatically.",
		`href="/status/` + slug + `#claim"`,
		`href="/status/policy"`,
		`href="/bot"`,
		`href="/status"`,
		`href="/?check=` + host,
		"Powered by UpControl",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("the page is missing %q", want)
		}
	}
	if !strings.Contains(html, `<meta property="og:image" content="https://upcontrol.io/public/status/`+slug+`/og.png">`) {
		t.Fatal("og:image is missing or wrong")
	}
	if !strings.Contains(html, `<meta name="twitter:card" content="summary_large_image">`) {
		t.Fatal("twitter:card is missing")
	}
	for k, want := range map[string]string{
		"Content-Type":  "text/html; charset=utf-8",
		"Vary":          "User-Agent",
		"Cache-Control": "public, max-age=60",
	} {
		if got := w2.Header().Get(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
}

// TestHTMLDoorRobotsPerClass: the robots meta mirrors the JSON door's
// indexable for every page class, and the canonical link belongs to host
// pages only.
func TestHTMLDoorRobotsPerClass(t *testing.T) {
	pool, mux, wa := newSurfacesWorld(t)
	host := fmt.Sprintf("robots-%d.example.com", time.Now().UnixNano())
	slug := seedHostPage(t, mux, host)
	ctx := context.Background()

	// An unstamped host page: qualified but quiet. Its canonical points at
	// ITSELF (never at another page), so the pair cannot propagate anything.
	w := get(t, mux, "/status/"+slug)
	if w.Code != http.StatusOK || !strings.Contains(robotsLine(w.Body.String()), "noindex, follow") {
		t.Fatalf("unstamped host page robots = %q", robotsLine(w.Body.String()))
	}

	// Stamped: index, follow, and the canonical appears.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET indexed_at = now() WHERE slug = $1`, slug); err != nil {
		t.Fatal(err)
	}
	w = get(t, mux, "/status/"+slug)
	if w.Code != http.StatusOK || !strings.Contains(robotsLine(w.Body.String()), "index, follow") {
		t.Fatalf("stamped host page robots = %q", robotsLine(w.Body.String()))
	}
	if !strings.Contains(w.Body.String(), `<link rel="canonical" href="https://upcontrol.io/status/`+slug+`">`) {
		t.Fatal("the host page's canonical link is missing")
	}

	// The kill switch outranks the stamp.
	wa.statusKnobs.IndexDisabled = true
	w = get(t, mux, "/status/"+slug)
	if !strings.Contains(robotsLine(w.Body.String()), "noindex, follow") {
		t.Fatalf("kill-switched host page robots = %q", robotsLine(w.Body.String()))
	}
	wa.statusKnobs.IndexDisabled = false

	// A suffixed (non-host) page: out entirely, never a canonical.
	sib := slug + "-999"
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET slug = $2, is_host_page = false WHERE slug = $1`, slug, sib); err != nil {
		t.Fatal(err)
	}
	w = get(t, mux, "/status/"+sib)
	if w.Code != http.StatusOK || !strings.Contains(robotsLine(w.Body.String()), "noindex, nofollow") {
		t.Fatalf("suffixed page robots = %q", robotsLine(w.Body.String()))
	}
	if strings.Contains(w.Body.String(), `rel="canonical"`) {
		t.Fatal("a suffixed page carries a canonical link")
	}

	// The prj-N fallback of a page-less project: out entirely.
	var tenantID, projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), 'pageless') RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'pageless.example.com') RETURNING id`,
		tenantID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	w = get(t, mux, "/status/prj-"+fmt.Sprint(projectID))
	if w.Code != http.StatusOK || !strings.Contains(robotsLine(w.Body.String()), "noindex, nofollow") {
		t.Fatalf("prj-N page robots = %q (code %d)", robotsLine(w.Body.String()), w.Code)
	}
}

// TestHTMLDoorAnswers404410301: unknown slug, removed page, and the alias
// fold, each in the door's own dress.
func TestHTMLDoorAnswers404410301(t *testing.T) {
	pool, mux, _ := newSurfacesWorld(t)
	host := fmt.Sprintf("answers-%d.example.com", time.Now().UnixNano())
	slug := seedHostPage(t, mux, host)

	if w := get(t, mux, "/status/no-such-page-here"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown slug = %d, want 404", w.Code)
	}
	if !strings.Contains(get(t, mux, "/status/no-such-page-here").Body.String(), "No such page") {
		t.Fatal("the 404 page has no body copy")
	}

	// The www-shaped slug of the host folds to the canonical one.
	w := get(t, mux, "/status/www-"+slug)
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/status/"+slug {
		t.Fatalf("alias = %d %q, want 301 %q", w.Code, w.Header().Get("Location"), "/status/"+slug)
	}

	if _, err := pool.Raw().Exec(context.Background(),
		`UPDATE status_page SET removed_at = now() WHERE slug = $1`, slug); err != nil {
		t.Fatal(err)
	}
	w = get(t, mux, "/status/"+slug)
	if w.Code != http.StatusGone {
		t.Fatalf("removed page = %d, want 410", w.Code)
	}
	if !strings.Contains(w.Body.String(), "This page was removed at the site owner's request.") {
		t.Fatal("the 410 body copy is wrong")
	}
}

// TestDirectoryAndSitemapListIndexedPagesOnly: the directory and the
// sitemap share one predicate (indexed_at set, live), and the kill switch
// empties both.
func TestDirectoryAndSitemapListIndexedPagesOnly(t *testing.T) {
	pool, mux, wa := newSurfacesWorld(t)
	host := fmt.Sprintf("listed-%d.example.com", time.Now().UnixNano())
	slug := seedHostPage(t, mux, host)
	unlisted := seedHostPage(t, mux, fmt.Sprintf("unlisted-%d.example.com", time.Now().UnixNano()))
	ctx := context.Background()
	seedOkChecks(t, pool, rootTargetOf(t, pool, slug), 180)
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET indexed_at = now() WHERE slug = $1`, slug); err != nil {
		t.Fatal(err)
	}

	w := get(t, mux, "/status")
	if w.Code != http.StatusOK {
		t.Fatalf("directory = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `href="/status/`+slug+`"`) {
		t.Fatal("the indexed page is not listed")
	}
	if strings.Contains(w.Body.String(), unlisted) {
		t.Fatal("the unindexed page is listed")
	}

	sm := get(t, mux, "/sitemap-status.xml")
	if sm.Code != http.StatusOK || sm.Header().Get("Content-Type") != "application/xml; charset=utf-8" {
		t.Fatalf("sitemap = %d %q", sm.Code, sm.Header().Get("Content-Type"))
	}
	if !strings.Contains(sm.Body.String(), "<loc>https://upcontrol.io/status/"+slug+"</loc>") {
		t.Fatal("the sitemap misses the indexed page")
	}
	if strings.Contains(sm.Body.String(), unlisted) {
		t.Fatal("the sitemap lists an unindexed page")
	}
	if !strings.Contains(sm.Body.String(), "<lastmod>"+time.Now().UTC().Format("2006-01-02")+"</lastmod>") {
		t.Fatal("the sitemap's lastmod is not the day of the newest check")
	}

	wa.statusKnobs.IndexDisabled = true
	if body := get(t, mux, "/status").Body.String(); !strings.Contains(body, "The directory is temporarily empty.") {
		t.Fatal("the kill-switched directory is not empty")
	}
	if body := get(t, mux, "/sitemap-status.xml").Body.String(); strings.Contains(body, "<url>") {
		t.Fatal("the kill-switched sitemap is not empty")
	}
}

// TestOGImageDecodesAt1200x630: the OG door answers a decodable 1200x630
// PNG both for a measured page and for a fresh one with no data.
func TestOGImageDecodesAt1200x630(t *testing.T) {
	pool, mux, _ := newSurfacesWorld(t)
	host := fmt.Sprintf("og-%d.example.com", time.Now().UnixNano())
	fresh := seedHostPage(t, mux, host)
	measured := seedHostPage(t, mux, fmt.Sprintf("ogm-%d.example.com", time.Now().UnixNano()))
	seedOkChecks(t, pool, rootTargetOf(t, pool, measured), 210)

	for _, slug := range []string{fresh, measured} {
		w := get(t, mux, "/public/status/"+slug+"/og.png")
		if w.Code != http.StatusOK {
			t.Fatalf("og.png for %s = %d %s", slug, w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "image/png" {
			t.Fatalf("Content-Type = %q", ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=60" {
			t.Fatalf("Cache-Control = %q", cc)
		}
		cfg, _, err := image.DecodeConfig(w.Body)
		if err != nil {
			t.Fatalf("decode og.png for %s: %v", slug, err)
		}
		if cfg.Width != 1200 || cfg.Height != 630 {
			t.Fatalf("og.png for %s is %dx%d, want 1200x630", slug, cfg.Width, cfg.Height)
		}
	}

	if w := get(t, mux, "/public/status/no-such-page/og.png"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown og.png = %d, want 404", w.Code)
	}
	if _, err := pool.Raw().Exec(context.Background(),
		`UPDATE status_page SET removed_at = now() WHERE slug = $1`, fresh); err != nil {
		t.Fatal(err)
	}
	if w := get(t, mux, "/public/status/"+fresh+"/og.png"); w.Code != http.StatusGone {
		t.Fatalf("removed og.png = %d, want 410", w.Code)
	}
}

// (The static pages carry no data; staticpages_test.go covers them in
// the unit lane.)
