//go:build integration

// A sign-in account is born WITH the email channel seeded; a second sign-in
// seeds no duplicate. Run: go test -tags=integration ./internal/account/auth/...
package auth

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

func TestProvisionSeedsEmailChannelOnce(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping provisioning integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "../../../../db/postgres"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// A unique address per run: the database may hold earlier runs' accounts.
	email := fmt.Sprintf("signin-%d@example.com", time.Now().UnixNano())

	_, tenantID, err := Provision(ctx, pool, email, "", nil, false)
	if err != nil || tenantID == 0 {
		t.Fatalf("provision: tenantID=%d err=%v", tenantID, err)
	}

	count := func() int {
		var n int
		if err := pool.Raw().QueryRow(ctx,
			`SELECT count(*) FROM alert_channel WHERE tenant_id = $1 AND kind = 'email'`,
			tenantID).Scan(&n); err != nil {
			t.Fatalf("count channels: %v", err)
		}
		return n
	}

	if n := count(); n != 1 {
		t.Fatalf("a freshly provisioned sign-in account has %d email channels, want exactly 1 — the first incident has to reach somebody", n)
	}

	// A repeat sign-in is an existing person: the seeding is creation-time only.
	if _, again, err := Provision(ctx, pool, email, "", nil, false); err != nil || again != tenantID {
		t.Fatalf("second provision: tenant=%d err=%v (want same tenant %d)", again, err, tenantID)
	}
	if n := count(); n != 1 {
		t.Fatalf("a repeat sign-in seeded a duplicate: %d email channels, want 1", n)
	}
}

// Single-user mode: the synthetic session has no token hash, so the
// token-joined GetMe can never answer for it; me keys off the identity.
func TestMe_FixedIdentityAnswersWithoutCookie(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping single-user /v1/me integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "../../../../db/postgres"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	email := fmt.Sprintf("owner-%d@localhost", time.Now().UnixNano())
	personID, tenantID, err := Provision(ctx, pool, email, "", nil, true)
	if err != nil {
		t.Fatalf("provision owner: %v", err)
	}

	sm := session.New(pool, 0, nil).WithFixedIdentity(personID, tenantID)
	rec := httptest.NewRecorder()
	NewMe(pool, sm).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/me", nil))

	if rec.Code != 200 {
		t.Fatalf("/v1/me without a cookie in single-user mode = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, email) {
		t.Fatalf("/v1/me does not carry the owner email %q: %s", email, body)
	}
	if !strings.Contains(body, `"Self-hosted"`) {
		t.Fatalf("/v1/me owner is not on the Self-hosted plan (Decision 7): %s", body)
	}
}
