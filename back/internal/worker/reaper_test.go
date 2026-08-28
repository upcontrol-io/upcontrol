//go:build integration

// The reaper's two statements (Decision 10 of docs/plans/projects-axis.md):
// an unclaimed tenant older than 24 h has its monitors paused, one older than
// 7 days is deleted (cascade), and a claimed tenant is never touched. Run
// with -tags=integration, UC_TEST_POSTGRES set.
package worker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// openReaperDB applies migrations and returns a pool.
func openReaperDB(t *testing.T) *pg.Pool {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping reaper integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "", "", "", "", "../../../db/postgres", ""); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedTenant inserts a tenant backdated by age. Unclaimed tenants carry a
// claim token hash (the unclaimed marker); claimed ones a claimed_at instead.
// Every tenant gets a project (the monitor FK needs one); withMonitor adds
// one unpaused monitor under it.
func seedTenant(t *testing.T, pool *pg.Pool, name string, unclaimed bool, age string, withMonitor bool) int64 {
	t.Helper()
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	var id int64
	if unclaimed {
		claimHash := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", name, uniq)))
		if err := pool.Raw().QueryRow(ctx,
			`INSERT INTO tenant (public_id, name, claim_token_hash, created_at)
			 VALUES (gen_random_uuid(), $1, $2, now() - $3::interval) RETURNING id`,
			fmt.Sprintf("%s-%d", name, uniq), claimHash[:], age).Scan(&id); err != nil {
			t.Fatalf("seed tenant %s: %v", name, err)
		}
	} else {
		if err := pool.Raw().QueryRow(ctx,
			`INSERT INTO tenant (public_id, name, claimed_at, created_at)
			 VALUES (gen_random_uuid(), $1, now(), now() - $2::interval) RETURNING id`,
			fmt.Sprintf("%s-%d", name, uniq), age).Scan(&id); err != nil {
			t.Fatalf("seed tenant %s: %v", name, err)
		}
	}
	var projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		id, fmt.Sprintf("%s-%d.example.com", name, uniq%100000)).Scan(&projectID); err != nil {
		t.Fatalf("seed project for %s: %v", name, err)
	}
	if withMonitor {
		if _, err := pool.Raw().Exec(ctx,
			`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec)
			 VALUES (gen_random_uuid(), $1, $2, 'website', 'Watch', $3, 300)`,
			id, projectID, fmt.Sprintf("https://%d.example.com", uniq%100000)); err != nil {
			t.Fatalf("seed monitor for %s: %v", name, err)
		}
	}
	return id
}

func count(t *testing.T, pool *pg.Pool, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.Raw().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// A day-old unclaimed tenant is paused (not deleted), a week-old one is gone
// with its rows, and a week-old claimed tenant survives untouched. A second
// run changes nothing.
func TestReapUnclaimedPausesAtADayDeletesAtAWeek(t *testing.T) {
	pool := openReaperDB(t)
	ctx := context.Background()

	dayOld := seedTenant(t, pool, "reaper-25h", true, "25 hours", true)
	weekOld := seedTenant(t, pool, "reaper-8d", true, "8 days", false)
	claimed := seedTenant(t, pool, "reaper-claimed-8d", false, "8 days", true)

	run := func() {
		t.Helper()
		if err := reapUnclaimed(ctx, pool); err != nil {
			t.Fatalf("reapUnclaimed: %v", err)
		}
	}
	assertState := func(tag string) {
		t.Helper()
		// 25 h: paused, still alive.
		if n := count(t, pool, `SELECT count(*) FROM tenant WHERE id = $1`, dayOld); n != 1 {
			t.Fatalf("%s: 25 h unclaimed tenant rows = %d, want 1 (deleted only at 7 days)", tag, n)
		}
		if n := count(t, pool, `SELECT count(*) FROM monitor WHERE tenant_id = $1 AND paused`, dayOld); n != 1 {
			t.Fatalf("%s: paused monitors on the 25 h tenant = %d, want 1", tag, n)
		}
		if n := count(t, pool, `SELECT count(*) FROM monitor WHERE tenant_id = $1 AND NOT paused`, dayOld); n != 0 {
			t.Fatalf("%s: unpaused monitors on the 25 h tenant = %d, want 0", tag, n)
		}
		// 8 days unclaimed: gone, cascade took its project.
		if n := count(t, pool, `SELECT count(*) FROM tenant WHERE id = $1`, weekOld); n != 0 {
			t.Fatalf("%s: 8 day unclaimed tenant rows = %d, want 0", tag, n)
		}
		if n := count(t, pool, `SELECT count(*) FROM project WHERE tenant_id = $1`, weekOld); n != 0 {
			t.Fatalf("%s: 8 day tenant's surviving projects = %d, want 0 (cascade)", tag, n)
		}
		// 8 days claimed: untouched, monitor running, claim state unchanged.
		if n := count(t, pool, `SELECT count(*) FROM tenant WHERE id = $1 AND claim_token_hash IS NULL AND claimed_at IS NOT NULL`, claimed); n != 1 {
			t.Fatalf("%s: claimed tenant untouched = %d, want 1 (exists, still claimed)", tag, n)
		}
		if n := count(t, pool, `SELECT count(*) FROM monitor WHERE tenant_id = $1 AND NOT paused`, claimed); n != 1 {
			t.Fatalf("%s: running monitors on the claimed tenant = %d, want 1", tag, n)
		}
	}
	run()
	assertState("first run")

	// Idempotent: the pause matches nothing already paused, the delete target
	// is gone, and nothing else crossed a threshold.
	run()
	assertState("second run")
}

// Not every unclaimed tenant is an abandoned demo page. `uc init` without an
// account mints one through POST /v1/projects/anonymous and hands over its API
// key, and the developer may ship data through it for weeks before signing up.
// project_seq.next off its birth value is that ingest, and it keeps the tenant
// alive past the seven days — otherwise the reaper deletes a live install's
// project and key out from under it.
func TestReapUnclaimedSparesAnAnonymousInstallThatIsIngesting(t *testing.T) {
	pool := openReaperDB(t)
	ctx := context.Background()

	install := seedTenant(t, pool, "reaper-install-8d", true, "8 days", false)
	idle := seedTenant(t, pool, "reaper-idle-8d", true, "8 days", false)

	released := seedTenant(t, pool, "reaper-released-8d", true, "8 days", false)

	// An install in use is BOTH halves: data has flowed (project_seq off its
	// birth value) and it still holds the key that carried it.
	ingested := func(tenantID int64) {
		t.Helper()
		if _, err := pool.Raw().Exec(ctx,
			`INSERT INTO project_seq (project_id, next)
			 SELECT id, 4097 FROM project WHERE tenant_id = $1
			 ON CONFLICT (project_id) DO UPDATE SET next = EXCLUDED.next`, tenantID); err != nil {
			t.Fatalf("seed project_seq: %v", err)
		}
	}
	ingested(install)
	ingested(released)
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash)
		 SELECT $1, id, $2, $3 FROM project WHERE tenant_id = $1`,
		install, fmt.Sprintf("uc_live_i%d", time.Now().UnixNano()), []byte("hash")); err != nil {
		t.Fatalf("seed api_key: %v", err)
	}

	if err := reapUnclaimed(ctx, pool); err != nil {
		t.Fatalf("reapUnclaimed: %v", err)
	}

	if n := count(t, pool, `SELECT count(*) FROM tenant WHERE id = $1`, install); n != 1 {
		t.Fatalf("an ingesting anonymous install was reaped (rows = %d, want 1)", n)
	}
	// The neighbour proves the exclusion is real and not a dead statement:
	// same age, same shape, nothing ever ingested.
	if n := count(t, pool, `SELECT count(*) FROM tenant WHERE id = $1`, idle); n != 0 {
		t.Fatalf("an idle 8 day unclaimed tenant survived (rows = %d, want 0)", n)
	}
	// A page RELEASED by a project deletion has ingested plenty, but its key
	// died with the release. It is ownerless, which is what this job collects —
	// the seq alone would have spared it forever.
	if n := count(t, pool, `SELECT count(*) FROM tenant WHERE id = $1`, released); n != 0 {
		t.Fatalf("a released ownerless page survived the reaper (rows = %d, want 0)", n)
	}
}
