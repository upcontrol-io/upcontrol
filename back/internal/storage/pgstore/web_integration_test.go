//go:build integration

// Integration test for the web door's bounds against a real Postgres: the
// month's visit count and the daily compaction that caps web_heat.
// Run: go test -tags=integration ./internal/storage/pgstore/...
package pgstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedWebTenant is one tenant on plan with one project.
func seedWebTenant(t *testing.T, pool *pgxpool.Pool, plan string) (tenantID, projectID int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, plan) VALUES (gen_random_uuid(), $1, $1) RETURNING id`,
		plan).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant on %s: %v", plan, err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		tenantID, fmt.Sprintf("web-%d.example.com", tenantID)).Scan(&projectID); err != nil {
		t.Fatalf("seed project on %s: %v", plan, err)
	}
	return tenantID, projectID
}

// A visit is one visitor on one UTC day in a project, counted into the
// workspace's month.
func TestInsertPageviewCountsVisits(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	tenant, project := seedWebTenant(t, pool, "Free")
	day := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for _, v := range []struct {
		ts    time.Time
		actor string
	}{
		{day, "a"},
		{day.Add(time.Hour), "a"},     // the same visitor again: no visit
		{day.Add(2 * time.Hour), "b"}, // another visitor
		{day.AddDate(0, 0, 1), "a"},   // a new day is a new visit
		{time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "a"},
	} {
		if err := s.InsertPageview(ctx, tenant, project, v.ts, map[string]string{"path": "/"}, v.actor); err != nil {
			t.Fatalf("page view: %v", err)
		}
	}
	q, err := s.WebQuota(ctx, tenant, day)
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if q.Visits != 3 || q.MaxVisits == nil || *q.MaxVisits != 3000 || q.HeatPages == nil || *q.HeatPages != 5 {
		t.Fatalf("September = %d visits of %v, %v pages; want 3 of Free's 3000 and 5", q.Visits, q.MaxVisits, q.HeatPages)
	}
	if q, _ = s.WebQuota(ctx, tenant, time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)); q.Visits != 1 {
		t.Fatalf("October = %d visits, want the month's own 1", q.Visits)
	}
	var views int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE tenant_id = $1`, tenant).Scan(&views); err != nil || views != 5 {
		t.Fatalf("%d page views stored (%v), want every one of the 5", views, err)
	}
	// One visitor's views in a day stop at maxVisitViews: one visit, however much it sends.
	next := day.AddDate(0, 0, 2)
	for range maxVisitViews + 2 {
		if err := s.InsertPageview(ctx, tenant, project, next, map[string]string{"path": "/"}, "c"); err != nil {
			t.Fatalf("page view: %v", err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE tenant_id = $1 AND actor = 'c'`, tenant).Scan(&views); err != nil || views != maxVisitViews {
		t.Fatalf("%d page views stored for one visitor's day (%v), want %d", views, err, maxVisitViews)
	}
	if q, _ = s.WebQuota(ctx, tenant, next); q.Visits != 4 {
		t.Fatalf("September = %d visits, want 4: one more for the one visitor's day", q.Visits)
	}
}

// One visitor's named events in a day stop at maxVisitEvents; page views do
// not count toward it, and an event counts no visit.
func TestInsertWebEventCapsOneVisitorsDay(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	tenant, project := seedWebTenant(t, pool, "Agency")
	at := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for range 5 {
		if err := s.InsertPageview(ctx, tenant, project, at, map[string]string{"path": "/"}, "w:v"); err != nil {
			t.Fatalf("page view: %v", err)
		}
	}
	for range maxVisitEvents + 1 {
		if err := s.InsertWebEvent(ctx, tenant, project, at.Add(time.Minute), "signup", map[string]string{"path": "/"}, "w:v"); err != nil {
			t.Fatalf("event: %v", err)
		}
	}
	var events, views int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE name = 'signup'), count(*) FILTER (WHERE name = 'uc.pageview')
		FROM events WHERE tenant_id = $1 AND actor = 'w:v'`, tenant).Scan(&events, &views); err != nil {
		t.Fatalf("count: %v", err)
	}
	if events != maxVisitEvents || views != 5 {
		t.Fatalf("%d events and %d page views stored, want %d and 5", events, views, maxVisitEvents)
	}
	// The next UTC day starts a fresh budget.
	if err := s.InsertWebEvent(ctx, tenant, project, at.AddDate(0, 0, 1), "signup", map[string]string{"path": "/"}, "w:v"); err != nil {
		t.Fatalf("event: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE tenant_id = $1 AND name = 'signup'`, tenant).Scan(&events); err != nil || events != maxVisitEvents+1 {
		t.Fatalf("%d events after a new day (%v), want %d", events, err, maxVisitEvents+1)
	}
	if q, _ := s.WebQuota(ctx, tenant, at); q.Visits != 1 {
		t.Fatalf("%d visits, want the page views' 1: an event counts no visit", q.Visits)
	}
}

func TestCompactWeb(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	today := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC) // a Saturday
	date := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	heat := func(tenant, project int64, day time.Time, path string, cells ...HeatAdd) {
		t.Helper()
		if err := s.AddHeat(ctx, tenant, project, day, path, "desktop", "", "", cells); err != nil {
			t.Fatalf("seed heat: %v", err)
		}
	}
	count := func(tenant int64, where string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM web_heat WHERE tenant_id = $1 AND `+where,
			append([]any{tenant}, args...)...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	a := HeatAdd{Kind: "click", Selector: "a", FX: 1, FY: 1}
	with := func(c HeatAdd, n int64) HeatAdd { c.N = n; return c }

	// a. Buckets: the week of Monday 09-07 folds onto that Monday; 09-13 is
	// inside the last week and stays a day.
	agency, site := seedWebTenant(t, pool, "Agency")
	heat(agency, site, date(7), "/", with(a, 4))
	heat(agency, site, date(8), "/", with(a, 1))
	heat(agency, site, date(10), "/", with(a, 2))
	heat(agency, site, date(13), "/", with(a, 5))

	// b. Caps on a completed day: 301 click and 601 move cells, n rising with
	// the index, and 21 scroll rows; today's 301 clicks are not a completed day.
	var clicks, moves, scroll, fresh []HeatAdd
	for i := range 601 {
		if i < 301 {
			clicks = append(clicks, HeatAdd{Kind: "click", Selector: fmt.Sprintf("c%d", i), N: int64(i + 1)})
			fresh = append(fresh, HeatAdd{Kind: "click", Selector: fmt.Sprintf("c%d", i), N: 1})
		}
		if i < 21 {
			scroll = append(scroll, HeatAdd{Kind: "scroll", FY: int16(i), N: 1})
		}
		moves = append(moves, HeatAdd{Kind: "move", Selector: fmt.Sprintf("m%d", i), N: int64(i + 1)})
	}
	heat(agency, site, date(18), "/capped", append(append(clicks, moves...), scroll...)...)
	heat(agency, site, today, "/capped", fresh...)

	// c. Free keeps its 5 busiest paths by yesterday's views (scroll rows):
	// /p1../p4 and /new, busier yesterday than /p5 however much /p5 kept from
	// before. /p6 has fewer views and /noscroll has none.
	free, freeSite := seedWebTenant(t, pool, "Free")
	for i := 1; i <= 6; i++ {
		heat(free, freeSite, date(18), fmt.Sprintf("/p%d", i), HeatAdd{Kind: "scroll", FY: 20, N: int64(100 - 10*i)}, with(a, 1))
	}
	heat(free, freeSite, date(17), "/p5", HeatAdd{Kind: "scroll", FY: 20, N: 1000})
	heat(free, freeSite, date(18), "/new", HeatAdd{Kind: "scroll", FY: 20, N: 55})
	heat(free, freeSite, date(18), "/noscroll", with(a, 100))
	self, selfSite := seedWebTenant(t, pool, "Self-hosted")
	for i := 1; i <= 7; i++ {
		heat(self, selfSite, date(18), fmt.Sprintf("/p%d", i), with(a, 1))
	}

	// d. web_usage keeps this month and the last.
	for _, m := range []int{7, 8, 9} {
		if _, err := pool.Exec(ctx, `INSERT INTO web_usage (tenant_id, month, visits) VALUES ($1, $2, 1)`,
			agency, time.Date(2026, time.Month(m), 1, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}

	// Twice: every step is idempotent, so the second run changes nothing.
	for run := 1; run <= 2; run++ {
		if err := s.CompactWeb(ctx, today); err != nil {
			t.Fatalf("run %d: compact: %v", run, err)
		}
		var n int64
		if err := pool.QueryRow(ctx, `SELECT n FROM web_heat WHERE tenant_id = $1 AND path = '/' AND day = $2`,
			agency, date(7)).Scan(&n); err != nil || n != 7 {
			t.Fatalf("run %d: Monday 09-07's bucket = %d (%v), want the week's 4+1+2", run, n, err)
		}
		if got := count(agency, `path = '/'`); got != 2 {
			t.Fatalf("run %d: %d rows on /, want the bucket and 09-13", run, got)
		}
		for _, c := range []struct {
			kind string
			day  time.Time
			want int64
		}{{"click", date(18), 300}, {"move", date(18), 600}, {"scroll", date(18), 21}, {"click", today, 301}} {
			if got := count(agency, `path = '/capped' AND kind = $2 AND day = $3`, c.kind, c.day); got != c.want {
				t.Fatalf("run %d: %d %s cells on %s, want %d", run, got, c.kind, c.day.Format(time.DateOnly), c.want)
			}
		}
		if got := count(agency, `path = '/capped' AND kind = 'click' AND day = $2 AND n = 1`, date(18)); got != 0 {
			t.Fatalf("run %d: the coldest click cell survived the cap", run)
		}
		for path, want := range map[string]int64{"/p1": 2, "/p4": 2, "/new": 1, "/p5": 0, "/p6": 0, "/noscroll": 0} {
			if got := count(free, `path = $2`, path); got != want {
				t.Fatalf("run %d: Free's %s has %d rows, want %d", run, path, got, want)
			}
		}
		if got := count(self, `true`); got != 7 {
			t.Fatalf("run %d: Self-hosted kept %d of its 7 pages", run, got)
		}
		var kept string
		if err := pool.QueryRow(ctx, `SELECT string_agg(month::text, ',' ORDER BY month) FROM web_usage WHERE tenant_id = $1`,
			agency).Scan(&kept); err != nil || kept != "2026-08-01,2026-09-01" {
			t.Fatalf("run %d: months kept = %q (%v), want August and September", run, kept, err)
		}
	}

	// The read from the bucket's Monday counts the week whole; from its
	// Tuesday it would miss it, which is why a long range snaps to Monday.
	for _, c := range []struct {
		from time.Time
		want int64
	}{{date(7), 12}, {date(8), 5}} {
		cells, err := s.HeatCells(ctx, agency, site, "/", "desktop", "", "", "click", c.from, 10)
		if err != nil || len(cells) != 1 || cells[0].N != c.want {
			t.Fatalf("HeatCells from %s = %+v (%v), want one cell of %d", c.from.Format(time.DateOnly), cells, err, c.want)
		}
	}
}

// A stored page view adds one to its hour's web_engage views, in the same
// statement; one past maxVisitViews is not stored and adds nothing. AddEngage
// sums into the same row.
func TestWebEngageCountsStoredViewsAndSums(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	tenant, project := seedWebTenant(t, pool, "Agency")
	at := time.Date(2026, 9, 18, 10, 20, 0, 0, time.UTC)
	hour := at.Truncate(time.Hour)
	labels := map[string]string{"path": "/docs", "device": "mobile"}
	for range maxVisitViews + 3 {
		if err := s.InsertPageview(ctx, tenant, project, at, labels, "v"); err != nil {
			t.Fatalf("page view: %v", err)
		}
	}
	for _, add := range []EngageAdd{{MS: 4000, R10: 1}, {MS: 26000, R10: 0, R30: 1}} {
		if err := s.AddEngage(ctx, tenant, project, hour, "/docs", "mobile", "", "", add); err != nil {
			t.Fatalf("engage: %v", err)
		}
	}
	var views, ms, r10, r30, r120 int64
	if err := pool.QueryRow(ctx, `SELECT views, ms, r10, r30, r120 FROM web_engage
		WHERE tenant_id = $1 AND project_id = $2 AND hour = $3 AND path = '/docs' AND device = 'mobile'`,
		tenant, project, hour).Scan(&views, &ms, &r10, &r30, &r120); err != nil {
		t.Fatalf("read engage row: %v", err)
	}
	if views != maxVisitViews || ms != 30000 || r10 != 1 || r30 != 1 || r120 != 0 {
		t.Fatalf("row = views %d ms %d r10 %d r30 %d r120 %d, want %d stored views, 30000 ms, one r10, one r30",
			views, ms, r10, r30, r120, maxVisitViews)
	}
}

// The web series reads: buckets of sums from web_engage narrowed by path and
// device, the oldest hour, engaged pages ranked, and a heat kind's pages
// ranked from the day asked.
func TestWebSeriesReads(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	tenant, project := seedWebTenant(t, pool, "Agency")
	from := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	view := func(at time.Time, path, device string) {
		t.Helper()
		if err := s.InsertPageview(ctx, tenant, project, at, map[string]string{"path": path, "device": device}, fmt.Sprintf("v%d", at.UnixNano())); err != nil {
			t.Fatalf("page view: %v", err)
		}
	}
	engage := func(at time.Time, path, device string, add EngageAdd) {
		t.Helper()
		if err := s.AddEngage(ctx, tenant, project, at.Truncate(time.Hour), path, device, "", "", add); err != nil {
			t.Fatalf("engage: %v", err)
		}
	}
	// Hour 2: two views of / on desktop, 30 s engaged between them, one past
	// 10 s. Hour 5: one view of /docs on mobile, 90 s, past 10 and 30 s.
	h2, h5 := from.Add(2*time.Hour+time.Minute), from.Add(5*time.Hour+time.Minute)
	view(h2, "/", "desktop")
	view(h2, "/", "desktop")
	engage(h2, "/", "desktop", EngageAdd{MS: 30000, R10: 1})
	view(h5, "/docs", "mobile")
	engage(h5, "/docs", "mobile", EngageAdd{MS: 90000, R10: 1, R30: 1})

	all, err := s.EngageBuckets(ctx, tenant, project, "", "", "", "", from, from.Add(6*time.Hour), 3600)
	if err != nil || len(all) != 2 {
		t.Fatalf("buckets = %+v (%v), want hours 2 and 5", all, err)
	}
	if b := all[0]; b.Index != 2 || b.Views != 2 || b.MS != 30000 || b.R != [4]int64{1, 0, 0, 0} {
		t.Errorf("hour 2 = %+v", b)
	}
	if b := all[1]; b.Index != 5 || b.Views != 1 || b.MS != 90000 || b.R != [4]int64{1, 1, 0, 0} {
		t.Errorf("hour 5 = %+v", b)
	}
	if mobile, err := s.EngageBuckets(ctx, tenant, project, "", "mobile", "", "", from, from.Add(6*time.Hour), 3600); err != nil || len(mobile) != 1 || mobile[0].Index != 5 {
		t.Errorf("mobile buckets = %+v (%v), want hour 5 alone", mobile, err)
	}
	if one, err := s.EngageBuckets(ctx, tenant, project, "/", "", "", "", from, from.Add(6*time.Hour), 6*3600); err != nil || len(one) != 1 || one[0].Views != 2 {
		t.Errorf("/ in one bucket = %+v (%v), want its 2 views", one, err)
	}

	paths, err := s.EngagePaths(ctx, tenant, project, "", "", "", from, from.Add(6*time.Hour))
	if err != nil || len(paths) != 2 || paths[0].Labels["path"] != "/docs" || paths[0].Sum != 90 || paths[1].Sum != 15 {
		t.Fatalf("engaged pages = %+v (%v), want /docs at 90 s then / at 15 s", paths, err)
	}

	for _, c := range []struct {
		day  time.Time
		path string
		n    int64
	}{{from, "/", 2}, {from, "/docs", 5}, {from.AddDate(0, 0, -1), "/", 10}} {
		if err := s.AddHeat(ctx, tenant, project, c.day, c.path, "desktop", "", "", []HeatAdd{{Kind: "rage", Selector: "a", N: c.n}, {Kind: "click", Selector: "a", N: 100}}); err != nil {
			t.Fatalf("heat: %v", err)
		}
	}
	rage, err := s.HeatPaths(ctx, tenant, project, "rage", "", "", "", from)
	if err != nil || len(rage) != 2 || rage[0].Labels["path"] != "/docs" || rage[0].Sum != 5 || rage[1].Sum != 2 {
		t.Fatalf("rage pages from the day = %+v (%v), want /docs 5 then / 2", rage, err)
	}
	if rage, err = s.HeatPaths(ctx, tenant, project, "rage", "", "", "", from.AddDate(0, 0, -1)); err != nil || rage[0].Labels["path"] != "/" || rage[0].Sum != 12 {
		t.Fatalf("rage pages from the day before = %+v (%v), want / at 12 first", rage, err)
	}
	if mobile, err := s.HeatPaths(ctx, tenant, project, "rage", "mobile", "", "", from); err != nil || len(mobile) != 0 {
		t.Fatalf("mobile rage pages = %+v (%v), want none", mobile, err)
	}
}

// history-trim drops web_engage rows past the web depth, like the page views
// they count.
func TestTrimHistoryDropsOldWebEngage(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	tenant, project := seedWebTenant(t, pool, "Agency") // 365 days, 31 on the web
	now := time.Now().UTC().Truncate(time.Hour)
	for _, hour := range []time.Time{now.AddDate(0, 0, -40), now.Add(-time.Hour)} {
		if err := s.AddEngage(ctx, tenant, project, hour, "/", "desktop", "", "", EngageAdd{MS: 1000}); err != nil {
			t.Fatalf("engage: %v", err)
		}
	}
	if _, err := s.TrimHistory(ctx); err != nil {
		t.Fatalf("trim history: %v", err)
	}
	var left int64
	var oldest time.Time
	if err := pool.QueryRow(ctx, `SELECT count(*), min(hour) FROM web_engage WHERE tenant_id = $1`, tenant).Scan(&left, &oldest); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 1 || !oldest.Equal(now.Add(-time.Hour)) {
		t.Fatalf("%d rows left from %v, want the fresh one alone", left, oldest)
	}
}

// A project keeps its first four segments: a fifth is refused, an admitted
// one answers yes again, and the list reads oldest first.
func TestAdmitSegment(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	_, project := seedWebTenant(t, pool, "Agency")
	_, other := seedWebTenant(t, pool, "Agency")
	for _, name := range []string{"zeta", "alpha", "mid", "beta"} {
		if ok, err := s.AdmitSegment(ctx, project, name); err != nil || !ok {
			t.Fatalf("admit %s = %v (%v), want admitted", name, ok, err)
		}
	}
	if ok, err := s.AdmitSegment(ctx, project, "fifth"); err != nil || ok {
		t.Fatalf("a fifth name = %v (%v), want refused", ok, err)
	}
	if ok, err := s.AdmitSegment(ctx, project, "alpha"); err != nil || !ok {
		t.Fatalf("an admitted name again = %v (%v), want admitted", ok, err)
	}
	if ok, err := s.AdmitSegment(ctx, other, "fifth"); err != nil || !ok {
		t.Fatalf("another project's first name = %v (%v), want admitted: the cap is per project", ok, err)
	}
	got, err := s.Segments(ctx, project)
	if err != nil || fmt.Sprint(got) != "[zeta alpha mid beta]" {
		t.Fatalf("segments = %v (%v), want the four oldest first", got, err)
	}
	if none, err := s.Segments(ctx, 0); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("no segments = %#v (%v), want []", none, err)
	}
}

// The heatmap reads narrow by segment and source when asked, and sum across
// every one of them when not.
func TestHeatReadsBySegment(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	tenant, project := seedWebTenant(t, pool, "Agency")
	day := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	for i, v := range []struct{ segment, source string }{{"member", "search"}, {"member", ""}, {"", "search"}} {
		labels := map[string]string{"path": "/", "device": "desktop"}
		if v.segment != "" {
			labels["segment"] = v.segment
		}
		if v.source != "" {
			labels["source"] = v.source
		}
		if err := s.InsertPageview(ctx, tenant, project, day.Add(time.Hour), labels, fmt.Sprintf("v%d", i)); err != nil {
			t.Fatalf("page view: %v", err)
		}
		if err := s.AddHeat(ctx, tenant, project, day, "/", "desktop", v.segment, v.source, []HeatAdd{
			{Kind: "click", Selector: "a", FX: 1, FY: 1, N: int64(i + 1)},
			{Kind: "scroll", FY: 20, N: 1},
		}); err != nil {
			t.Fatalf("heat: %v", err)
		}
	}
	for _, c := range []struct {
		segment, source   string
		views, clicks, sc int64
	}{{"", "", 3, 6, 3}, {"member", "", 2, 3, 2}, {"", "search", 2, 4, 2}, {"member", "search", 1, 1, 1}, {"anon", "", 0, 0, 0}} {
		views, err := s.PageviewCount(ctx, tenant, project, "/", "desktop", c.segment, c.source, day)
		if err != nil || views != c.views {
			t.Errorf("(%q, %q) views = %d (%v), want %d", c.segment, c.source, views, err, c.views)
		}
		cells, err := s.HeatCells(ctx, tenant, project, "/", "desktop", c.segment, c.source, "click", day, 10)
		var clicks int64
		for _, cell := range cells {
			clicks += cell.N
		}
		if err != nil || clicks != c.clicks || len(cells) > 1 {
			t.Errorf("(%q, %q) clicks = %+v (%v), want one cell of %d", c.segment, c.source, cells, err, c.clicks)
		}
		reach, err := s.ScrollReach(ctx, tenant, project, "/", "desktop", c.segment, c.source, day)
		if err != nil || reach[0] != c.sc {
			t.Errorf("(%q, %q) scroll reach = %d (%v), want %d", c.segment, c.source, reach[0], err, c.sc)
		}
	}
}

// The weekly fold keeps segments apart, and a completed day's cell cap counts
// every segment together: two segments cost no more rows than one.
func TestCompactWebKeepsSegmentsApartAndCapsAcross(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	tenant, project := seedWebTenant(t, pool, "Agency")
	today := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	date := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	heat := func(day time.Time, path, segment string, cells ...HeatAdd) {
		t.Helper()
		if err := s.AddHeat(ctx, tenant, project, day, path, "desktop", segment, "", cells); err != nil {
			t.Fatalf("seed heat: %v", err)
		}
	}
	with := func(n int64) HeatAdd { return HeatAdd{Kind: "click", Selector: "a", FX: 1, FY: 1, N: n} }
	heat(date(7), "/", "member", with(4))
	heat(date(8), "/", "anon", with(1))
	heat(date(10), "/", "member", with(2))
	// 2 x MaxClickCells cells, interleaved by n across the two segments.
	var member, anon []HeatAdd
	for i := range MaxClickCells {
		member = append(member, HeatAdd{Kind: "click", Selector: fmt.Sprintf("m%d", i), N: int64(2*i + 2)})
		anon = append(anon, HeatAdd{Kind: "click", Selector: fmt.Sprintf("a%d", i), N: int64(2*i + 1)})
	}
	heat(date(18), "/capped", "member", member...)
	heat(date(18), "/capped", "anon", anon...)

	for run := 1; run <= 2; run++ {
		if err := s.CompactWeb(ctx, today); err != nil {
			t.Fatalf("run %d: compact: %v", run, err)
		}
		var buckets string
		if err := pool.QueryRow(ctx, `SELECT string_agg(segment || '=' || n, ',' ORDER BY segment) FROM web_heat
			WHERE tenant_id = $1 AND path = '/' AND day = $2`, tenant, date(7)).Scan(&buckets); err != nil || buckets != "anon=1,member=6" {
			t.Fatalf("run %d: Monday's buckets = %q (%v), want anon 1 and member 4+2 apart", run, buckets, err)
		}
		var kept, coldest int64
		if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(min(n), 0) FROM web_heat WHERE tenant_id = $1 AND path = '/capped' AND day = $2`,
			tenant, date(18)).Scan(&kept, &coldest); err != nil {
			t.Fatalf("count: %v", err)
		}
		if kept != MaxClickCells || coldest != MaxClickCells+1 {
			t.Fatalf("run %d: %d cells left, coldest %d; want the %d hottest across both segments, from %d", run, kept, coldest, MaxClickCells, MaxClickCells+1)
		}
	}
}

// A page view's engage row and the heat beacon's engaged time land on the same
// row per segment and source, and the reads narrow by them.
func TestWebEngageSplitsBySegmentAndSource(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	tenant, project := seedWebTenant(t, pool, "Agency")
	at := time.Date(2026, 9, 18, 10, 20, 0, 0, time.UTC)
	hour := at.Truncate(time.Hour)
	for i, v := range []struct{ segment, source string }{{"member", "search"}, {"member", "search"}, {"", ""}} {
		labels := map[string]string{"path": "/", "device": "desktop"}
		if v.segment != "" {
			labels["segment"], labels["source"] = v.segment, v.source
		}
		if err := s.InsertPageview(ctx, tenant, project, at, labels, fmt.Sprintf("v%d", i)); err != nil {
			t.Fatalf("page view: %v", err)
		}
		if err := s.AddEngage(ctx, tenant, project, hour, "/", "desktop", v.segment, v.source, EngageAdd{MS: 10000, R10: 1}); err != nil {
			t.Fatalf("engage: %v", err)
		}
	}
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM web_engage WHERE tenant_id = $1`, tenant).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("%d engage rows (%v), want one per segment and source", rows, err)
	}
	for _, c := range []struct {
		segment, source string
		views, ms       int64
	}{{"", "", 3, 30000}, {"member", "", 2, 20000}, {"", "search", 2, 20000}, {"anon", "", 0, 0}} {
		b, err := s.EngageBuckets(ctx, tenant, project, "", "", c.segment, c.source, hour, hour.Add(time.Hour), 3600)
		var views, ms int64
		for _, x := range b {
			views, ms = views+x.Views, ms+x.MS
		}
		if err != nil || views != c.views || ms != c.ms {
			t.Errorf("(%q, %q) = %d views %d ms (%v), want %d and %d", c.segment, c.source, views, ms, err, c.views, c.ms)
		}
	}
	if paths, err := s.EngagePaths(ctx, tenant, project, "", "member", "search", hour, hour.Add(time.Hour)); err != nil || len(paths) != 1 || paths[0].Sum != 10 {
		t.Errorf("member engaged pages = %+v (%v), want / at 10 s", paths, err)
	}
	if err := s.AddHeat(ctx, tenant, project, hour.Truncate(24*time.Hour), "/", "desktop", "member", "", []HeatAdd{{Kind: "rage", Selector: "a", N: 3}}); err != nil {
		t.Fatalf("heat: %v", err)
	}
	for seg, want := range map[string]int{"member": 1, "anon": 0, "": 1} {
		if rage, err := s.HeatPaths(ctx, tenant, project, "rage", "", seg, "", hour.Truncate(24*time.Hour)); err != nil || len(rage) != want {
			t.Errorf("%q rage pages = %+v (%v), want %d", seg, rage, err, want)
		}
	}
}
