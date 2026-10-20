package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
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
	events []struct {
		name   string
		labels map[string]string
		actor  string
	}
	heatCalls []struct {
		tenant, project int64
		day             time.Time
		path, device    string
		segment, source string
		cells           []pgstore.HeatAdd
	}
	engageCalls []struct {
		hour            time.Time
		path, device    string
		segment, source string
		add             pgstore.EngageAdd
	}

	declared     []string // the project's declared segments
	segmentReads int
	noView       bool      // the visitor has no stored page view: events and engaged time store nothing
	readFilter   [2]string // the segment and source the last read was narrowed to

	salt      []byte
	origins   []string
	resolveOK bool
	histMsg   string
	histPlan  string
	histDays  int // > 0: a plan this deep, refusing anything deeper
	views     int64
	cells     map[string][]pgstore.HeatCell
	scroll    [21]int64
	forms     []pgstore.FormStat
	quota     pgstore.WebQuota
	heatFrom  time.Time // the fromDay HeatCells was last asked for
	reads     []fakeRead

	deploys, releases []pgstore.MarkRow
	deployWindow      [2]time.Time // the [from, to) RecentDeploys was asked for
}

// fakeRead is one heatmap read: what ("views", a heat kind, "scroll",
// "forms"), its [from, to) and its filters.
type fakeRead struct {
	what            string
	from, to        time.Time
	segment, source string
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

func (f *fakeWebStore) InsertWebEvent(_ context.Context, _, _ int64, _ time.Time, name string, labels map[string]string, actor string) error {
	if f.noView {
		return nil
	}
	f.events = append(f.events, struct {
		name   string
		labels map[string]string
		actor  string
	}{name, labels, actor})
	return nil
}

func (f *fakeWebStore) AddHeat(_ context.Context, tenantID, projectID int64, day time.Time, path, device, segment, source string, cells []pgstore.HeatAdd) error {
	f.heatCalls = append(f.heatCalls, struct {
		tenant, project int64
		day             time.Time
		path, device    string
		segment, source string
		cells           []pgstore.HeatAdd
	}{tenantID, projectID, day, path, device, segment, source, cells})
	return nil
}

func (f *fakeWebStore) AddEngage(_ context.Context, _, _ int64, hour time.Time, path, device, segment, source string, add pgstore.EngageAdd) (bool, error) {
	if f.noView {
		return false, nil
	}
	f.engageCalls = append(f.engageCalls, struct {
		hour            time.Time
		path, device    string
		segment, source string
		add             pgstore.EngageAdd
	}{hour, path, device, segment, source, add})
	return true, nil
}

func (f *fakeWebStore) Segments(context.Context, int64) ([]string, error) {
	f.segmentReads++
	return append([]string{}, f.declared...), nil
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

func (f *fakeWebStore) PageviewCount(_ context.Context, _, _ int64, _, _, segment, source string, from, to time.Time) (int64, error) {
	f.readFilter = [2]string{segment, source}
	f.reads = append(f.reads, fakeRead{"views", from, to, segment, source})
	return f.views, nil
}

// HeatCells answers [] for a kind with no cells, never nil, as the real store does.
func (f *fakeWebStore) HeatCells(_ context.Context, _, _ int64, _, _, segment, source, kind string, fromDay, toDay time.Time, _ int) ([]pgstore.HeatCell, error) {
	f.heatFrom = fromDay
	f.readFilter = [2]string{segment, source}
	f.reads = append(f.reads, fakeRead{kind, fromDay, toDay, segment, source})
	return append([]pgstore.HeatCell{}, f.cells[kind]...), nil
}

func (f *fakeWebStore) ScrollReach(_ context.Context, _, _ int64, _, _, segment, source string, fromDay, toDay time.Time) ([21]int64, error) {
	f.readFilter = [2]string{segment, source}
	f.reads = append(f.reads, fakeRead{"scroll", fromDay, toDay, segment, source})
	return f.scroll, nil
}

// FormStats answers [] for no rows, never nil, as the real store does.
func (f *fakeWebStore) FormStats(_ context.Context, _, _ int64, _, _, segment, source string, fromDay, toDay time.Time) ([]pgstore.FormStat, error) {
	f.readFilter = [2]string{segment, source}
	f.reads = append(f.reads, fakeRead{"forms", fromDay, toDay, segment, source})
	return append([]pgstore.FormStat{}, f.forms...), nil
}

func (f *fakeWebStore) RecentDeploys(_ context.Context, _, _ int64, from, to time.Time) ([]pgstore.MarkRow, []pgstore.MarkRow, error) {
	f.deployWindow = [2]time.Time{from, to}
	return f.deploys, f.releases, nil
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
		now:      func() time.Time { return time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC) },
		segments: &segmentCache{},
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

var webActorHex = regexp.MustCompile(`^w:[0-9a-f]{16}$`)

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
		t.Errorf("actor %q is not w: and 16 hex chars", pv.actor)
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
			"hit":   {{Selector: "a", N: 3}},
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
	if len(out.Hits) != 1 || out.Hits[0].Selector != "a" || out.Hits[0].N != 3 {
		t.Errorf("hits = %+v, want the store's one hit cell", out.Hits)
	}
	// The empty kinds are [] on the wire, never null.
	for _, cells := range [][]pgstore.HeatCell{out.Rage, out.Moves, out.Dead} {
		if cells == nil {
			t.Error("an empty cell list answered null, want []")
		}
	}
	if !strings.Contains(rr.Body.String(), `"dead":[]`) {
		t.Errorf("body %s, want dead as [] on the wire", rr.Body.String())
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

// f is the path the tab came from: stored as `from` when it is a path, and
// dropped without a word when it is not.
func TestCollectViewFrom(t *testing.T) {
	origin := map[string]string{"Origin": "https://example.com"}
	for _, tc := range []struct{ f, want string }{
		{"/pricing", "/pricing"},
		{"pricing", ""},
		{"/" + strings.Repeat("a", maxWebPath), ""},
	} {
		store := &fakeWebStore{}
		body := fmt.Sprintf(`{"v":2,"t":"view","p":"/signup","w":1200,"f":%q}`, tc.f)
		if rr := postBeacon(t, newTestDoor(publicWebTenant(), store, nil), body, origin); rr.Code != http.StatusNoContent || len(store.pageviews) != 1 {
			t.Fatalf("f %q: %d, %d page views; want 204 and the view stored", tc.f, rr.Code, len(store.pageviews))
		}
		got, ok := store.pageviews[0].labels["from"]
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("f %q: from = %q (%v), want %q", tc.f, got, ok, tc.want)
		}
	}
}

// h counts a view once per element it clicked: a selector repeated in one
// beacon is still one hit. d is dead clicks, clamped like clicks.
func TestCollectHitsAndDeadClicks(t *testing.T) {
	store := &fakeWebStore{}
	d := newTestDoor(publicWebTenant(), store, nil)
	rr := postBeacon(t, d, `{"v":2,"t":"heat","p":"/","w":1200,
		"h":["a","a","b","",7],
		"d":[["x",99,-5,5000],["x",99,-5,1],["skipped",1],["",1,1,1]]}`,
		map[string]string{"Origin": "https://example.com"})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	hits := heatCellsOf(t, store, "hit")
	if len(hits) != 2 {
		t.Fatalf("hits = %+v, want a and b once each", hits)
	}
	for _, h := range hits {
		if h.N != 1 || h.FX != 0 || h.FY != 0 {
			t.Errorf("hit %+v, want n 1 at 0,0", h)
		}
	}
	dead := heatCellsOf(t, store, "dead")
	if len(dead) != 1 || dead[0].FX != 63 || dead[0].FY != 0 || dead[0].N != 1001 {
		t.Errorf("dead = %+v, want one cell at 63,0 of the clamped 1000 plus 1", dead)
	}
}

// e and a reach AddEngage in the beacon's hour: e clamped to 30 minutes, a
// deduped and kept to 0..3, a wrong-typed one ignored rather than refused.
func TestCollectEngage(t *testing.T) {
	origin := map[string]string{"Origin": "https://example.com"}
	for _, tc := range []struct {
		name, extra string
		want        pgstore.EngageAdd
	}{
		{"clamped e and deduped a", `"e":9999999,"a":[0,0,3,4,-1,"x"]`, pgstore.EngageAdd{MS: 1_800_000, R10: 1, R300: 1}},
		{"a wrong-typed e", `"e":"lots","a":[1]`, pgstore.EngageAdd{R30: 1}},
		{"a wrong-typed a", `"e":1500,"a":"all"`, pgstore.EngageAdd{MS: 1500}},
	} {
		store := &fakeWebStore{}
		body := `{"v":2,"t":"heat","p":"/docs","w":390,` + tc.extra + `}`
		if rr := postBeacon(t, newTestDoor(publicWebTenant(), store, nil), body, origin); rr.Code != http.StatusNoContent {
			t.Fatalf("%s: status %d: %s", tc.name, rr.Code, rr.Body.String())
		}
		if len(store.engageCalls) != 1 {
			t.Fatalf("%s: %d AddEngage calls, want 1", tc.name, len(store.engageCalls))
		}
		c := store.engageCalls[0]
		if c.add != tc.want || c.path != "/docs" || c.device != "mobile" || !c.hour.Equal(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("%s: AddEngage(%v, %q, %q, %+v), want the hour, /docs, mobile, %+v", tc.name, c.hour, c.path, c.device, c.add, tc.want)
		}
		// Nothing but engaged time is no heat at all.
		if len(store.heatCalls) != 0 {
			t.Errorf("%s: %d AddHeat calls for a beacon with no cells", tc.name, len(store.heatCalls))
		}
	}
	// A heat beacon with nothing engaged writes no engage row.
	store := &fakeWebStore{}
	postBeacon(t, newTestDoor(publicWebTenant(), store, nil), `{"v":2,"t":"heat","p":"/","w":1200,"e":0,"s":0.5}`, origin)
	if len(store.engageCalls) != 0 || len(store.heatCalls) != 1 {
		t.Errorf("%d AddEngage, %d AddHeat calls; want none and the scroll", len(store.engageCalls), len(store.heatCalls))
	}
}

func TestValidSegment(t *testing.T) {
	for s, want := range map[string]bool{
		"member": true, "trial_2": true, "a-b": true, "abc123": true, strings.Repeat("a", 24): true,
		"": false, strings.Repeat("a", 25): false, "Member": false, "a b": false, "pro.": false,
		"u1234": false, "2026": false, "x123y456": true,
	} {
		if got := validSegment(s); got != want {
			t.Errorf("validSegment(%q) = %v, want %v", s, got, want)
		}
	}
}

// g and o ride a view's labels and every heat and engage write; an invalid
// one, or a name the project never declared, is stored as none, never a 400.
func TestCollectSegmentAndSource(t *testing.T) {
	origin := map[string]string{"Origin": "https://example.com"}
	for _, tc := range []struct {
		name, g, o           string
		declared             []string
		wantSegment, wantSrc string
	}{
		{"declared", "member", "search", []string{"member"}, "member", "search"},
		{"undeclared", "member", "search", nil, "", "search"},
		{"invalid g and o", "user_12345", "google", []string{"member"}, "", ""},
		{"not among the four", "fifth", "direct", []string{"a", "b", "c", "d"}, "", "direct"},
		{"one of the four", "c", "", []string{"a", "b", "c", "d"}, "c", ""},
	} {
		store := &fakeWebStore{declared: tc.declared}
		d := newTestDoor(publicWebTenant(), store, nil)
		view := fmt.Sprintf(`{"v":2,"t":"view","p":"/","w":1200,"g":%q,"o":%q}`, tc.g, tc.o)
		heat := fmt.Sprintf(`{"v":2,"t":"heat","p":"/","w":1200,"g":%q,"o":%q,"s":0.5,"e":1000}`, tc.g, tc.o)
		for _, body := range []string{view, heat} {
			if rr := postBeacon(t, d, body, origin); rr.Code != http.StatusNoContent {
				t.Fatalf("%s: status %d: %s", tc.name, rr.Code, rr.Body.String())
			}
		}
		labels := store.pageviews[0].labels
		if got, ok := labels["segment"]; got != tc.wantSegment || ok != (tc.wantSegment != "") {
			t.Errorf("%s: labels segment = %q (%v), want %q", tc.name, got, ok, tc.wantSegment)
		}
		if got, ok := labels["source"]; got != tc.wantSrc || ok != (tc.wantSrc != "") {
			t.Errorf("%s: labels source = %q (%v), want %q", tc.name, got, ok, tc.wantSrc)
		}
		h, e := store.heatCalls[0], store.engageCalls[0]
		if h.segment != tc.wantSegment || h.source != tc.wantSrc || e.segment != tc.wantSegment || e.source != tc.wantSrc {
			t.Errorf("%s: heat (%q, %q), engage (%q, %q), want (%q, %q)", tc.name, h.segment, h.source, e.segment, e.source, tc.wantSegment, tc.wantSrc)
		}
		if slices.Contains(store.declared, "fifth") {
			t.Errorf("%s: a beacon declared a segment", tc.name)
		}
	}
}

// A site marking every beacon reads the project's names once per ten
// minutes, whatever it marks, refusals included; a PUT drops the project's
// entry, so a new name lands on the next beacon.
func TestCollectCachesSegments(t *testing.T) {
	origin := map[string]string{"Origin": "https://example.com"}
	store := &fakeWebStore{declared: []string{"a", "b", "c", "d"}}
	d := newTestDoor(publicWebTenant(), store, nil)
	clock := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	for range 5 {
		postBeacon(t, d, `{"v":2,"t":"view","p":"/","w":1200,"g":"a"}`, origin)
		postBeacon(t, d, `{"v":2,"t":"view","p":"/","w":1200,"g":"fifth"}`, origin)
	}
	if store.segmentReads != 1 {
		t.Fatalf("%d segment reads for 10 beacons of two names, want 1", store.segmentReads)
	}
	clock = clock.Add(segmentTTL + time.Second)
	postBeacon(t, d, `{"v":2,"t":"view","p":"/","w":1200,"g":"a"}`, origin)
	if store.segmentReads != 2 {
		t.Fatalf("%d segment reads past the TTL, want the names read again", store.segmentReads)
	}
	store.declared = []string{"fifth"}
	d.segments.forget(9)
	postBeacon(t, d, `{"v":2,"t":"view","p":"/","w":1200,"g":"fifth"}`, origin)
	if got := store.pageviews[len(store.pageviews)-1].labels["segment"]; got != "fifth" || store.segmentReads != 3 {
		t.Errorf("after forget: segment %q, %d reads; want fifth, read again", got, store.segmentReads)
	}
}

// A heat beacon with no stored view behind it adds no engaged time: the store
// updates no row, and the browser still gets its 204.
func TestCollectEngageWithoutAViewStoresNothing(t *testing.T) {
	store := &fakeWebStore{noView: true}
	rr := postBeacon(t, newTestDoor(publicWebTenant(), store, nil), `{"v":2,"t":"heat","p":"/","w":1200,"e":5000,"a":[0]}`,
		map[string]string{"Origin": "https://example.com"})
	if rr.Code != http.StatusNoContent || len(store.engageCalls) != 0 {
		t.Errorf("%d, %d engage rows; want 204 and none", rr.Code, len(store.engageCalls))
	}
}

// An event from a visitor with no page view today is not stored, and the
// browser still gets its 204.
func TestCollectEventWithoutAViewStoresNothing(t *testing.T) {
	store := &fakeWebStore{noView: true}
	rr := postBeacon(t, newTestDoor(publicWebTenant(), store, nil), `{"v":2,"t":"event","p":"/","w":1200,"n":"signup"}`,
		map[string]string{"Origin": "https://example.com"})
	if rr.Code != http.StatusNoContent || len(store.events) != 0 {
		t.Errorf("%d, %d events; want 204 and none", rr.Code, len(store.events))
	}
}

// The read narrows by segment and source, lists the project's segments, and
// refuses a filter it could never match.
func TestHeatmapSegmentAndSource(t *testing.T) {
	store := fakeReadStore()
	store.declared = []string{"member", "anon"}
	d := newTestDoor(publicWebTenant(), store, nil)
	base := "/w/heatmap?token=uch_good&key=uc_pub_test&path=/&w=1200"
	rr := getHeatmap(t, d, base+"&segment=member&source=social", nil)
	var out heatOut
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &out) != nil {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if store.readFilter != [2]string{"member", "social"} {
		t.Errorf("reads narrowed to %v, want member, social", store.readFilter)
	}
	if !slices.Equal(out.Segments, []string{"member", "anon"}) {
		t.Errorf("segments = %v, want the project's two, oldest first", out.Segments)
	}
	store.declared = nil
	if rr := getHeatmap(t, d, base, nil); !strings.Contains(rr.Body.String(), `"segments":[]`) || store.readFilter != [2]string{} {
		t.Errorf("no filter: %s, reads narrowed to %v; want segments [] and no filter", rr.Body.String(), store.readFilter)
	}
	for _, bad := range []string{"&segment=Member", "&segment=u12345", "&source=google"} {
		if rr := getHeatmap(t, d, base+bad, nil); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "bad_request") {
			t.Errorf("%s: %d %s, want 400 bad_request", bad, rr.Code, rr.Body.String())
		}
	}
}

// An event beacon is an events row under the same visitor as the request's
// page view: its name lowercased, its string fields filtered, capped and
// scrubbed, and the server's own labels winning over a field of that name.
func TestCollectEvent(t *testing.T) {
	headers := map[string]string{"Origin": "https://example.com", "User-Agent": "Mozilla/5.0"}
	store := &fakeWebStore{declared: []string{"member"}}
	d := newTestDoor(publicWebTenant(), store, nil)
	if rr := postBeacon(t, d, `{"v":2,"t":"view","p":"/pricing","w":1200,"g":"member","o":"search"}`, headers); rr.Code != http.StatusNoContent {
		t.Fatalf("view: %d %s", rr.Code, rr.Body.String())
	}
	fields := map[string]any{
		"plan": "pro", "email": "anna@example.com", "long": strings.Repeat("a", 150),
		"n": 3, "obj": map[string]any{"x": 1}, "uc.stat": "x", "": "empty-key",
		strings.Repeat("k", 61): "long-key", "path": "/forged", "device": "forged", "segment": "forged", "source": "forged",
	}
	for i := range 12 {
		fields[fmt.Sprintf("z%02d", i)] = "v"
	}
	l, _ := json.Marshal(fields)
	body := fmt.Sprintf(`{"v":2,"t":"event","p":"/pricing","w":1200,"g":"member","o":"search","n":"  Signup_Clicked ","l":%s}`, l)
	if rr := postBeacon(t, d, body, headers); rr.Code != http.StatusNoContent {
		t.Fatalf("event: %d %s", rr.Code, rr.Body.String())
	}
	if len(store.events) != 1 {
		t.Fatalf("%d events stored, want 1", len(store.events))
	}
	ev := store.events[0]
	if ev.name != "signup_clicked" {
		t.Errorf("name %q, want trimmed and lowercased", ev.name)
	}
	if ev.actor != store.pageviews[0].actor || !webActorHex.MatchString(ev.actor) {
		t.Errorf("event actor %q, page view actor %q: want the same w: visitor", ev.actor, store.pageviews[0].actor)
	}
	// Sorted valid keys: email, long, plan, z00..z06 is ten; z07 on is cut.
	want := map[string]string{
		"path": "/pricing", "device": "desktop", "segment": "member", "source": "search",
		"plan": "pro", "long": strings.Repeat("a", 100),
	}
	for k, v := range want {
		if ev.labels[k] != v {
			t.Errorf("labels[%q] = %q, want %q", k, ev.labels[k], v)
		}
	}
	if strings.Contains(ev.labels["email"], "anna@example.com") || ev.labels["email"] == "" {
		t.Errorf("email field %q, want it scrubbed", ev.labels["email"])
	}
	for _, k := range []string{"n", "obj", "uc.stat", "", strings.Repeat("k", 61), "z07"} {
		if _, ok := ev.labels[k]; ok {
			t.Errorf("labels kept %q", k)
		}
	}
	if len(ev.labels) != 14 {
		t.Errorf("%d labels, want 10 fields and 4 server labels: %v", len(ev.labels), ev.labels)
	}
	if len(store.pageviews) != 1 || len(store.heatCalls) != 0 || len(store.engageCalls) != 0 {
		t.Error("an event wrote a page view, heat or engaged time")
	}
}

// A server label the door does not set is still never the page's: a forged
// segment on an undeclared beacon does not survive.
func TestCollectEventDropsForgedServerLabels(t *testing.T) {
	store := &fakeWebStore{}
	d := newTestDoor(publicWebTenant(), store, nil)
	body := `{"v":2,"t":"event","p":"/","w":1200,"n":"x","l":{"segment":"vip","source":"search"}}`
	if rr := postBeacon(t, d, body, map[string]string{"Origin": "https://example.com"}); rr.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if l := store.events[0].labels; l["segment"] != "" || l["source"] != "" {
		t.Errorf("labels %v carry the page's segment or source", l)
	}
}

func TestCollectEventRefusesBadNames(t *testing.T) {
	for _, n := range []string{`""`, `"   "`, `"uc.pageview"`, `"UC.Custom"`, `"a\u0007b"`, fmt.Sprintf("%q", strings.Repeat("a", 41)), `5`} {
		store := &fakeWebStore{}
		body := fmt.Sprintf(`{"v":2,"t":"event","p":"/","w":1200,"n":%s}`, n)
		rr := postBeacon(t, newTestDoor(publicWebTenant(), store, nil), body, map[string]string{"Origin": "https://example.com"})
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "bad_beacon") {
			t.Errorf("n=%s: %d %s, want 400 bad_beacon", n, rr.Code, rr.Body.String())
		}
		if len(store.events) != 0 {
			t.Errorf("n=%s: stored", n)
		}
	}
	store := &fakeWebStore{}
	body := fmt.Sprintf(`{"v":2,"t":"event","p":"/","w":1200,"n":%q}`, strings.Repeat("a", 40))
	if rr := postBeacon(t, newTestDoor(publicWebTenant(), store, nil), body, map[string]string{"Origin": "https://example.com"}); rr.Code != http.StatusNoContent || len(store.events) != 1 {
		t.Errorf("a 40-byte name: %d, %d events, want 204 and stored", rr.Code, len(store.events))
	}
}

func TestCollectEventOverTheVisitCapStoresNothingQuietly(t *testing.T) {
	limit := int32(3000)
	store := &fakeWebStore{quota: pgstore.WebQuota{Visits: 3000, MaxVisits: &limit}}
	rr := postBeacon(t, newTestDoor(publicWebTenant(), store, nil), `{"v":2,"t":"event","p":"/","w":1200,"n":"signup"}`,
		map[string]string{"Origin": "https://example.com"})
	if rr.Code != http.StatusNoContent || rr.Body.Len() != 0 || len(store.events) != 0 {
		t.Errorf("%d %q, %d events; want a bare 204 storing nothing", rr.Code, rr.Body.String(), len(store.events))
	}
}

// fm items land in web_heat under their f-kinds, one per kind and selector a
// beacon; a malformed item is skipped, and the 101st is never read.
func TestCollectFormItems(t *testing.T) {
	origin := map[string]string{"Origin": "https://example.com"}
	store := &fakeWebStore{}
	rr := postBeacon(t, newTestDoor(publicWebTenant(), store, nil), `{"v":2,"t":"heat","p":"/signup","w":1200,"fm":[
		["focus","#email"],["focus","#email"],["fill","#email"],["submit","form#signup"],["left","#name"],
		["blur","#email"],["focus",""],["focus"],["focus","#a","x"],[1,"#b"],["fill",2]
	]}`, origin)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	for kind, want := range map[string]string{"ffocus": "#email", "ffill": "#email", "fsubmit": "form#signup", "fleft": "#name"} {
		cells := heatCellsOf(t, store, kind)
		if len(cells) != 1 || cells[0].Selector != want || cells[0].N != 1 || cells[0].FX != 0 || cells[0].FY != 0 {
			t.Errorf("%s = %+v, want %s once at 0,0", kind, cells, want)
		}
	}
	if n := len(store.heatCalls[0].cells); n != 4 {
		t.Errorf("%d cells, want the 4 valid items: %+v", n, store.heatCalls[0].cells)
	}

	var b strings.Builder
	b.WriteString(`{"v":2,"t":"heat","p":"/","w":1200,"fm":[`)
	for i := range maxFormItems + 1 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `["focus","#f%d"]`, i)
	}
	b.WriteString(`]}`)
	store = &fakeWebStore{}
	postBeacon(t, newTestDoor(publicWebTenant(), store, nil), b.String(), origin)
	if got := len(heatCellsOf(t, store, "ffocus")); got != maxFormItems {
		t.Errorf("%d focus cells, want the %d cap", got, maxFormItems)
	}
}

// The read answers the store's form stats under the same filters as the
// heat, and [] when there are none.
func TestHeatmapAnswersForms(t *testing.T) {
	store := fakeReadStore()
	d := newTestDoor(publicWebTenant(), store, nil)
	base := "/w/heatmap?token=uch_good&key=uc_pub_test&path=/&w=1200"
	if rr := getHeatmap(t, d, base, nil); !strings.Contains(rr.Body.String(), `"forms":[]`) {
		t.Errorf("no forms: %s, want forms as [] on the wire", rr.Body.String())
	}
	store.forms = []pgstore.FormStat{{Selector: "#email", Focus: 10, Fill: 8, Left: 2}}
	rr := getHeatmap(t, d, base+"&segment=member&source=search", nil)
	if !strings.Contains(rr.Body.String(), `"forms":[{"selector":"#email","focus":10,"fill":8,"submit":0,"left":2}]`) {
		t.Errorf("body %s, want the store's one form field", rr.Body.String())
	}
	if store.readFilter != [2]string{"member", "search"} {
		t.Errorf("form read narrowed to %v, want member, search", store.readFilter)
	}
}

// Every answer carries the project's deploys and releases inside the web
// depth, worded like the board's marks (a burst folded into one), newest
// first, at most 10; [] when there are none. The door's clock is Saturday
// 2026-09-19.
func TestHeatmapDeploys(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 9, 10, h, m, 0, 0, time.UTC) }
	store := fakeReadStore()
	d := newTestDoor(publicWebTenant(), store, nil)
	base := "/w/heatmap?token=uch_good&key=uc_pub_test&path=/&w=1200"

	rr := getHeatmap(t, d, base, nil)
	body := rr.Body.String()
	if rr.Code != http.StatusOK || !strings.Contains(body, `"deploys":[]`) {
		t.Fatalf("no deploys: %d %s, want deploys as []", rr.Code, body)
	}
	if strings.Contains(body, `"from"`) || strings.Contains(body, `"before"`) || strings.Contains(body, `"to"`) {
		t.Errorf("no split: %s, want no from, to or before", body)
	}
	want := [2]time.Time{time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)}
	if store.deployWindow != want {
		t.Errorf("deploys read over %v, want the 31-day web depth to the end of today", store.deployWindow)
	}

	// Oldest first, as the store reads them: 12 hourly deploys, the last one
	// followed 5 minutes later by a sha-less event of the same burst.
	for i := range 12 {
		store.deploys = append(store.deploys, pgstore.MarkRow{At: at(i, 0), Name: "deploy", SHA: fmt.Sprintf("%02dabcdef0123", i)})
	}
	store.deploys = append(store.deploys, pgstore.MarkRow{At: at(11, 5), Name: "deploy.live"})
	store.releases = []pgstore.MarkRow{{At: at(5, 30), Name: "v1.2.0"}}
	rr = getHeatmap(t, d, base, nil)
	var out heatOut
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Deploys) != 10 {
		t.Fatalf("%d deploys, want the latest 10: %+v", len(out.Deploys), out.Deploys)
	}
	if got := out.Deploys[0]; !got.At.Equal(at(11, 5)) || got.Label != "11abcde" {
		t.Errorf("newest = %+v, want the burst at its last event, keeping its sha", got)
	}
	if got := out.Deploys[6]; !got.At.Equal(at(5, 30)) || got.Label != "v1.2.0" {
		t.Errorf("deploys[6] = %+v, want the release merged in its place", got)
	}
	if got := out.Deploys[9]; !got.At.Equal(at(3, 0)) {
		t.Errorf("oldest kept = %+v, want 03:00", got)
	}

	// Only the first look lists them: a read with a range or a split is the
	// overlay's own, and carries [].
	for _, q := range []string{"&range=7d", "&split=2026-09-18T00:00:00Z"} {
		store.deployWindow = [2]time.Time{}
		if rr := getHeatmap(t, d, base+q, nil); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"deploys":[]`) || !store.deployWindow[0].IsZero() {
			t.Errorf("%s: %d, read %v; want deploys [] and no deploy read: %s", q, rr.Code, store.deployWindow, rr.Body.String())
		}
	}

	// A 7-day plan reads them over its 7 days and offers only those whose
	// before window it can read: 09-16 (n 4, 7 days back) fits, 09-15 (9) not.
	store.histDays = 7
	store.deploys = []pgstore.MarkRow{
		{At: time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC), Name: "deploy", SHA: "aaaaaaaaaa"},
		{At: time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC), Name: "deploy", SHA: "bbbbbbbbbb"},
	}
	store.releases = nil
	rr = getHeatmap(t, d, base, nil)
	out = heatOut{}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if store.deployWindow[0] != time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC) {
		t.Errorf("7-day plan: deploys from %v, want 09-12", store.deployWindow[0])
	}
	if len(out.Deploys) != 1 || out.Deploys[0].Label != "bbbbbbb" {
		t.Errorf("7-day plan offers %+v, want the 09-16 deploy alone", out.Deploys)
	}
	// Past 15 days the before window outruns the 31 days ever kept.
	store.histDays = 0
	store.deploys = []pgstore.MarkRow{{At: time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC), Name: "deploy", SHA: "cccccccccc"}}
	if rr := getHeatmap(t, d, base, nil); !strings.Contains(rr.Body.String(), `"deploys":[]`) {
		t.Errorf("a 16-day-old deploy: %s, want deploys []", rr.Body.String())
	}
}

// With split the main numbers read from the split's UTC day to today, n days,
// and before the n days before it, with the same filters. Past a week each
// heat bound snaps back to its Monday, so a weekly row lands on one side only;
// page views stay day-exact.
func TestHeatmapSplitWindows(t *testing.T) {
	date := func(m, d int) time.Time { return time.Date(2026, time.Month(m), d, 0, 0, 0, 0, time.UTC) }
	var zero time.Time
	for _, tc := range []struct {
		name, split                    string
		from, heatFrom                 time.Time // the after window's; it runs to now
		bFrom, bTo, bHeatFrom, bHeatTo time.Time
	}{
		{"2 days old", "2026-09-17T15:00:00Z", date(9, 17), date(9, 17), date(9, 14), date(9, 17), date(9, 14), date(9, 17)},
		{"today, in another zone", "2026-09-18T22:00:00-05:00", date(9, 19), date(9, 19), date(9, 18), date(9, 19), date(9, 18), date(9, 19)},
		// Wednesday 09-09: n = 11, before from Saturday 08-29.
		{"10 days old", "2026-09-09T10:00:00Z", date(9, 9), date(9, 7), date(8, 29), date(9, 9), date(8, 24), date(9, 7)},
	} {
		store := fakeReadStore()
		d := newTestDoor(publicWebTenant(), store, nil)
		rr := getHeatmap(t, d, "/w/heatmap?token=uch_good&key=uc_pub_test&path=/&w=1200&range=7d&segment=member&source=search&split="+tc.split, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.name, rr.Code, rr.Body.String())
		}
		for _, r := range store.reads {
			if r.segment != "member" || r.source != "search" {
				t.Errorf("%s: %s read narrowed to %q %q, want member, search", tc.name, r.what, r.segment, r.source)
			}
			after := r.to.IsZero()
			var from, to time.Time
			switch {
			case after && r.what == "views":
				from, to = tc.from, zero
			case after:
				from, to = tc.heatFrom, zero
			case r.what == "views":
				from, to = tc.bFrom, tc.bTo
			default:
				from, to = tc.bHeatFrom, tc.bHeatTo
			}
			if !r.from.Equal(from) || !r.to.Equal(to) {
				t.Errorf("%s: %s read over [%v, %v), want [%v, %v)", tc.name, r.what, r.from, r.to, from, to)
			}
		}
		if len(store.reads) != 16 {
			t.Errorf("%s: %d reads, want 8 per window", tc.name, len(store.reads))
		}
		var out struct {
			Range  string `json:"range"`
			From   string `json:"from"`
			Views  int64  `json:"views"`
			Before map[string]json.RawMessage
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Range != "31d" || out.From != tc.from.Format(time.DateOnly) || out.Views != 42 {
			t.Errorf("%s: range %q from %q views %d, want the deepest range, the split's day, the store's views", tc.name, out.Range, out.From, out.Views)
		}
		if string(out.Before["from"]) != `"`+tc.bFrom.Format(time.DateOnly)+`"` || string(out.Before["to"]) != `"`+tc.bTo.Format(time.DateOnly)+`"` {
			t.Errorf("%s: before from %s to %s", tc.name, out.Before["from"], out.Before["to"])
		}
		for _, k := range []string{"views", "clicks", "rage", "dead", "hits", "scroll", "forms"} {
			if _, ok := out.Before[k]; !ok {
				t.Errorf("%s: before has no %s", tc.name, k)
			}
		}
		if _, ok := out.Before["moves"]; ok {
			t.Errorf("%s: before carries moves, which HeatmapWindow has not", tc.name)
		}
	}
}

// A split is refused when it is not a time, lies in a future day, or its
// before window reaches past what the plan can read: n days from the split
// through today read 2n - 1 days back, inside the 31 days ever kept and the
// plan's history. Such a split could not have been offered, so it is a 400,
// never the 402 a range would be.
func TestHeatmapSplitRefusals(t *testing.T) {
	for _, tc := range []struct {
		split    string
		histDays int
		want     int
	}{
		{"yesterday", 0, http.StatusBadRequest},
		{"2026-09-20T00:00:00Z", 0, http.StatusBadRequest},
		{"2026-09-19T23:00:00Z", 0, http.StatusOK},         // later today: the split's day is today
		{"2026-09-04T12:00:00Z", 0, http.StatusOK},         // n 16: 31 days back
		{"2026-09-03T12:00:00Z", 0, http.StatusBadRequest}, // n 17: 33 days, past the 31 kept
		{"2026-08-19T12:00:00Z", 0, http.StatusBadRequest},
		{"2026-09-16T12:00:00Z", 7, http.StatusOK},         // before from 09-12: 7 days
		{"2026-09-15T12:00:00Z", 7, http.StatusBadRequest}, // 9 days
		{"2026-09-12T12:00:00Z", 7, http.StatusBadRequest},
		{"2026-09-19T12:00:00Z", 1, http.StatusOK},
		{"2026-09-18T12:00:00Z", 1, http.StatusBadRequest}, // before from 09-16: 3 days
	} {
		store := fakeReadStore()
		store.histDays = tc.histDays
		d := newTestDoor(publicWebTenant(), store, nil)
		rr := getHeatmap(t, d, "/w/heatmap?token=uch_good&key=uc_pub_test&path=/&w=1200&split="+tc.split, nil)
		if rr.Code != tc.want {
			t.Errorf("split %s on a %d-day plan: %d %s, want %d", tc.split, tc.histDays, rr.Code, rr.Body.String(), tc.want)
		}
	}
}
