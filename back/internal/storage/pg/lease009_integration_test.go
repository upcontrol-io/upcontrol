//go:build integration

// LeaseDueTargets, the derived-cadence lease query of migration 009, against
// a real Postgres: liveness from subscribers, the host-page cadence ladder,
// heartbeats never leased, paying subscribers first, refusal backoff, and
// stale-lease recovery. Every test gets a database of its own.
// Run with -tags=integration, UC_TEST_POSTGRES set.
package pg

import (
	"context"
	"testing"
	"time"

	sqlc "go.upcontrol.io/back/gen/pg"
)

// leaseWorld seeds one tenant+project and hands back ids for the helpers.
type leaseWorld struct {
	pool     *Pool
	tenantID int64
	project  int64
	project2 int64
}

func newLeaseWorld(t *testing.T) *leaseWorld {
	t.Helper()
	dsn := newTestDB(t)
	applyAll(t, dsn)
	pool := openPool(t, dsn)
	w := &leaseWorld{pool: pool}
	ctx := context.Background()
	if err := pool.db.QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, plan) VALUES (gen_random_uuid(), 'lease-free', 'Free') RETURNING id`,
	).Scan(&w.tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := pool.db.QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'lease.example') RETURNING id`,
		w.tenantID).Scan(&w.project); err != nil {
		t.Fatalf("project: %v", err)
	}
	// A second project of the same tenant: a project may subscribe to a
	// target only once (UNIQUE (project_id, target_id)), so a paused+active
	// pair on one target lives in two projects.
	if err := pool.db.QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'lease2.example') RETURNING id`,
		w.tenantID).Scan(&w.project2); err != nil {
		t.Fatalf("project2: %v", err)
	}
	return w
}

func (w *leaseWorld) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := w.pool.db.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("exec: %v\n%s", err, query)
	}
}

// addTarget makes a website target with its schedule row, due now.
func (w *leaseWorld) addTarget(t *testing.T, url string) int64 {
	t.Helper()
	var id int64
	if err := w.pool.db.QueryRow(context.Background(),
		`INSERT INTO probe_target (key, kind, url) VALUES ('website' || chr(31) || $1 || chr(31), 'website', $1) RETURNING id`,
		url).Scan(&id); err != nil {
		t.Fatalf("target: %v", err)
	}
	w.exec(t, `INSERT INTO target_schedule (target_id, region, next_due_at) VALUES ($1, 'default', now() - interval '1 minute')`, id)
	return id
}

// addMonitor subscribes a monitor of w's tenant to the target; the paused
// variant lands in the second project (the UNIQUE allows one per project).
func (w *leaseWorld) addMonitor(t *testing.T, targetID int64, intervalSec int, paused bool) {
	t.Helper()
	project := w.project
	if paused {
		project = w.project2
	}
	if _, err := w.pool.db.Exec(context.Background(),
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec, paused, target_id)
		 VALUES (gen_random_uuid(), $1, $2, 'website', 'M', 'x', $3, $4, $5)`,
		w.tenantID, project, intervalSec, paused, targetID); err != nil {
		t.Fatalf("monitor: %v", err)
	}
}

// addPayingMonitor subscribes a monitor under a paying-plan tenant.
func (w *leaseWorld) addPayingMonitor(t *testing.T, targetID int64, intervalSec int) int64 {
	t.Helper()
	var tenant int64
	if err := w.pool.db.QueryRow(context.Background(),
		`INSERT INTO tenant (public_id, name, plan) VALUES (gen_random_uuid(), 'lease-growth', 'Growth') RETURNING id`,
	).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	var project int64
	if err := w.pool.db.QueryRow(context.Background(),
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'paying.example') RETURNING id`,
		tenant).Scan(&project); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := w.pool.db.QueryRow(context.Background(),
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec, target_id)
		 VALUES (gen_random_uuid(), $1, $2, 'website', 'Pay', 'x', $3, $4) RETURNING id`,
		tenant, project, intervalSec, targetID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// addHostPage mints a status page rooting on the target; claimed pages get a
// claimed tenant, unclaimed ones a claim token.
func (w *leaseWorld) addHostPage(t *testing.T, targetID int64, slug string, claimed bool) {
	t.Helper()
	if claimed {
		w.exec(t, `INSERT INTO status_page (tenant_id, project_id, slug, title, root_target_id, is_host_page)
		           VALUES ($1, $2, $3, 'P', $4, true)`, w.tenantID, w.project, slug, targetID)
		return
	}
	var tenant int64
	if err := w.pool.db.QueryRow(context.Background(),
		`INSERT INTO tenant (public_id, name, claim_token_hash) VALUES (gen_random_uuid(), 'unclaimed', convert_to($1 || '-claim', 'utf8')) RETURNING id`,
		slug).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	var project int64
	if err := w.pool.db.QueryRow(context.Background(),
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'unclaimed.example') RETURNING id`,
		tenant).Scan(&project); err != nil {
		t.Fatal(err)
	}
	w.exec(t, `INSERT INTO status_page (tenant_id, project_id, slug, title, root_target_id, is_host_page)
	           VALUES ($1, $2, $3, 'P', $4, true)`, tenant, project, slug, targetID)
}

func lease(t *testing.T, w *leaseWorld) []sqlc.LeaseDueTargetsRow {
	t.Helper()
	rows, err := w.pool.Queries().LeaseDueTargets(context.Background(), 50)
	if err != nil {
		t.Fatalf("LeaseDueTargets: %v", err)
	}
	return rows
}

func intervalsByTarget(rows []sqlc.LeaseDueTargetsRow) map[int64]int32 {
	out := map[int64]int32{}
	for _, r := range rows {
		out[r.ID] = r.IntervalSec
	}
	return out
}

// A paused subscription next to an active one keeps the target due at the
// active interval; pausing both makes it never due.
func TestLeasePausedPairKeepsTargetDue(t *testing.T) {
	w := newLeaseWorld(t)
	target := w.addTarget(t, "https://pair.example")
	w.addMonitor(t, target, 300, true) // paused
	w.addMonitor(t, target, 60, false) // active: the tightest live interval wins

	rows := lease(t, w)
	iv := intervalsByTarget(rows)
	if got, ok := iv[target]; !ok || got != 60 {
		t.Fatalf("paused+active pair: interval = %d/%v, want 60", got, ok)
	}

	w.exec(t, `UPDATE monitor SET paused = true WHERE target_id = $1`, target)
	if rows = lease(t, w); len(rows) != 0 {
		t.Fatalf("both paused: %d targets still due, want 0", len(rows))
	}
}

// The host-page cadence ladder for a target with no subscribers: 300 s in
// the first 24 h, 900 s after, 3600 s only when every referencing page is
// unclaimed, unindexed and untouched for 30 days.
func TestLeaseHostPageCadenceLadder(t *testing.T) {
	w := newLeaseWorld(t)
	fresh := w.addTarget(t, "https://fresh.example")
	seen := w.addTarget(t, "https://seen.example")
	cold := w.addTarget(t, "https://cold.example")
	coldClaimed := w.addTarget(t, "https://coldclaimed.example")
	coldIndexed := w.addTarget(t, "https://coldindexed.example")
	neverTouched := w.addTarget(t, "https://nevertouched.example")

	w.addHostPage(t, fresh, "fresh", false)
	w.addHostPage(t, seen, "seen", false)
	w.addHostPage(t, cold, "cold", false)
	w.addHostPage(t, coldClaimed, "coldclaimed", true)
	w.addHostPage(t, coldIndexed, "coldindexed", false)
	w.addHostPage(t, neverTouched, "nevertouched", false) // last_seen_at stays NULL

	w.exec(t, `UPDATE probe_target SET created_at = now() - interval '40 days' WHERE id = ANY($1)`,
		[]int64{seen, cold, coldClaimed, coldIndexed, neverTouched})
	w.exec(t, `UPDATE status_page SET last_seen_at = now() - interval '2 days' WHERE slug = 'seen'`)
	w.exec(t, `UPDATE status_page SET last_seen_at = now() - interval '35 days' WHERE slug IN ('cold', 'coldclaimed')`)
	w.exec(t, `UPDATE status_page SET last_seen_at = now() - interval '35 days', indexed_at = now() - interval '5 days' WHERE slug = 'coldindexed'`)

	iv := intervalsByTarget(lease(t, w))
	for _, tc := range []struct {
		id   int64
		want int32
		name string
	}{
		{fresh, 300, "first 24 h"},
		{seen, 900, "recently seen"},
		{cold, 3600, "unclaimed, unindexed, untouched 30 d"},
		{coldClaimed, 900, "claimed page never slows"},
		{coldIndexed, 900, "indexed page never slows"},
		{neverTouched, 3600, "never touched counts as cold (NULL last_seen_at reads as created_at)"},
	} {
		if got, ok := iv[tc.id]; !ok || got != tc.want {
			t.Errorf("%s: interval = %d/%v, want %d", tc.name, got, ok, tc.want)
		}
	}
}

// Heartbeat targets are never leased, no matter how overdue.
func TestLeaseNeverReturnsHeartbeats(t *testing.T) {
	w := newLeaseWorld(t)
	var hb int64
	if err := w.pool.db.QueryRow(context.Background(),
		`INSERT INTO probe_target (key, kind, url) VALUES ('heartbeat' || chr(31) || '018f00c3-0000-7000-8000-000000000001', 'heartbeat', 'hb') RETURNING id`,
	).Scan(&hb); err != nil {
		t.Fatal(err)
	}
	w.exec(t, `INSERT INTO target_schedule (target_id, region, next_due_at) VALUES ($1, 'default', now() - interval '1 hour')`, hb)
	// And a heartbeat monitor row through it, exactly like the migration makes.
	w.exec(t, `INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec, target_id)
	           VALUES ('018f00c3-0000-7000-8000-000000000001', $1, $2, 'heartbeat', 'Cron', 'hb', 300, $3)`,
		w.tenantID, w.project, hb)

	// A website target beside it proves the query ran and found work.
	site := w.addTarget(t, "https://site.example")
	w.addMonitor(t, site, 300, false)

	rows := lease(t, w)
	for _, r := range rows {
		if r.ID == hb {
			t.Fatal("LeaseDueTargets returned a heartbeat target")
		}
	}
	if len(rows) != 1 || rows[0].ID != site {
		t.Fatalf("lease = %v, want only the website target", rows)
	}
}

// Paying subscribers' targets lease first, even when less overdue.
func TestLeasePayingSubscriberOrdersFirst(t *testing.T) {
	w := newLeaseWorld(t)
	freeOld := w.addTarget(t, "https://freeold.example") // more overdue...
	w.exec(t, `UPDATE target_schedule SET next_due_at = now() - interval '1 hour' WHERE target_id = $1`, freeOld)
	w.addMonitor(t, freeOld, 300, false)

	payingNew := w.addTarget(t, "https://payingnew.example") // ...than the paying one
	w.addPayingMonitor(t, payingNew, 300)

	rows := lease(t, w)
	if len(rows) != 2 {
		t.Fatalf("lease returned %d targets, want 2", len(rows))
	}
	if rows[0].ID != payingNew || !rows[0].Paying {
		t.Fatalf("first leased = %d (paying=%v), want the paying target %d first", rows[0].ID, rows[0].Paying, payingNew)
	}
}

// A target in refusal backoff is not leased until the backoff expires; an
// expired lease is taken back and its previous holder reported.
func TestLeaseBackoffAndStaleLease(t *testing.T) {
	w := newLeaseWorld(t)
	target := w.addTarget(t, "https://backoff.example")
	w.addMonitor(t, target, 300, false)
	w.exec(t, `INSERT INTO target_facts (target_id, status, consecutive_refusals, backoff_until)
	           VALUES ($1, 'could_not_measure', 2, now() + interval '10 minutes')`, target)

	if rows := lease(t, w); len(rows) != 0 {
		t.Fatalf("target in backoff was leased: %v", rows)
	}

	// Backoff expires: the target comes back.
	w.exec(t, `UPDATE target_facts SET backoff_until = now() - interval '1 second' WHERE target_id = $1`, target)
	rows := lease(t, w)
	if len(rows) != 1 || rows[0].ID != target {
		t.Fatalf("target out of backoff was not leased: %v", rows)
	}

	// A stale lease (holder never submitted, lease_until past) is admitted
	// again and the previous holder is named for the log line.
	w.exec(t, `UPDATE target_schedule SET leased_by = 'dead-node', lease_until = now() - interval '2 minutes' WHERE target_id = $1`, target)
	w.exec(t, `UPDATE target_schedule SET next_due_at = now() - interval '1 minute' WHERE target_id = $1`, target)
	rows = lease(t, w)
	if len(rows) != 1 {
		t.Fatalf("stale lease not recovered: %v", rows)
	}
	if rows[0].PrevLeasedBy == nil || *rows[0].PrevLeasedBy != "dead-node" {
		t.Fatalf("prev_leased_by = %v, want dead-node", rows[0].PrevLeasedBy)
	}
	if err := w.pool.Queries().SetLease(context.Background(), sqlc.SetLeaseParams{
		LeasedBy: ptr("live-node"), Column2: []int64{target},
	}); err != nil {
		t.Fatal(err)
	}
	var leasedBy string
	var until time.Time
	if err := w.pool.db.QueryRow(context.Background(),
		`SELECT leased_by, lease_until FROM target_schedule WHERE target_id = $1`, target).Scan(&leasedBy, &until); err != nil {
		t.Fatal(err)
	}
	if leasedBy != "live-node" || until.Before(time.Now()) {
		t.Fatalf("SetLease wrote %q until %v, want live-node in the future", leasedBy, until)
	}
}

func ptr(s string) *string { return &s }
