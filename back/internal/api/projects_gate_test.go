//go:build integration

// The projects plan-axis gate: the refusal wording (mirroring the http_checks
// wall), the cheapest-lifting-plan hint, and the NULL = unlimited contract.
// Run with -tags=integration, UC_TEST_POSTGRES set; the migrations (026) seed
// the ladder the assertions read back.

package api

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// openProjectsGateDB applies migrations and returns a pool on the real
// plan_entitlement rows — the table is the source of truth under test.
func openProjectsGateDB(t *testing.T) *pg.Pool {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping projects-gate integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "../../../db/postgres"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedPlanTenant inserts a tenant on the plan holding count projects, owned
// by a person of its own: everything a screen reads now resolves through the
// reader, so a workspace with no owner reaches none of its projects.
func seedPlanTenant(t *testing.T, pool *pg.Pool, plan string, count int) int64 {
	t.Helper()
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	var ownerID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Gate') RETURNING id`,
		fmt.Sprintf("gate-%d@example.com", uniq)).Scan(&ownerID); err != nil {
		t.Fatalf("seed owner for %s: %v", plan, err)
	}
	var tenantID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, plan, owner_person_id) VALUES (gen_random_uuid(), $1, $2, $3) RETURNING id`,
		fmt.Sprintf("projects-gate-%d", uniq), plan, ownerID).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant on %s: %v", plan, err)
	}
	for i := 0; i < count; i++ {
		if _, err := pool.Raw().Exec(ctx,
			`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2)`,
			tenantID, fmt.Sprintf("gate-%d.example.com", i)); err != nil {
			t.Fatalf("seed project %d for %s: %v", i, plan, err)
		}
	}
	return tenantID
}

// planTenantAPI is a writeAPI whose fixed identity is the workspace's owner:
// the handlers resolve the current project off it, as a cookie session would.
func planTenantAPI(t *testing.T, pool *pg.Pool, tenantID int64) *writeAPI {
	t.Helper()
	var ownerID int64
	if err := pool.Raw().QueryRow(context.Background(),
		`SELECT owner_person_id FROM tenant WHERE id = $1`, tenantID).Scan(&ownerID); err != nil {
		t.Fatalf("read the workspace owner: %v", err)
	}
	return &writeAPI{pool: pool, keys: pg.NewKeyResolver(pool, nil), sess: session.New(pool, session.DefaultTTL, nil).
		WithFixedIdentity(ownerID, tenantID)}
}

func TestProjectsRefusal(t *testing.T) {
	pool := openProjectsGateDB(t)
	h := &writeAPI{pool: pool}
	ctx := context.Background()
	cases := []struct {
		name        string
		plan        string
		count       int
		wantMsg     string
		wantUpgrade string
	}{
		{"Free below the limit", "Free", 0, "", ""},
		{"Free at the limit points one rung up", "Free", 1, "Free allows 1 project.", "indie"},
		{"Indie at the limit points at Growth", "Indie", 2, "Indie allows 2 projects.", "growth"},
		{"Growth at the limit points at Agency", "Growth", 5, "Growth allows 5 projects.", "agency"},
		{"Agency at the top has no plan hint", "Agency", 10, "Agency allows 10 projects.", ""},
		{"Self-hosted is unlimited", "Self-hosted", 50, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenantID := seedPlanTenant(t, pool, c.plan, c.count)
			msg, plan := h.projectsRefusal(ctx, tenantID)
			if msg != c.wantMsg || plan != c.wantUpgrade {
				t.Fatalf("projectsRefusal(%s, %d projects) = (%q, %q), want (%q, %q)",
					c.plan, c.count, msg, plan, c.wantMsg, c.wantUpgrade)
			}
		})
	}
}

// The ladder hint reads the entitlement rows, not constants: a limit bump in
// a migration must change the answer without a code edit here.
func TestUpgradePlanForProjectsFollowsTheTable(t *testing.T) {
	pool := openProjectsGateDB(t)
	ctx := context.Background()
	cases := []struct {
		count int64
		want  string
	}{
		{1, "indie"},
		{2, "growth"},
		{5, "agency"},
		{10, ""}, // Agency is the top rung: no plan field on the 402
		{50, ""},
	}
	for _, c := range cases {
		if got := upgradePlanForProjects(ctx, pool, c.count); got != c.want {
			t.Fatalf("upgradePlanForProjects(%d) = %q, want %q", c.count, got, c.want)
		}
	}
}
