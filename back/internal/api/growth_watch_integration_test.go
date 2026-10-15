//go:build integration

// The watch door's owner rules (2026-10-15), for every caller: a host's
// existing page is opened as it is, before the host gate, the cooldown and
// the day's ceilings; a fresh host gets an unclaimed page with one check,
// signed in or not, and only when it answers; our robot's calls are tagged and
// nothing else changes for them; a status page sends its reader to the landing
// with nothing typed in. Each test runs on its own database (newHostWorld),
// with a stub prober and a live recorder.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/analytics"
	"go.upcontrol.io/back/internal/probe/executor"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// stubProber answers every probe with one result: the tests watch made-up
// hosts that no network answers. calls counts the probes a door asked for.
type stubProber struct {
	res   executor.Result
	calls atomic.Int32
}

func (p *stubProber) Execute(context.Context, executor.CheckSpec) executor.Result {
	p.calls.Add(1)
	return p.res
}

// answering is a host that answers HTTP 200.
func answering() *stubProber {
	return &stubProber{res: executor.Result{
		OK: true, StatusCode: 200, DNSMs: 12, DNSAddrs: 1, ConnectMs: 20, TTFBMs: 90, TotalMs: 120,
	}}
}

type growthWatch struct {
	pool  *pg.Pool
	sm    *session.Manager
	wa    *writeAPI
	rec   *analytics.Recorder
	probe *stubProber
	route http.Handler
}

func newGrowthWatch(t *testing.T) *growthWatch {
	t.Helper()
	pool, _, sm := newHostWorld(t)
	g := &growthWatch{pool: pool, sm: sm, probe: answering()}
	g.rec = analytics.NewRecorder(analytics.PoolStore{Pool: pool}, pgstore.New(pool.Raw()), nil)
	g.rec.Start()
	t.Cleanup(func() { _ = g.rec.Stop(context.Background()) })
	g.wa = NewWriteAPI(pool, pgstore.New(pool.Raw()), sm, false, nil, g.rec, false, "")
	g.wa.exec = g.probe
	mux := http.NewServeMux()
	mux.Handle("POST /public/watch", g.wa)
	mux.Handle("POST /public/check", g.wa)
	g.route = mux
	return g
}

// post sends one request from ip; extra adds a cookie or a header.
func (g *growthWatch) post(t *testing.T, path, body, ip string, extra ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("X-Forwarded-For", ip)
	for _, f := range extra {
		f(r)
	}
	w := httptest.NewRecorder()
	g.route.ServeHTTP(w, r)
	return w
}

// footprint counts every kind of row a mint leaves behind.
func (g *growthWatch) footprint(t *testing.T) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, table := range []string{"tenant", "project", "status_page", "monitor", "probe_target", "api_key", "checks"} {
		out[table] = oneInt(t, g.pool, `SELECT count(*) FROM `+table)
	}
	return out
}

func sameFootprint(t *testing.T, before, after map[string]int64) {
	t.Helper()
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s rows %d -> %d, want nothing created", table, n, after[table])
		}
	}
}

// coolDown puts ip inside the watch cooldown, as if it had just been refused
// or admitted: the next fresh-host watch from it is a 429.
func (g *growthWatch) coolDown(ip string) {
	g.wa.checkMu.Lock()
	g.wa.checkSeenAt["watch:"+ip] = time.Now()
	g.wa.checkMu.Unlock()
}

func (g *growthWatch) liftCooldowns() {
	g.wa.checkMu.Lock()
	g.wa.checkSeenAt = map[string]time.Time{}
	g.wa.checkMu.Unlock()
}

// L3, L4, L10: a host's existing page, claimed or not, opens as it is from an
// address inside its cooldown and past its day's ceiling: nothing is probed,
// created or subscribed. The same address is still refused a fresh host.
func TestGrowthExistingPageOpensInsideTheCooldownAndPastTheCeiling(t *testing.T) {
	g := newGrowthWatch(t)
	ctx := context.Background()
	g.wa.statusKnobs.MintPerIPPerDay = 1
	uniq := time.Now().UnixNano() % 100000
	claimed := fmt.Sprintf("claimed-%d.example.com", uniq)
	open := fmt.Sprintf("open-%d.example.com", uniq)
	const ip = "198.51.100.10"

	first := g.post(t, "/public/watch", `{"host":"`+claimed+`"}`, ip)
	if first.Code != http.StatusOK {
		t.Fatalf("first watch = %d (%s), want 200", first.Code, first.Body.String())
	}
	if body := watchBody(t, first); body["created"] != true {
		t.Fatalf("a fresh host's watch answered created = %v, want true", body["created"])
	}
	slug := watchBody(t, first)["slug"].(string)
	if w := g.post(t, "/public/watch", `{"host":"`+open+`"}`, "198.51.100.11"); w.Code != http.StatusOK {
		t.Fatalf("second host's watch = %d (%s), want 200", w.Code, w.Body.String())
	}
	if _, err := g.pool.Raw().Exec(ctx,
		`UPDATE tenant SET claim_token_hash = NULL WHERE id = (SELECT tenant_id FROM project WHERE domain = $1)`, claimed); err != nil {
		t.Fatal(err)
	}

	before, probes := g.footprint(t), g.probe.calls.Load()
	// The minting address again: at its day's ceiling (one page) and, by
	// construction, inside its cooldown. Targets are ignored on the way.
	g.coolDown(ip)
	again := g.post(t, "/public/watch", `{"host":"`+claimed+`","targets":["https://`+claimed+`/pricing"]}`, ip)
	if again.Code != http.StatusOK {
		t.Fatalf("claimed host from a throttled address = %d (%s), want 200", again.Code, again.Body.String())
	}
	body := watchBody(t, again)
	if body["slug"] != slug || body["created"] != false || body["watching"] != float64(0) {
		t.Fatalf("claimed host answered %v, want its own slug %q, created false, watching 0", body, slug)
	}
	g.coolDown(ip)
	reopened := g.post(t, "/public/watch", `{"host":"www.`+open+`"}`, ip)
	if reopened.Code != http.StatusOK {
		t.Fatalf("unclaimed host from a throttled address = %d (%s), want 200", reopened.Code, reopened.Body.String())
	}
	if body := watchBody(t, reopened); body["created"] != false || body["watching"] != float64(1) {
		t.Fatalf("unclaimed host answered %v, want created false and the one check it holds", body)
	}
	sameFootprint(t, before, g.footprint(t))
	if n := g.probe.calls.Load(); n != probes {
		t.Fatalf("opening existing pages probed %d times, want none", n-probes)
	}

	// The throttles hold for what would mint: the cooldown, then the ceiling.
	fresh := fmt.Sprintf(`{"host":"fresh-%d.example.com"}`, uniq)
	g.coolDown(ip)
	if w := g.post(t, "/public/watch", fresh, ip); w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("fresh host inside the cooldown = %d (%s), want 429 rate_limited", w.Code, w.Body.String())
	}
	g.liftCooldowns()
	if w := g.post(t, "/public/watch", fresh, ip); w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "mint_ip_ceiling") {
		t.Fatalf("fresh host past the ceiling = %d (%s), want 429 mint_ip_ceiling", w.Code, w.Body.String())
	}
}

// L4 on a host the mint gate refuses: an owner's first website check names
// the project after a platform subdomain and gives it a page, and the door
// opens that page for anyone. A platform subdomain with no page is still
// refused.
func TestGrowthExistingPageOpensOnAHostTheMintGateRefuses(t *testing.T) {
	g := newGrowthWatch(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	host := fmt.Sprintf("myapp-%d.vercel.app", uniq%100000)
	slug := fmt.Sprintf("myapp-%d-vercel-app", uniq%100000)
	owner := seedPerson(t, g.pool, fmt.Sprintf("vercel-%d@example.com", uniq))
	tenant := seedOwnedTenant(t, g.pool, owner, fmt.Sprintf("vercel-%d", uniq))
	project := seedProject(t, g.pool, tenant, host)
	if _, err := g.pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title) VALUES ($1, $2, $3, $4)`,
		tenant, project, slug, host); err != nil {
		t.Fatal(err)
	}

	before, probes := g.footprint(t), g.probe.calls.Load()
	w := g.post(t, "/public/watch", `{"host":"`+host+`"}`, "198.51.100.60")
	if w.Code != http.StatusOK {
		t.Fatalf("watch of a claimed page on %s = %d (%s), want 200", host, w.Code, w.Body.String())
	}
	if body := watchBody(t, w); body["slug"] != slug || body["created"] != false || body["watching"] != float64(0) {
		t.Fatalf("watch answered %v, want the page %q, created false, watching 0", body, slug)
	}
	sameFootprint(t, before, g.footprint(t))
	if n := g.probe.calls.Load(); n != probes {
		t.Fatalf("opening the page probed %d times, want none", n-probes)
	}

	fresh := g.post(t, "/public/watch", fmt.Sprintf(`{"host":"other-%d.vercel.app"}`, uniq%100000), "198.51.100.61")
	if fresh.Code != http.StatusBadRequest || !strings.Contains(fresh.Body.String(), "unmintable_host") {
		t.Fatalf("a platform subdomain with no page = %d (%s), want 400 unmintable_host", fresh.Code, fresh.Body.String())
	}
}

// L14: the HTML door's "Check your own site" opens the landing with nothing
// typed in, as the app's footer does: the host it would carry already has
// its page.
func TestGrowthHTMLFooterLinksTheEmptyLanding(t *testing.T) {
	_, mux, _ := newSurfacesWorld(t)
	host := fmt.Sprintf("footer-%d.example.com", time.Now().UnixNano()%100000)
	w := get(t, mux, "/status/"+seedHostPage(t, mux, host))
	if w.Code != http.StatusOK {
		t.Fatalf("html door = %d (%s), want 200", w.Code, w.Body.String())
	}
	if html := w.Body.String(); !strings.Contains(html, `<a href="/">Check your own site</a>`) || strings.Contains(html, "?check=") {
		t.Fatal(`the footer's "Check your own site" is not a plain link to /`)
	}
}

// L8: a signed-in caller watching a fresh host gets the unclaimed page a
// stranger would. Nothing lands in their workspace and the session stays
// where it was: only a claim attaches a page.
func TestGrowthSignedInCallerGetsAnUnclaimedPage(t *testing.T) {
	g := newGrowthWatch(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	person := seedPerson(t, g.pool, fmt.Sprintf("signed-%d@example.com", uniq))
	tenant := seedOwnedTenant(t, g.pool, person, fmt.Sprintf("signed-%d", uniq))
	token, err := g.sm.Create(ctx, person, tenant, nil)
	if err != nil {
		t.Fatal(err)
	}
	host := fmt.Sprintf("mine-%d.example.com", uniq%100000)

	w := g.post(t, "/public/watch", `{"host":"`+host+`"}`, "198.51.100.20", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	})
	if w.Code != http.StatusOK || watchBody(t, w)["created"] != true {
		t.Fatalf("signed-in watch = %d (%s), want 200 created", w.Code, w.Body.String())
	}
	if n := oneInt(t, g.pool, `SELECT count(*) FROM project WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("the caller's workspace holds %d projects, want 0", n)
	}
	var unclaimed bool
	if err := g.pool.Raw().QueryRow(ctx,
		`SELECT t.claim_token_hash IS NOT NULL FROM project p JOIN tenant t ON t.id = p.tenant_id
		  WHERE p.domain = $1`, host).Scan(&unclaimed); err != nil || !unclaimed {
		t.Fatalf("the page's holder unclaimed = %v (err %v), want an unclaimed tenant", unclaimed, err)
	}
	var sessTenant int64
	var sessProject *int64
	if err := g.pool.Raw().QueryRow(ctx,
		`SELECT tenant_id, project_id FROM session WHERE person_id = $1`, person).Scan(&sessTenant, &sessProject); err != nil {
		t.Fatal(err)
	}
	if sessTenant != tenant || sessProject != nil {
		t.Fatalf("session scope = (%d, %v), want untouched (%d, none)", sessTenant, sessProject, tenant)
	}
}

// L9: without an e-mail the door reads no targets. A new page carries the
// host alone, and an existing page gains nothing.
func TestGrowthWatchWithoutAnEmailIgnoresTargets(t *testing.T) {
	g := newGrowthWatch(t)
	host := fmt.Sprintf("targets-%d.example.com", time.Now().UnixNano()%100000)
	body := fmt.Sprintf(`{"host":%q,"targets":["https://%s/pricing","https://%s/docs"]}`, host, host, host)
	checks := func() int64 {
		return oneInt(t, g.pool,
			`SELECT count(*) FROM monitor m JOIN project p ON p.id = m.project_id WHERE p.domain = $1`, host)
	}

	w := g.post(t, "/public/watch", body, "198.51.100.30")
	if w.Code != http.StatusOK {
		t.Fatalf("watch = %d (%s), want 200", w.Code, w.Body.String())
	}
	if got := watchBody(t, w); got["created"] != true || got["watching"] != float64(1) {
		t.Fatalf("watch answered %v, want created with one check", got)
	}
	if n := checks(); n != 1 {
		t.Fatalf("the new page carries %d checks, want 1 (the host)", n)
	}
	if n := oneInt(t, g.pool, `SELECT count(*) FROM probe_target WHERE url LIKE $1`, "https://"+host+"/%"); n != 0 {
		t.Fatalf("%d targets enrolled from the ignored list, want 0", n)
	}
	again := g.post(t, "/public/watch", body, "198.51.100.31")
	if again.Code != http.StatusOK || watchBody(t, again)["created"] != false {
		t.Fatalf("second watch = %d (%s), want 200 created false", again.Code, again.Body.String())
	}
	if n := checks(); n != 1 {
		t.Fatalf("the existing page carries %d checks after a second watch, want still 1", n)
	}
}

// L6: a fresh host that gives no HTTP answer is refused with the probe's
// diagnostic and nothing is created. Any answer is an answer: a host that
// answers 503 gets its page.
func TestGrowthUnreachableHostIsRefusedAndCreatesNothing(t *testing.T) {
	g := newGrowthWatch(t)
	ctx := context.Background()
	host := fmt.Sprintf("gone-%d.example.com", time.Now().UnixNano()%100000)
	g.probe.res = executor.Result{ErrorClass: "dns", TotalMs: 35}
	before := g.footprint(t)

	w := g.post(t, "/public/watch", `{"host":"`+host+`"}`, "198.51.100.40")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("watch of a host that does not answer = %d (%s), want 400", w.Code, w.Body.String())
	}
	var refused struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Check struct {
			Probe struct {
				OK         bool   `json:"ok"`
				ErrorClass string `json:"error_class"`
			} `json:"probe"`
			NetworkChecks []struct {
				Label string `json:"label"`
				Note  string `json:"note"`
			} `json:"networkChecks"`
			Groups []struct {
				Rows []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"rows"`
			} `json:"groups"`
		} `json:"check"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &refused); err != nil {
		t.Fatalf("decode refusal %s: %v", w.Body.String(), err)
	}
	if refused.Error.Code != "unreachable" || refused.Error.Message == "" {
		t.Fatalf("refusal error = %+v, want unreachable with a message", refused.Error)
	}
	if refused.Check.Probe.OK || refused.Check.Probe.ErrorClass != "dns" {
		t.Fatalf("check.probe = %+v, want the failed dns probe", refused.Check.Probe)
	}
	var response bool
	for _, row := range refused.Check.NetworkChecks {
		if row.Label == "redirects" {
			t.Fatal("the diagnostic carries a redirects row for a request that never completed")
		}
		if row.Label == "response" {
			response = row.Note == "dns"
		}
	}
	if !response {
		t.Fatalf("check.networkChecks = %+v, want a response row naming dns", refused.Check.NetworkChecks)
	}
	if len(refused.Check.Groups) == 0 || len(refused.Check.Groups[0].Rows) != 1 ||
		refused.Check.Groups[0].Rows[0].ID != "https://"+host || refused.Check.Groups[0].Rows[0].Status != "down" {
		t.Fatalf("check.groups = %+v, want the host's one row, down", refused.Check.Groups)
	}
	sameFootprint(t, before, g.footprint(t))

	g.probe.res = executor.Result{StatusCode: 503, ErrorClass: "status", TTFBMs: 70, TotalMs: 80}
	answered := g.post(t, "/public/watch", `{"host":"`+host+`"}`, "198.51.100.41")
	if answered.Code != http.StatusOK || watchBody(t, answered)["created"] != true {
		t.Fatalf("watch of a host answering 503 = %d (%s), want 200 created", answered.Code, answered.Body.String())
	}

	if err := g.rec.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if n := oneInt(t, g.pool,
		`SELECT count(*) FROM web_events WHERE name = 'watch_signup' AND props->>'host' = $1`, host); n != 1 {
		t.Fatalf("watch_signup rows for the host = %d, want 1 (the refusal records none)", n)
	}
}

// L16: our robot names itself with X-Upcontrol-Source. The events of the
// doors it calls carry props.source, and it meets the same ceiling as anyone.
func TestGrowthRobotHeaderTagsTheEventsAndLiftsNothing(t *testing.T) {
	g := newGrowthWatch(t)
	ctx := context.Background()
	g.wa.statusKnobs.MintPerIPPerDay = 1
	uniq := time.Now().UnixNano() % 100000
	robot := func(r *http.Request) { r.Header.Set("X-Upcontrol-Source", "outreach") }
	prospect := fmt.Sprintf("prospect-%d.example.com", uniq)
	visitor := fmt.Sprintf("visitor-%d.example.com", uniq)
	const ip = "198.51.100.50"

	if w := g.post(t, "/public/watch", `{"host":"`+prospect+`"}`, ip, robot); w.Code != http.StatusOK {
		t.Fatalf("robot watch = %d (%s), want 200", w.Code, w.Body.String())
	}
	g.liftCooldowns()
	second := g.post(t, "/public/watch", fmt.Sprintf(`{"host":"second-%d.example.com"}`, uniq), ip, robot)
	if second.Code != http.StatusTooManyRequests || !strings.Contains(second.Body.String(), "mint_ip_ceiling") {
		t.Fatalf("robot's second mint from one address = %d (%s), want 429 mint_ip_ceiling", second.Code, second.Body.String())
	}
	if w := g.post(t, "/public/watch", `{"host":"`+visitor+`"}`, "198.51.100.51"); w.Code != http.StatusOK {
		t.Fatalf("visitor watch = %d (%s), want 200", w.Code, w.Body.String())
	}
	// The check door's cached answer records its event with no probe at all.
	g.wa.cacheCheck("https://"+prospect, map[string]any{"groups": []any{}, "networkChecks": []any{}})
	if w := g.post(t, "/public/check", `{"host":"`+prospect+`"}`, "198.51.100.52", robot); w.Code != http.StatusOK {
		t.Fatalf("robot check = %d (%s), want 200", w.Code, w.Body.String())
	}

	if err := g.rec.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	source := func(name, host string) *string {
		t.Helper()
		var s *string
		if err := g.pool.Raw().QueryRow(ctx,
			`SELECT props->>'source' FROM web_events WHERE name = $1 AND props->>'host' = $2`, name, host).Scan(&s); err != nil {
			t.Fatalf("read %s for %s: %v", name, host, err)
		}
		return s
	}
	if s := source("watch_signup", prospect); s == nil || *s != "outreach" {
		t.Fatalf("robot's watch_signup source = %v, want outreach", s)
	}
	if s := source("public_check_run", prospect); s == nil || *s != "outreach" {
		t.Fatalf("robot's public_check_run source = %v, want outreach", s)
	}
	if s := source("watch_signup", visitor); s != nil {
		t.Fatalf("a visitor's watch_signup source = %q, want none", *s)
	}
	if n := oneInt(t, g.pool,
		`SELECT count(*) FROM web_events WHERE name = 'page_minted' AND props->>'source' = 'outreach'`); n != 1 {
		t.Fatalf("robot page_minted rows = %d, want 1", n)
	}
	if n := oneInt(t, g.pool,
		`SELECT count(*) FROM web_events WHERE name = 'page_minted' AND props->>'source' = 'watch'`); n != 1 {
		t.Fatalf("visitor page_minted rows = %d, want 1 (the door's own source kept)", n)
	}
}
