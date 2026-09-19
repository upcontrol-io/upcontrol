package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/ingest"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// webStubKeys resolves every key to a fixed tenant, the door tests' keyring.
type webStubKeys struct {
	tenant ingest.Tenant
	err    error
}

func (s *webStubKeys) Resolve(_ context.Context, _ string) (ingest.Tenant, error) {
	return s.tenant, s.err
}

func publicWebTenant() ingest.Tenant {
	return ingest.Tenant{
		TenantID:  7,
		ProjectID: 9,
		Kind:      ingest.KeyKindPublic,
		Origins:   []string{"https://example.com"},
	}
}

// fakeWebStore records every call; the reads answer fixed values.
type fakeWebStore struct {
	pageviews []struct {
		tenant, project int64
		ts              time.Time
		labels          map[string]string
		actor           string
	}
	heatCalls []struct {
		tenant, project int64
		day             time.Time
		path, device    string
		cells           []pgstore.HeatAdd
	}

	salt      []byte
	origins   []string
	resolveOK bool
	histMsg   string
	histPlan  string
	histDays  int // > 0: a plan this deep, refusing anything deeper
	views     int64
	cells     map[string][]pgstore.HeatCell
	scroll    [21]int64
	quota     pgstore.WebQuota
	heatFrom  time.Time // the fromDay HeatCells was last asked for
}

func (f *fakeWebStore) WebQuota(context.Context, int64, time.Time) (pgstore.WebQuota, error) {
	return f.quota, nil
}

func (f *fakeWebStore) InsertPageview(_ context.Context, tenantID, projectID int64, ts time.Time, labels map[string]string, actor string) error {
	f.pageviews = append(f.pageviews, struct {
		tenant, project int64
		ts              time.Time
		labels          map[string]string
		actor           string
	}{tenantID, projectID, ts, labels, actor})
	return nil
}

func (f *fakeWebStore) AddHeat(_ context.Context, tenantID, projectID int64, day time.Time, path, device string, cells []pgstore.HeatAdd) error {
	f.heatCalls = append(f.heatCalls, struct {
		tenant, project int64
		day             time.Time
		path, device    string
		cells           []pgstore.HeatAdd
	}{tenantID, projectID, day, path, device, cells})
	return nil
}

func (f *fakeWebStore) DailySalt(_ context.Context, _ time.Time) ([]byte, error) {
	if f.salt == nil {
		f.salt = []byte("0123456789abcdef")
	}
	return f.salt, nil
}

func (f *fakeWebStore) PublicOrigins(context.Context, int64, int64) ([]string, error) {
	return f.origins, nil
}

func (f *fakeWebStore) MintHeatmapLink(context.Context, []byte, int64, int64, time.Time) error {
	return nil
}

func (f *fakeWebStore) ResolveHeatmapLink(_ context.Context, _ []byte) (int64, int64, bool, error) {
	if !f.resolveOK {
		return 0, 0, false, nil
	}
	return 7, 9, true, nil
}

func (f *fakeWebStore) PageviewCount(context.Context, int64, int64, string, string, time.Time) (int64, error) {
	return f.views, nil
}

// HeatCells answers [] for a kind with no cells, never nil, as the real store does.
func (f *fakeWebStore) HeatCells(_ context.Context, _, _ int64, _, _, kind string, fromDay time.Time, _ int) ([]pgstore.HeatCell, error) {
	f.heatFrom = fromDay
	return append([]pgstore.HeatCell{}, f.cells[kind]...), nil
}

func (f *fakeWebStore) ScrollReach(context.Context, int64, int64, string, string, time.Time) ([21]int64, error) {
	return f.scroll, nil
}

func (f *fakeWebStore) HistoryRefusal(_ context.Context, _ int64, days int) (string, string, error) {
	if f.histDays > 0 && days > f.histDays {
		return "deeper than the plan", "indie", nil
	}
	return f.histMsg, f.histPlan, nil
}

func newTestDoor(t ingest.Tenant, store webStore, isBot func(string) bool) *webDoor {
	return &webDoor{
		ing:   ingest.New(ingest.Deps{Keys: &webStubKeys{tenant: t}, IsBot: isBot}),
		store: store,
		describe: func(_, _ string) (string, string, string) {
			return "DE", "macos", "safari"
		},
		now: func() time.Time { return time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC) },
	}
}

func postBeacon(t *testing.T, d *webDoor, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/w?key=uc_pub_test", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	d.Collect(rr, req)
	return rr
}

var webActorHex = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestDeviceFor(t *testing.T) {
	for _, c := range []struct {
		w    int
		want string
	}{
		{767, "mobile"},
		{768, "tablet"},
		{1023, "tablet"},
		{1024, "desktop"},
	} {
		if got := deviceFor(c.w); got != c.want {
			t.Errorf("deviceFor(%d) = %q, want %q", c.w, got, c.want)
		}
	}
}

func TestReferrerHost(t *testing.T) {
	for _, c := range []struct {
		ref, page, want string
	}{
		{"https://google.com/search?q=x", "example.com", "google.com"},
		{"https://www.google.com/", "example.com", "google.com"}, // www stripped
		{"https://example.com/other", "example.com", ""},         // own host
		{"https://www.example.com/other", "example.com", ""},     // www twin of own host
		{"://not a url", "example.com", ""},
		{"", "example.com", ""},
	} {
		if got := referrerHost(c.ref, c.page); got != c.want {
			t.Errorf("referrerHost(%q, %q) = %q, want %q", c.ref, c.page, got, c.want)
		}
	}
}

func TestUTMLabels(t *testing.T) {
	got := utmLabels("?utm_source=news&utm_medium=email&utm_campaign=launch&other=x", false)
	want := map[string]string{"utm_source": "news", "utm_medium": "email", "utm_campaign": "launch"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("utmLabels[%q] = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("utmLabels kept %d keys, want %d", len(got), len(want))
	}
	if utmLabels("?utm_source=&foo=1", false) != nil {
		t.Error("no kept value must answer nil, never an empty map")
	}
	if utmLabels("?utm_source="+strings.Repeat("a", 150), false)["utm_source"] != strings.Repeat("a", 100) {
		t.Error("utm value not capped to 100 bytes")
	}
}

func TestWebPath(t *testing.T) {
	if _, ok := webPath("pricing", false); ok {
		t.Error("a path without a leading / is not a path")
	}
	if _, ok := webPath("/"+strings.Repeat("a", maxWebPath), false); ok {
		t.Error("a path one byte over the cap must be refused")
	}
	// A percent-encoded non-ASCII slug is long on the wire and still a page.
	slug := "/" + strings.Repeat("%D0%BF", 150)
	if got, ok := webPath(slug, false); !ok || got != slug {
		t.Errorf("a %d-byte encoded slug = %q, %v; want it kept", len(slug), got, ok)
	}
	// The cap is on what gets stored: a redaction marker outgrows the address
	// it replaced, so a path at the cap before the scrub is over it after.
	if _, ok := webPath("/"+strings.Repeat("a", maxWebPath-16)+"/me@example.com", false); ok {
		t.Error("a path the scrub grows past the cap must be refused")
	}
	got, ok := webPath("/pricing", false)
	if !ok || got != "/pricing" {
		t.Errorf("webPath(/pricing) = %q, %v", got, ok)
	}
}

func TestCollectRefusesSecretKey(t *testing.T) {
	store := &fakeWebStore{}
	d := newTestDoor(ingest.Tenant{TenantID: 7, ProjectID: 9}, store, nil)
	rr := postBeacon(t, d, `{"v":1,"t":"view","p":"/","w":1200}`, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401: a secret key in a page is refused", rr.Code)
	}
	if len(store.pageviews) != 0 || len(store.heatCalls) != 0 {
		t.Error("a refused key wrote anyway")
	}
}

func TestCollectRefusesUnlistedOrigin(t *testing.T) {
	store := &fakeWebStore{}
	d := newTestDoor(publicWebTenant(), store, nil)
	rr := postBeacon(t, d, `{"v":1,"t":"view","p":"/","w":1200}`, map[string]string{"Origin": "https://evil.com"})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rr.Code)
	}
	if len(store.pageviews) != 0 {
		t.Error("an unlisted origin wrote a page view")
	}
}

func TestCollectDropsBots(t *testing.T) {
	store := &fakeWebStore{}
	d := newTestDoor(publicWebTenant(), store, func(ua string) bool { return strings.Contains(ua, "bot") })
	rr := postBeacon(t, d, `{"v":1,"t":"view","p":"/","w":1200}`, map[string]string{
		"Origin":     "https://example.com",
		"User-Agent": "Mozilla/5.0 (compatible; Googlebot/2.1)",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 - a 4xx teaches a crawler to retry", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "bot_dropped") {
		t.Errorf("body %q, want a bot_dropped warning", rr.Body.String())
	}
	if len(store.pageviews) != 0 || len(store.heatCalls) != 0 {
		t.Error("a bot wrote anyway")
	}
}

func TestCollectView(t *testing.T) {
	store := &fakeWebStore{}
	d := newTestDoor(publicWebTenant(), store, nil)
	rr := postBeacon(t, d,
		`{"v":1,"t":"view","p":"/pricing","w":390,"r":"https://www.google.com/search","q":"?utm_source=news&utm_medium=email&utm_campaign=july"}`,
		map[string]string{"Origin": "https://example.com", "User-Agent": "Mozilla/5.0"})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if len(store.pageviews) != 1 {
		t.Fatalf("%d page views, want 1", len(store.pageviews))
	}
	pv := store.pageviews[0]
	if pv.tenant != 7 || pv.project != 9 {
		t.Errorf("page view landed on tenant %d project %d", pv.tenant, pv.project)
	}
	for k, want := range map[string]string{
		"path": "/pricing", "device": "mobile", "referrer": "google.com",
		"utm_source": "news", "utm_medium": "email", "utm_campaign": "july",
		"country": "DE", "os": "macos", "browser": "safari",
	} {
		if pv.labels[k] != want {
			t.Errorf("labels[%q] = %q, want %q", k, pv.labels[k], want)
		}
	}
	if !webActorHex.MatchString(pv.actor) {
		t.Errorf("actor %q is not 16 hex chars", pv.actor)
	}
	if !pv.ts.Equal(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("ts %v, want the door's clock", pv.ts)
	}
}

// heatCellsOf picks one kind's cells out of the recorded AddHeat call.
func heatCellsOf(t *testing.T, store *fakeWebStore, kind string) []pgstore.HeatAdd {
	t.Helper()
	if len(store.heatCalls) != 1 {
		t.Fatalf("%d AddHeat calls, want 1", len(store.heatCalls))
	}
	var out []pgstore.HeatAdd
	for _, c := range store.heatCalls[0].cells {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func TestCollectHeatCapsAndClamps(t *testing.T) {
	store := &fakeWebStore{}
	d := newTestDoor(publicWebTenant(), store, nil)

	// 301 click entries: the cap keeps the first 300.
	var b strings.Builder
	b.WriteString(`{"v":1,"t":"heat","p":"/","w":1200,"c":[`)
	for i := 0; i < 301; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `["s%d",1,2,1,0]`, i)
	}
	b.WriteString(`]}`)
	rr := postBeacon(t, d, b.String(), map[string]string{"Origin": "https://example.com"})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if got := len(heatCellsOf(t, store, "click")); got != 300 {
		t.Errorf("%d click cells, want the 300 cap", got)
	}

	// fx past 63 clamps down; rage past clicks clamps to clicks; a malformed
	// entry (wrong length, wrong types, empty selector) is skipped, not a 400.
	store2 := &fakeWebStore{}
	d2 := newTestDoor(publicWebTenant(), store2, nil)
	rr = postBeacon(t, d2, `{"v":1,"t":"heat","p":"/","w":1200,"c":[
		["a",99,0,3,5],
		["skipped",1],
		["skipped","x","y",1,1],
		["",1,2,1,0]
	]}`, map[string]string{"Origin": "https://example.com"})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204 - a malformed entry is skipped: %s", rr.Code, rr.Body.String())
	}
	clicks := heatCellsOf(t, store2, "click")
	if len(clicks) != 1 {
		t.Fatalf("%d click cells, want 1", len(clicks))
	}
	if clicks[0].FX != 63 || clicks[0].N != 3 {
		t.Errorf("click cell = fx %d n %d, want fx 63 (clamped) n 3", clicks[0].FX, clicks[0].N)
	}
	rage := heatCellsOf(t, store2, "rage")
	if len(rage) != 1 || rage[0].N != 3 {
		t.Errorf("rage cells = %+v, want one clamped to the 3 clicks", rage)
	}

	// Duplicate entries merge into one cell with the summed n.
	store3 := &fakeWebStore{}
	d3 := newTestDoor(publicWebTenant(), store3, nil)
	rr = postBeacon(t, d3, `{"v":1,"t":"heat","p":"/","w":1200,"c":[["a",1,2,5,0],["a",1,2,5,0]]}`,
		map[string]string{"Origin": "https://example.com"})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	merged := heatCellsOf(t, store3, "click")
	if len(merged) != 1 || merged[0].N != 10 {
		t.Errorf("merged cells = %+v, want one click cell of n 10", merged)
	}
}

func TestCollectHeatScroll(t *testing.T) {
	store := &fakeWebStore{}
	d := newTestDoor(publicWebTenant(), store, nil)
	rr := postBeacon(t, d, `{"v":1,"t":"heat","p":"/","w":1200,"s":1.0,"m":[["a",10,10,7]]}`,
		map[string]string{"Origin": "https://example.com"})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	scroll := heatCellsOf(t, store, "scroll")
	if len(scroll) != 1 || scroll[0].FY != 20 || scroll[0].N != 1 {
		t.Errorf("scroll cells = %+v, want fy 20 (a full-page reach) n 1", scroll)
	}
	moves := heatCellsOf(t, store, "move")
	if len(moves) != 1 || moves[0].N != 7 {
		t.Errorf("move cells = %+v, want the 7 samples", moves)
	}
	if calls := len(store.heatCalls); calls != 1 {
		t.Errorf("%d AddHeat calls, want 1 - everything lands in one statement", calls)
	}
}

func TestCollectBodyTooLarge(t *testing.T) {
	store := &fakeWebStore{}
	d := newTestDoor(publicWebTenant(), store, nil)
	big := `{"v":1,"t":"heat","p":"/","w":1200,"pad":"` + strings.Repeat("a", webBodyBytes) + `"}`
	rr := postBeacon(t, d, big, map[string]string{"Origin": "https://example.com"})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", rr.Code)
	}
	if len(store.heatCalls) != 0 {
		t.Error("an oversized body wrote anyway")
	}
}

func getHeatmap(t *testing.T, d *webDoor, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	d.Heatmap(rr, req)
	return rr
}

func fakeReadStore() *fakeWebStore {
	s := &fakeWebStore{
		resolveOK: true,
		origins:   []string{"https://example.com"},
		views:     42,
		cells: map[string][]pgstore.HeatCell{
			"click": {{Selector: "a", X: 1, Y: 2, N: 5}},
		},
		scroll: [21]int64{10, 9, 8, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7},
	}
	return s
}

func TestHeatmapRefusesUnknownToken(t *testing.T) {
	store := &fakeWebStore{resolveOK: false}
	d := newTestDoor(publicWebTenant(), store, nil)
	rr := getHeatmap(t, d, "/w/heatmap?token=uch_stale&key=uc_pub_test&path=/&w=1200", map[string]string{"Origin": "https://example.com"})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("ACAO = %q, want the echoed origin so the overlay can read the refusal", got)
	}
	if !strings.Contains(rr.Body.String(), "expired") {
		t.Errorf("body %q, want the expired-link message", rr.Body.String())
	}

	// A token that is not even uch_-shaped is the same refusal.
	rr = getHeatmap(t, d, "/w/heatmap?token=guess&key=uc_pub_test&path=/&w=1200", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rr.Code)
	}
}

// The read is bound to the page's own public key: anyone can list a stranger's
// origin on a key of their own, and a token of theirs must not draw their data
// on the stranger's page. The token resolves to tenant 7, project 9.
func TestHeatmapRefusesAKeyThatIsNotTheTokensProject(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tenant ingest.Tenant
		target string
	}{
		{"another project's public key", ingest.Tenant{TenantID: 8, ProjectID: 10, Kind: ingest.KeyKindPublic}, "&key=uc_pub_other"},
		{"a sibling project's public key", ingest.Tenant{TenantID: 7, ProjectID: 10, Kind: ingest.KeyKindPublic}, "&key=uc_pub_sibling"},
		{"the project's secret key", ingest.Tenant{TenantID: 7, ProjectID: 9}, "&key=uc_live_test"},
		{"no key at all", publicWebTenant(), ""},
	} {
		d := newTestDoor(tc.tenant, fakeReadStore(), nil)
		rr := getHeatmap(t, d, "/w/heatmap?token=uch_good&path=/&w=1200"+tc.target, map[string]string{"Origin": "https://example.com"})
		if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "bad_token") {
			t.Errorf("%s: %d %s, want 401 bad_token", tc.name, rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
			t.Errorf("%s: ACAO = %q, want the echoed origin so the overlay can read the refusal", tc.name, got)
		}
	}
}

func TestHeatmapAnswersForListedOrigin(t *testing.T) {
	store := fakeReadStore()
	d := newTestDoor(publicWebTenant(), store, nil)
	rr := getHeatmap(t, d, "/w/heatmap?token=uch_good&key=uc_pub_test&path=/pricing&w=390", map[string]string{"Origin": "https://example.com"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("ACAO = %q, want the listed origin echoed", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Error("the overlay's read sends no credentials; the header invites a preflight it cannot pass")
	}
	var out heatOut
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// No range asked on an unlimited plan: the deepest one.
	if out.Path != "/pricing" || out.Device != "mobile" || out.Range != "31d" || out.Views != 42 {
		t.Errorf("answer = path %q device %q range %q views %d", out.Path, out.Device, out.Range, out.Views)
	}
	if len(out.Clicks) != 1 || out.Clicks[0].Selector != "a" || out.Clicks[0].N != 5 {
		t.Errorf("clicks = %+v, want the store's one cell", out.Clicks)
	}
	if len(out.Scroll) != 21 || out.Scroll[3] != 7 {
		t.Errorf("scroll[3] = %d of %d, want the store's cumulative array passed through", out.Scroll[3], len(out.Scroll))
	}
	// The empty kinds are [] on the wire, never null.
	for _, cells := range [][]pgstore.HeatCell{out.Rage, out.Moves} {
		if cells == nil {
			t.Error("an empty cell list answered null, want []")
		}
	}
}

func TestHeatmapSilentForUnlistedOrigin(t *testing.T) {
	store := fakeReadStore()
	d := newTestDoor(publicWebTenant(), store, nil)
	rr := getHeatmap(t, d, "/w/heatmap?token=uch_good&key=uc_pub_test&path=/&w=1200", map[string]string{"Origin": "https://evil.com"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want none: an unlisted origin reads nothing", got)
	}
}

func TestHeatmapHistoryWall(t *testing.T) {
	store := fakeReadStore()
	store.histMsg = "31 days of history is on Growth and up. It counts from the day you switch."
	store.histPlan = "growth"
	d := newTestDoor(publicWebTenant(), store, nil)
	rr := getHeatmap(t, d, "/w/heatmap?token=uch_good&key=uc_pub_test&path=/&w=1200&range=31d", nil)
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("status %d, want the 402 POST /v1/series sends", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Growth") {
		t.Errorf("body %q, want the wall's message", rr.Body.String())
	}
}

func TestHeatmapNoRangeTakesTheDeepestThePlanReaches(t *testing.T) {
	for _, tc := range []struct {
		days int
		want string
	}{{1, "24h"}, {7, "7d"}, {31, "31d"}} {
		store := fakeReadStore()
		store.histDays = tc.days
		d := newTestDoor(publicWebTenant(), store, nil)
		rr := getHeatmap(t, d, "/w/heatmap?token=uch_good&key=uc_pub_test&path=/&w=1200", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%d-day plan: status %d, want 200: a first look is never a wall", tc.days, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), `"range":"`+tc.want+`"`) {
			t.Errorf("%d-day plan: body %s, want range %s", tc.days, rr.Body.String(), tc.want)
		}
	}
}

// Past the month's visits a beacon stores nothing, and the page's browser is
// answered exactly as an accepted beacon is: a visitor has nothing to be told.
func TestCollectOverTheVisitCapStoresNothingQuietly(t *testing.T) {
	limit := int32(3000)
	origin := map[string]string{"Origin": "https://example.com"}
	for _, body := range []string{
		`{"v":1,"t":"view","p":"/","w":1200}`,
		`{"v":1,"t":"heat","p":"/","w":1200,"s":0.5,"c":[["a",1,2,1,0]]}`,
	} {
		store := &fakeWebStore{quota: pgstore.WebQuota{Visits: 3000, MaxVisits: &limit}}
		rr := postBeacon(t, newTestDoor(publicWebTenant(), store, nil), body, origin)
		if rr.Code != http.StatusNoContent || rr.Body.Len() != 0 {
			t.Errorf("%s: %d %q, want the accepted beacon's bare 204", body, rr.Code, rr.Body.String())
		}
		if len(store.pageviews) != 0 || len(store.heatCalls) != 0 {
			t.Errorf("%s: stored past the cap", body)
		}
	}
	store := &fakeWebStore{quota: pgstore.WebQuota{Visits: 2999, MaxVisits: &limit}}
	if rr := postBeacon(t, newTestDoor(publicWebTenant(), store, nil), `{"v":1,"t":"view","p":"/","w":1200}`, origin); rr.Code != http.StatusNoContent || len(store.pageviews) != 1 {
		t.Errorf("under the cap: %d, %d page views, want 204 and the view stored", rr.Code, len(store.pageviews))
	}
}

// The read carries the plan's heatmap pages, absent when unlimited, and a
// range past a week reads its heat from the first day's Monday, where the
// compaction keeps that week's bucket. The door's clock is Saturday
// 2026-09-19.
func TestHeatmapCarriesHeatPagesAndSnapsToTheWeek(t *testing.T) {
	pages := int32(20)
	store := fakeReadStore()
	store.quota.HeatPages = &pages
	d := newTestDoor(publicWebTenant(), store, nil)
	for _, tc := range []struct {
		rng  string
		want time.Time
	}{
		{"31d", time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)}, // Wednesday 08-19's Monday
		{"7d", time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)},  // daily rows: no snap
	} {
		rr := getHeatmap(t, d, "/w/heatmap?token=uch_good&key=uc_pub_test&path=/&w=1200&range="+tc.rng, nil)
		var out heatOut
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: %d %s", tc.rng, rr.Code, rr.Body.String())
		}
		if !store.heatFrom.Equal(tc.want) {
			t.Errorf("%s: heat read from %v, want %v", tc.rng, store.heatFrom, tc.want)
		}
		if out.HeatPages == nil || *out.HeatPages != 20 {
			t.Errorf("%s: heatPages = %v, want the plan's 20", tc.rng, out.HeatPages)
		}
	}
	store.quota.HeatPages = nil
	if rr := getHeatmap(t, d, "/w/heatmap?token=uch_good&key=uc_pub_test&path=/&w=1200", nil); strings.Contains(rr.Body.String(), "heatPages") {
		t.Errorf("an unlimited plan's read = %s, want no heatPages", rr.Body.String())
	}
}
