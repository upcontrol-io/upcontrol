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

func TestCompactWeb(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	today := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC) // a Saturday
	date := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	heat := func(tenant, project int64, day time.Time, path string, cells ...HeatAdd) {
		t.Helper()
		if err := s.AddHeat(ctx, tenant, project, day, path, "desktop", cells); err != nil {
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
		cells, err := s.HeatCells(ctx, agency, site, "/", "desktop", "click", c.from, 10)
		if err != nil || len(cells) != 1 || cells[0].N != c.want {
			t.Fatalf("HeatCells from %s = %+v (%v), want one cell of %d", c.from.Format(time.DateOnly), cells, err, c.want)
		}
	}
}
