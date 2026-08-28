//go:build integration

// Claim adopts (docs/plans/projects-axis.md Decisions 5 and 6): the anonymous
// tenant's footprint moves into the claimer's ONE tenant and the anonymous
// row dies — never a second membership. Both doors (token and slug) route
// through adoptTenant, so every test here drives the real POST /v1/claim with
// a session cookie. Run with -tags=integration, UC_TEST_POSTGRES set.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// claimFixture: a signed-in Free claimer (tenant, person, login member, no
// projects) and an unclaimed anonymous tenant carrying one row in every table
// the adoption must reparent.
type claimFixture struct {
	pool       *pg.Pool
	claimerID  int64
	personID   int64
	anonID     int64
	anonProjID int64
	anonDomain string
	slug       string
	rawToken   string
	claimRoute http.Handler
	cookie     *http.Cookie
}

func newClaimFixture(t *testing.T) *claimFixture {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping claim integration test")
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

	uniq := time.Now().UnixNano()
	f := &claimFixture{pool: pool}
	f.rawToken = fmt.Sprintf("tok-%d", uniq)
	claimHash := sha256.Sum256([]byte(f.rawToken))

	// The claimer: plan Free by default, no projects — the fresh signup.
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("claimer-%d", uniq)).Scan(&f.claimerID); err != nil {
		t.Fatalf("claimer tenant: %v", err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Owner') RETURNING id`,
		fmt.Sprintf("claimer-%d@example.com", uniq)).Scan(&f.personID); err != nil {
		t.Fatalf("person: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, 'login', 'active')`,
		f.claimerID, f.personID); err != nil {
		t.Fatalf("tenant_member: %v", err)
	}

	// The anonymous tenant: unclaimed (the hash IS the marker), one project
	// and one row in every other table the adoption reparents.
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, claim_token_hash) VALUES (gen_random_uuid(), 'unclaimed', $1) RETURNING id`,
		claimHash[:]).Scan(&f.anonID); err != nil {
		t.Fatalf("anon tenant: %v", err)
	}
	f.anonDomain = fmt.Sprintf("claimed-%d.example.com", uniq%100000)
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		f.anonID, f.anonDomain).Scan(&f.anonProjID); err != nil {
		t.Fatalf("anon project: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec)
		 VALUES (gen_random_uuid(), $1, $2, 'website', 'Checkout', $3, 300)`,
		f.anonID, f.anonProjID, "https://"+f.anonDomain); err != nil {
		t.Fatalf("anon monitor: %v", err)
	}
	f.slug = fmt.Sprintf("claim-%d", uniq)
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title) VALUES ($1, $2, $3, $4)`,
		f.anonID, f.anonProjID, f.slug, f.anonDomain); err != nil {
		t.Fatalf("anon status_page: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash) VALUES ($1, $2, $3, $4)`,
		f.anonID, f.anonProjID, fmt.Sprintf("uc_live_%d", uniq), claimHash[:]); err != nil {
		t.Fatalf("anon api_key: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO source_connection (tenant_id, project_id, kind) VALUES ($1, $2, 'site')`,
		f.anonID, f.anonProjID); err != nil {
		t.Fatalf("anon source_connection: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO install_token (tenant_id, project_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, now() + interval '10 minutes')`,
		f.anonID, f.anonProjID, claimHash[:]); err != nil {
		t.Fatalf("anon install_token: %v", err)
	}

	sm := session.New(pool, session.DefaultTTL, nil)
	token, err := sm.Create(ctx, f.personID, f.claimerID)
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/claim", NewInstall(pool, nil, sm, "", false))
	f.claimRoute = mux
	f.cookie = &http.Cookie{Name: session.CookieName, Value: token}
	return f
}

// claim posts a claim body through the real route with the claimer's cookie.
func (f *claimFixture) claim(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(body))
	r.AddCookie(f.cookie)
	w := httptest.NewRecorder()
	f.claimRoute.ServeHTTP(w, r)
	return w
}

func (f *claimFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.Raw().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// unclaimedCount counts THIS fixture's anonymous tenant, not every unclaimed
// tenant in the database. A whole-table count reads as a tighter assertion but
// is the opposite: the integration lane shares one DSN across packages, and
// tests running beside this one mint and reap unclaimed tenants of their own,
// so a global before/after drifts under it and fails at random. Scoped to the
// row this test owns, the same claim is exact and isolation-proof.
func (f *claimFixture) unclaimedCount(t *testing.T) int64 {
	return f.count(t,
		`SELECT count(*) FROM tenant WHERE id = $1 AND claim_token_hash IS NOT NULL`, f.anonID)
}

// The happy path, by token: the claimer's OWN tenant ends up holding the same
// project row, every footprint row moved with it, the anonymous tenant is
// gone, and the person still has exactly one tenant.
func TestClaimAdoptsTheAnonymousTenant(t *testing.T) {
	f := newClaimFixture(t)
	before := f.unclaimedCount(t)

	w := f.claim(t, `{"claimToken":"`+f.rawToken+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("claim by token = %d (%s), want 200", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["claimed"] != true {
		t.Fatalf("claim response = %s, want {\"claimed\":true}", w.Body.String())
	}

	ctx := context.Background()
	// The project row itself moved: same id, now the claimer's.
	var holder int64
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT tenant_id FROM project WHERE id = $1`, f.anonProjID).Scan(&holder); err != nil || holder != f.claimerID {
		t.Fatalf("project holder = %d (err %v), want the claimer %d", holder, err, f.claimerID)
	}
	// Every reparented table moved; nothing stayed behind on the anon tenant.
	for _, table := range [...]string{"monitor", "status_page", "source_connection", "api_key", "install_token"} {
		if n := f.count(t, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, f.claimerID); n != 1 {
			t.Fatalf("%s rows on the claimer = %d, want 1", table, n)
		}
		if n := f.count(t, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, f.anonID); n != 0 {
			t.Fatalf("%s rows still on the anon tenant = %d, want 0", table, n)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM tenant WHERE id = $1`, f.anonID); n != 0 {
		t.Fatal("the anonymous tenant outlived its own claim")
	}
	// Exactly one membership, to the claimer's own tenant: adoption must not
	// add a second one (the invisible-tenant bug this rewrites).
	if n := f.count(t, `SELECT count(*) FROM tenant_member WHERE person_id = $1`, f.personID); n != 1 {
		t.Fatalf("person holds %d memberships, want exactly 1", n)
	}
	if after := f.unclaimedCount(t); after != before-1 {
		t.Fatalf("this fixture's anonymous tenant %d -> %d, want it consumed by the adoption", before, after)
	}
}

// At the Free limit with a USED project the claim hits the 402 wall with the
// cheapest lifting plan on the wire — and nothing is spent: the token stays
// unclaimed and the page stays on the anonymous tenant.
func TestClaimAtLimitWithAUsedProjectHitsTheWall(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	var used int64
	if err := f.pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'used.example.com') RETURNING id`,
		f.claimerID).Scan(&used); err != nil {
		t.Fatalf("claimer project: %v", err)
	}
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec)
		 VALUES (gen_random_uuid(), $1, $2, 'website', 'Used', 'https://used.example.com', 300)`,
		f.claimerID, used); err != nil {
		t.Fatalf("claimer monitor: %v", err)
	}

	w := f.claim(t, `{"slug":"`+f.slug+`"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("claim at limit = %d (%s), want 402", w.Code, w.Body.String())
	}
	var wall struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Upgrade struct {
				Reason string `json:"reason"`
				Plan   string `json:"plan"`
			} `json:"upgrade"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wall); err != nil {
		t.Fatalf("decode wall: %v", err)
	}
	if wall.Error.Code != "plan_limit_exceeded" {
		t.Fatalf("code = %q, want plan_limit_exceeded", wall.Error.Code)
	}
	if wall.Error.Message != "Free allows 1 project." {
		t.Fatalf("message = %q, want the Decision 16 wording", wall.Error.Message)
	}
	if wall.Error.Upgrade.Plan != "indie" {
		t.Fatalf("upgrade.plan = %q, want indie (the cheapest lifting plan)", wall.Error.Upgrade.Plan)
	}
	// A refused claim rolls back whole: token unspent, page unmoved.
	if n := f.count(t, `SELECT count(*) FROM tenant WHERE id = $1 AND claim_token_hash IS NOT NULL`, f.anonID); n != 1 {
		t.Fatal("a refused claim must leave the token unspent")
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE tenant_id = $1`, f.anonID); n != 1 {
		t.Fatalf("anon projects = %d, want 1 (a refused claim moves nothing)", n)
	}
}

// At the same limit with an EMPTY project the claim succeeds: the placeholder
// is absorbed first (Decision 5), so the claimer ends with exactly one
// project — the claimed one — and no new anonymous tenant appeared.
func TestClaimAtLimitWithAnEmptyProjectAbsorbsIt(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	var empty int64
	if err := f.pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'placeholder.example.com') RETURNING id`,
		f.claimerID).Scan(&empty); err != nil {
		t.Fatalf("claimer project: %v", err)
	}
	// A key that never carried anything does not make the project used:
	// project_seq.next is the ingest marker the absorb checks, and here it is
	// still at the value every project is born with.
	emptyKey := sha256.Sum256([]byte(f.rawToken + "-empty"))
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash) VALUES ($1, $2, $3, $4)`,
		f.claimerID, empty, fmt.Sprintf("uc_live_e%d", time.Now().UnixNano()), emptyKey[:]); err != nil {
		t.Fatalf("claimer api_key: %v", err)
	}
	before := f.unclaimedCount(t)

	w := f.claim(t, `{"slug":"`+f.slug+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("claim with an empty project = %d (%s), want 200", w.Code, w.Body.String())
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE tenant_id = $1`, f.claimerID); n != 1 {
		t.Fatalf("claimer projects = %d, want exactly 1 (the claimed one)", n)
	}
	var domain string
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT domain FROM project WHERE tenant_id = $1`, f.claimerID).Scan(&domain); err != nil || domain != f.anonDomain {
		t.Fatalf("remaining project = %q (err %v), want the claimed %q", domain, err, f.anonDomain)
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE id = $1`, empty); n != 0 {
		t.Fatal("the placeholder project must be absorbed, not kept")
	}
	if n := f.count(t, `SELECT count(*) FROM tenant WHERE id = $1`, f.anonID); n != 0 {
		t.Fatal("the anonymous tenant must die with the adoption")
	}
	if after := f.unclaimedCount(t); after != before-1 {
		t.Fatalf("this fixture's anonymous tenant %d -> %d, want it consumed by the adoption", before, after)
	}
}

// Under the limit the absorb still fires: Decision 5's check runs
// limit-independent, BEFORE the gate, so an Indie claimer (room for 2)
// holding one empty placeholder also ends with exactly one project — the
// claimed one. A regression to "absorb only at the wall" passes every test
// above while changing behavior for every under-limit claimer.
func TestClaimUnderTheLimitWithAnEmptyProjectAbsorbsIt(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Raw().Exec(ctx,
		`UPDATE tenant SET plan = 'Indie' WHERE id = $1`, f.claimerID); err != nil {
		t.Fatalf("claimer plan: %v", err)
	}
	var empty int64
	if err := f.pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'placeholder.example.com') RETURNING id`,
		f.claimerID).Scan(&empty); err != nil {
		t.Fatalf("claimer project: %v", err)
	}
	// A key that never carried anything does not make the project used:
	// project_seq.next is the ingest marker the absorb checks, and here it is
	// still at the value every project is born with.
	emptyKey := sha256.Sum256([]byte(f.rawToken + "-room"))
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash) VALUES ($1, $2, $3, $4)`,
		f.claimerID, empty, fmt.Sprintf("uc_live_r%d", time.Now().UnixNano()), emptyKey[:]); err != nil {
		t.Fatalf("claimer api_key: %v", err)
	}

	w := f.claim(t, `{"slug":"`+f.slug+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("claim with room = %d (%s), want 200", w.Code, w.Body.String())
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE tenant_id = $1`, f.claimerID); n != 1 {
		t.Fatalf("claimer projects = %d, want exactly 1 (the claimed one)", n)
	}
	var domain string
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT domain FROM project WHERE tenant_id = $1`, f.claimerID).Scan(&domain); err != nil || domain != f.anonDomain {
		t.Fatalf("remaining project = %q (err %v), want the claimed %q", domain, err, f.anonDomain)
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE id = $1`, empty); n != 0 {
		t.Fatal("the placeholder project must be absorbed, not kept")
	}
}

// A project that has INGESTED blocks the absorb even with no monitors: an
// SDK-only account — a key wired up, logs flowing, no monitor ever created —
// is not a placeholder, so the claim 402s and that project survives.
//
// This is the case the old key_usage_log test could not actually see:
// LogKeyUsage and TouchAPIKeyLastUsed are generated but called from nowhere,
// so both of those markers are always absent and the absorb would have
// swallowed a live project. project_seq.next is written on the ingest path
// itself (LeaseSeqBlock, internal/ring/seq).
func TestClaimDoesNotAbsorbAProjectThatHasIngested(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	var proj int64
	if err := f.pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'signed.example.com') RETURNING id`,
		f.claimerID).Scan(&proj); err != nil {
		t.Fatalf("claimer project: %v", err)
	}
	usedKey := sha256.Sum256([]byte(f.rawToken + "-used"))
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash) VALUES ($1, $2, $3, $4)`,
		f.claimerID, proj, fmt.Sprintf("uc_live_u%d", time.Now().UnixNano()), usedKey[:]); err != nil {
		t.Fatalf("claimer api_key: %v", err)
	}
	// What a leased seq block leaves behind: next moved off 1.
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO project_seq (project_id, next) VALUES ($1, 4097)`, proj); err != nil {
		t.Fatalf("project_seq: %v", err)
	}

	w := f.claim(t, `{"slug":"`+f.slug+`"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("claim over an ingesting project = %d (%s), want 402", w.Code, w.Body.String())
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE id = $1`, proj); n != 1 {
		t.Fatal("a used project must never be absorbed")
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE tenant_id = $1`, f.claimerID); n != 1 {
		t.Fatalf("claimer projects = %d, want the used one still there", n)
	}
}

// Claiming a page that is already yours answers not_claimable: the adoption
// burned the token, so the burn matches no row. No special case involved —
// and nothing of the claimer's self-destructs on the way.
func TestClaimingYourOwnPageIsNotClaimable(t *testing.T) {
	f := newClaimFixture(t)
	if w := f.claim(t, `{"claimToken":"`+f.rawToken+`"}`); w.Code != http.StatusOK {
		t.Fatalf("first claim = %d (%s), want 200", w.Code, w.Body.String())
	}
	w := f.claim(t, `{"slug":"`+f.slug+`"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("own-page claim = %d (%s), want 404", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not_claimable") {
		t.Fatalf("body = %s, want not_claimable", w.Body.String())
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE tenant_id = $1`, f.claimerID); n != 1 {
		t.Fatalf("claimer projects = %d, want 1 (the echo claim must destroy nothing)", n)
	}
}
