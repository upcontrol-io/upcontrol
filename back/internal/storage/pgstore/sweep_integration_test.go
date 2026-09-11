//go:build integration

// The plan-capacity sweepers against a real Postgres, through the package's
// container harness.

package pgstore

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedSweepTenant: a tenant on plan with projects holding the given domains,
// all created a month ago so a project's activity, not its birth, decides.
func seedSweepTenant(t *testing.T, pool *pgxpool.Pool, plan string, domains ...string) int64 {
	t.Helper()
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	var ownerID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Sweep') RETURNING id`,
		fmt.Sprintf("sweep-%d@example.com", uniq)).Scan(&ownerID); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	var tenantID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, plan, owner_person_id) VALUES (gen_random_uuid(), $1, $2, $3) RETURNING id`,
		fmt.Sprintf("sweep-%d", uniq), plan, ownerID).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	for _, d := range domains {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project (public_id, tenant_id, domain, created_at)
			 VALUES (gen_random_uuid(), $1, $2, now() - interval '30 days')`,
			tenantID, d); err != nil {
			t.Fatalf("seed project %s: %v", d, err)
		}
	}
	return tenantID
}

func projectID(t *testing.T, pool *pgxpool.Pool, tenantID int64, domain string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM project WHERE tenant_id = $1 AND domain = $2`, tenantID, domain).Scan(&id); err != nil {
		t.Fatalf("read project %s: %v", domain, err)
	}
	return id
}

// touchProject writes one event row: an activity signal.
func touchProject(t *testing.T, pool *pgxpool.Pool, tenantID int64, domain string, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO events (tenant_id, project_id, ts, name, labels)
		 SELECT $1, p.id, $3, 'sweep_touch', '{}'::jsonb FROM project p
		  WHERE p.tenant_id = $1 AND p.domain = $2`,
		tenantID, domain, at)
	if err != nil {
		t.Fatalf("touch %s: %v", domain, err)
	}
}

// seedMonitor subscribes a project to its own new probe target; createdAgo
// orders the budget's oldest-first ranking.
func seedMonitor(t *testing.T, pool *pgxpool.Pool, tenantID, projectID int64, kind, name string, createdAgo time.Duration) {
	t.Helper()
	targetKind := "website"
	if kind == "heartbeat" {
		targetKind = "heartbeat"
	}
	key := fmt.Sprintf("%s-%d-%d", name, projectID, time.Now().UnixNano())
	if _, err := pool.Exec(context.Background(),
		`WITH tgt AS (INSERT INTO probe_target (key, kind, url) VALUES ($1, $2, $1) RETURNING id)
		 INSERT INTO monitor (public_id, tenant_id, project_id, target_id, kind, name, target, interval_sec, created_at)
		 SELECT gen_random_uuid(), $3, $4, tgt.id, $5, $6, $1, 60, now() - make_interval(secs => $7) FROM tgt`,
		key, targetKind, tenantID, projectID, kind, name, createdAgo.Seconds()); err != nil {
		t.Fatalf("seed monitor %s: %v", name, err)
	}
}

// frozenDomains: the tenant's frozen project domains, in id order.
func frozenDomains(t *testing.T, pool *pgxpool.Pool, tenantID int64) []string {
	t.Helper()
	rows, _ := pool.Query(context.Background(),
		`SELECT domain FROM project WHERE tenant_id = $1 AND frozen_at IS NOT NULL ORDER BY id`, tenantID)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read frozen: %v", err)
	}
	return out
}

func sweepFreeze(t *testing.T, s *Store) []int64 {
	t.Helper()
	thawed, err := s.FreezeSweep(context.Background())
	if err != nil {
		t.Fatalf("FreezeSweep: %v", err)
	}
	return thawed
}

// A Free tenant with three projects keeps the most active one live and
// freezes the other two; the live project's deletion promotes the next most
// active snapshot, not the oldest; the plan lifting the cap thaws them all.
func TestFreezeSweepKeepsMostActiveProject(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	uniq := time.Now().UnixNano()
	old := fmt.Sprintf("old-%d.example.com", uniq)
	pet := fmt.Sprintf("pet-%d.example.com", uniq)
	main := fmt.Sprintf("main-%d.example.com", uniq)
	tenant := seedSweepTenant(t, pool, "Free", old, pet, main)
	touchProject(t, pool, tenant, old, now.Add(-72*time.Hour))
	touchProject(t, pool, tenant, main, now.Add(-1*time.Hour))

	// The database is shared across tests, so what counts is which of this
	// tenant's projects a sweep reports thawed.
	oldID, petID := projectID(t, pool, tenant, old), projectID(t, pool, tenant, pet)
	thawedOf := func(thawed []int64) []int64 {
		return slices.DeleteFunc(thawed, func(id int64) bool { return id != oldID && id != petID })
	}

	if thawed := thawedOf(sweepFreeze(t, s)); len(thawed) != 0 {
		t.Fatalf("a freeze reported %v as thawed", thawed)
	}
	if got := frozenDomains(t, pool, tenant); !slices.Equal(got, []string{old, pet}) {
		t.Fatalf("frozen = %v, want [old pet] with main live", got)
	}

	if _, err := pool.Exec(ctx,
		`DELETE FROM project WHERE tenant_id = $1 AND domain = $2`, tenant, main); err != nil {
		t.Fatalf("delete live project: %v", err)
	}
	if thawed := thawedOf(sweepFreeze(t, s)); !slices.Equal(thawed, []int64{oldID}) {
		t.Fatalf("thawed = %v, want [%d] (old)", thawed, oldID)
	}
	if got := frozenDomains(t, pool, tenant); !slices.Equal(got, []string{pet}) {
		t.Fatalf("frozen after promote = %v, want [pet] with old (active 3 days ago) live", got)
	}

	if _, err := pool.Exec(ctx, `UPDATE tenant SET plan = 'Growth' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("upgrade tenant: %v", err)
	}
	if thawed := thawedOf(sweepFreeze(t, s)); !slices.Equal(thawed, []int64{petID}) {
		t.Fatalf("thawed after upgrade = %v, want [%d] (pet)", thawed, petID)
	}
	if got := frozenDomains(t, pool, tenant); len(got) != 0 {
		t.Fatalf("frozen after upgrade = %v, want none", got)
	}
}

// The pick is made once. The project with more running checks beats a
// newer, busier one when the limit first bites, and after that the incumbent
// stays live however the frozen snapshot keeps ingesting: the live slot never
// changes hands on a tick.
func TestFreezeSweepKeepsTheIncumbent(t *testing.T) {
	s, pool := openStore(t)
	now := time.Now().UTC()
	uniq := time.Now().UnixNano()
	checked := fmt.Sprintf("checked-%d.example.com", uniq)
	busy := fmt.Sprintf("busy-%d.example.com", uniq)
	tenant := seedSweepTenant(t, pool, "Free", checked, busy)
	checkedID, busyID := projectID(t, pool, tenant, checked), projectID(t, pool, tenant, busy)
	seedMonitor(t, pool, tenant, checkedID, "website", "site", time.Hour)
	seedMonitor(t, pool, tenant, checkedID, "website", "shop", time.Hour)
	seedMonitor(t, pool, tenant, busyID, "website", "blog", time.Hour)
	touchProject(t, pool, tenant, busy, now.Add(-time.Minute))

	sweepFreeze(t, s)
	if got := frozenDomains(t, pool, tenant); !slices.Equal(got, []string{busy}) {
		t.Fatalf("first pick froze %v, want [busy]: the more monitored project stays live", got)
	}
	// From here the snapshot holds more checks and is the busier of the two:
	// every key after incumbency now favours it.
	seedMonitor(t, pool, tenant, busyID, "website", "api", time.Hour)
	seedMonitor(t, pool, tenant, busyID, "website", "docs", time.Hour)
	for i := range 3 {
		touchProject(t, pool, tenant, busy, now.Add(time.Duration(i)*time.Second))
		touchProject(t, pool, tenant, checked, now.Add(-time.Hour))
		sweepFreeze(t, s)
		if got := frozenDomains(t, pool, tenant); !slices.Equal(got, []string{busy}) {
			t.Fatalf("sweep %d froze %v, want [busy]: the live slot changed hands", i+2, got)
		}
	}
}

// The budget sweep pauses past http_checks oldest-first in one statement,
// never touches an owner's own pause, skips heartbeats, and unpauses exactly
// its own markers when the plan lifts.
func TestMonitorBudgetSweepPausesAndRestores(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	domain := fmt.Sprintf("budget-%d.example.com", time.Now().UnixNano())
	tenant := seedSweepTenant(t, pool, "Free", domain)
	project := projectID(t, pool, tenant, domain)

	// m1..m5 in creation order; m2 is the owner's own pause, hb a heartbeat
	// (never counted). Free runs 3.
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("m%d", i)
		seedMonitor(t, pool, tenant, project, "website", name, time.Duration(100-i)*time.Minute)
	}
	seedMonitor(t, pool, tenant, project, "heartbeat", "hb", 0)
	if _, err := pool.Exec(ctx,
		`UPDATE monitor SET paused = true WHERE tenant_id = $1 AND name = 'm2'`, tenant); err != nil {
		t.Fatalf("owner pause: %v", err)
	}

	sweep := func(want int64, why string) {
		t.Helper()
		n, err := s.MonitorBudgetSweep(ctx)
		if err != nil {
			t.Fatalf("%s: MonitorBudgetSweep: %v", why, err)
		}
		if n != want {
			t.Fatalf("%s: %d rows changed, want %d", why, n, want)
		}
	}
	assertPaused := func(want map[string]string, why string) {
		t.Helper()
		rows, err := pool.Query(ctx,
			`SELECT name, paused::text, COALESCE(paused_by, 'owner') FROM monitor
			  WHERE tenant_id = $1 ORDER BY name`, tenant)
		if err != nil {
			t.Fatalf("read monitors: %v", err)
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var name, paused, by string
			if err := rows.Scan(&name, &paused, &by); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[name] = paused + "/" + by
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d rows, want %d", why, len(got), len(want))
		}
		for name, w := range want {
			if got[name] != w {
				t.Fatalf("%s: %s = %s, want %s (all: %v)", why, name, got[name], w, got)
			}
		}
	}

	// Oldest kept running: m1, m2 (owner-paused, still holding its slot),
	// m3. The sweep pauses m4 and m5; the heartbeat is out of the axis.
	sweep(2, "Free budget pause")
	assertPaused(map[string]string{
		"m1": "false/owner", "m2": "true/owner", "m3": "false/owner",
		"m4": "true/plan", "m5": "true/plan", "hb": "false/owner",
	}, "Free budget pause")
	sweep(0, "a second tick with nothing to change")

	// The plan lifting the cap unpauses exactly the sweeper's own markers.
	if _, err := pool.Exec(ctx, `UPDATE tenant SET plan = 'Agency' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("upgrade tenant: %v", err)
	}
	sweep(2, "Agency restore")
	assertPaused(map[string]string{
		"m1": "false/owner", "m2": "true/owner", "m3": "false/owner",
		"m4": "false/owner", "m5": "false/owner", "hb": "false/owner",
	}, "Agency restore keeps the owner's pause")
}

// The custom domain's grace clock: stamped when the plan stops carrying it,
// cleared when the plan carries it again, unbound once the stamp is older
// than the grace window - the page itself stays.
func TestDomainGraceSweep(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	domain := fmt.Sprintf("grace-%d.example.com", uniq)
	tenant := seedSweepTenant(t, pool, "Indie", domain)
	if _, err := pool.Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, domain, domain_verified_at)
		 VALUES ($1, $2, $3, $4, now())`,
		tenant, projectID(t, pool, tenant, domain), fmt.Sprintf("grace-%d", uniq), "status."+domain); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	type page struct {
		domain   *string
		verified bool
		lapsed   bool
	}
	read := func() page {
		t.Helper()
		var p page
		if err := pool.QueryRow(ctx,
			`SELECT domain, domain_verified_at IS NOT NULL, domain_lapsed_at IS NOT NULL
			   FROM status_page WHERE tenant_id = $1`, tenant).Scan(&p.domain, &p.verified, &p.lapsed); err != nil {
			t.Fatalf("read page: %v", err)
		}
		return p
	}
	sweep := func() {
		t.Helper()
		if err := s.DomainGraceSweep(ctx); err != nil {
			t.Fatalf("DomainGraceSweep: %v", err)
		}
	}
	plan := func(name string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE tenant SET plan = $2 WHERE id = $1`, tenant, name); err != nil {
			t.Fatalf("plan %s: %v", name, err)
		}
	}

	sweep()
	if p := read(); p.lapsed || p.domain == nil {
		t.Fatalf("a plan that carries the domain stamped it: %+v", p)
	}
	plan("Free")
	sweep()
	if p := read(); !p.lapsed || p.domain == nil || !p.verified {
		t.Fatalf("downgrade: %+v, want the stamp with the domain still bound", p)
	}
	plan("Growth")
	sweep()
	if p := read(); p.lapsed {
		t.Fatalf("upgrade kept the stamp: %+v", p)
	}
	plan("Free")
	sweep()
	if _, err := pool.Exec(ctx,
		`UPDATE status_page SET domain_lapsed_at = now() - make_interval(days => $2) - interval '1 minute'
		  WHERE tenant_id = $1`, tenant, DomainGraceDays); err != nil {
		t.Fatalf("age the stamp: %v", err)
	}
	sweep()
	if p := read(); p.domain != nil || p.verified || p.lapsed {
		t.Fatalf("grace over: %+v, want the domain unbound", p)
	}
}

// Rollup rows up to the freeze are the snapshot and survive TrimHistory;
// rows the frozen project wrote after the freeze trim like anyone's.
func TestTrimHistorySparesFrozenSnapshot(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	liveD := fmt.Sprintf("trim-live-%d.example.com", uniq)
	frozenD := fmt.Sprintf("trim-frozen-%d.example.com", uniq)
	tenant := seedSweepTenant(t, pool, "Free", liveD, frozenD)
	// Frozen five days ago; Free keeps one day of history.
	if _, err := pool.Exec(ctx,
		`UPDATE project SET frozen_at = now() - interval '5 days' WHERE tenant_id = $1 AND domain = $2`, tenant, frozenD); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	seed := func(d string, hour time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO series_1h (tenant_id, project_id, hour, service, level, fingerprint, lines)
			 SELECT $1, p.id, $2, 'api', 'info', 0, 1 FROM project p WHERE p.tenant_id = $1 AND p.domain = $3`,
			tenant, hour, d); err != nil {
			t.Fatalf("seed series_1h %s: %v", d, err)
		}
	}
	seed(liveD, now.Add(-48*time.Hour))
	seed(frozenD, now.Add(-6*24*time.Hour)) // before the freeze: the snapshot
	seed(frozenD, now.Add(-48*time.Hour))   // after the freeze, past Free's depth
	if _, err := s.TrimHistory(ctx); err != nil {
		t.Fatalf("TrimHistory: %v", err)
	}
	count := func(d string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM series_1h s JOIN project p ON p.id = s.project_id
			  WHERE p.tenant_id = $1 AND p.domain = $2`, tenant, d).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", d, err)
		}
		return n
	}
	if n := count(liveD); n != 0 {
		t.Fatalf("live rollup rows = %d, want 0 past the plan's depth", n)
	}
	if n := count(frozenD); n != 1 {
		t.Fatalf("frozen rollup rows = %d, want 1: the snapshot stays, the post-freeze row trims", n)
	}
}
