//go:build integration

// The projects axis endpoints (docs/plans/projects-axis.md T6) and the
// current-project resolver's four-state matrix, asserted against a real
// Postgres: list/switch over the mounted writeAPI routes, the 402 wall with
// its upgrade.plan, missing_domain, the provisioning POST owes a new project,
// and the single-user no-op. Same lane as the claim tests: real migrations,
// real session cookie, -tags=integration with UC_TEST_POSTGRES set.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// projectsFixture: a signed-in Free tenant with one project, and a mux
// mounting the three new routes exactly as cmd/ucapi does.
type projectsFixture struct {
	pool      *pg.Pool
	tenantID  int64
	personID  int64
	projectID int64 // the tenant's one seeded project
	route     http.Handler
	sess      *session.Manager
	cookie    *http.Cookie
}

func newProjectsFixture(t *testing.T) *projectsFixture {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping projects integration test")
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
	f := &projectsFixture{pool: pool}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("projects-%d", uniq)).Scan(&f.tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Owner') RETURNING id`,
		fmt.Sprintf("projects-%d@example.com", uniq)).Scan(&f.personID); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, 'login', 'active')`,
		f.tenantID, f.personID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	f.projectID = seedProject(t, pool, f.tenantID, fmt.Sprintf("first-%d.example.com", uniq%100000))

	f.sess = session.New(pool, session.DefaultTTL, nil)
	token, err := f.sess.Create(ctx, f.personID, f.tenantID)
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	f.cookie = &http.Cookie{Name: session.CookieName, Value: token}

	wa := NewWriteAPI(pool, nil, f.sess, false, nil, nil, false)
	mux := http.NewServeMux()
	mux.Handle("GET /v1/projects", wa)
	mux.Handle("POST /v1/projects", wa)
	mux.Handle("POST /v1/project/switch", wa)
	mux.Handle("DELETE /v1/project", wa)
	f.route = mux
	return f
}

func seedProject(t *testing.T, pool *pg.Pool, tenantID int64, domain string) int64 {
	t.Helper()
	var id int64
	if err := pool.Raw().QueryRow(context.Background(),
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		tenantID, domain).Scan(&id); err != nil {
		t.Fatalf("seed project %q: %v", domain, err)
	}
	return id
}

func (f *projectsFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.AddCookie(f.cookie)
	w := httptest.NewRecorder()
	f.route.ServeHTTP(w, r)
	return w
}

func (f *projectsFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.Raw().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// The resolver's four-state matrix (Critic [T5] △, now committed instead of
// live-verified): the session's pick when it belongs to the tenant, min(id)
// for a stale pick and for an unset one, 0 for a tenant with no projects —
// plus the cross-tenant discipline the guard fold added: a foreign session's
// pick is never honoured.
func TestCurrentProjectIDMatrix(t *testing.T) {
	f := newProjectsFixture(t)
	ctx := context.Background()
	first := f.projectID
	second := seedProject(t, f.pool, f.tenantID, "second.example.com")
	if second < first {
		first, second = second, first
	}
	// A stale pick: a project that no longer exists in the tenant.
	gone := seedProject(t, f.pool, f.tenantID, "gone.example.com")
	if _, err := f.pool.Raw().Exec(ctx, `DELETE FROM project WHERE id = $1`, gone); err != nil {
		t.Fatalf("delete stale project: %v", err)
	}
	// A foreign tenant with its own project: its pick must not leak in.
	var otherTenant int64
	if err := f.pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), 'foreign') RETURNING id`).Scan(&otherTenant); err != nil {
		t.Fatalf("seed foreign tenant: %v", err)
	}
	foreignPick := seedProject(t, f.pool, otherTenant, "foreign.example.com")
	// A tenant with no projects at all.
	var emptyTenant int64
	if err := f.pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), 'empty') RETURNING id`).Scan(&emptyTenant); err != nil {
		t.Fatalf("seed empty tenant: %v", err)
	}

	for _, tc := range []struct {
		name     string
		s        sqlc.Session
		want     int64
		tenantID int64
	}{
		{"pick honoured", sqlc.Session{TenantID: f.tenantID, ProjectID: &second}, second, f.tenantID},
		{"stale pick falls to min(id)", sqlc.Session{TenantID: f.tenantID, ProjectID: &gone}, first, f.tenantID},
		{"unset pick falls to min(id)", sqlc.Session{TenantID: f.tenantID}, first, f.tenantID},
		{"empty tenant answers 0", sqlc.Session{}, 0, emptyTenant},
		{"foreign session pick is never honoured", sqlc.Session{TenantID: otherTenant, ProjectID: &foreignPick}, first, f.tenantID},
		{"failed session re-read keeps the caller's tenant", sqlc.Session{ProjectID: &second}, first, f.tenantID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := currentProjectID(ctx, f.pool, tc.s, tc.tenantID); got != tc.want {
				t.Fatalf("currentProjectID = %d, want %d", got, tc.want)
			}
		})
	}
}

// The list hands out the same public-id encoding /v1/me uses (bare hex, no
// prefix), and the switch points the session row at the chosen project.
func TestListAndSwitchProjects(t *testing.T) {
	f := newProjectsFixture(t)
	ctx := context.Background()
	second := seedProject(t, f.pool, f.tenantID, "second.example.com")

	w := f.do(t, http.MethodGet, "/v1/projects", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d (%s), want 200", w.Code, w.Body.String())
	}
	var listed struct {
		Projects []struct {
			ID        string `json:"id"`
			Domain    string `json:"domain"`
			CreatedAt string `json:"createdAt"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Projects) != 2 {
		t.Fatalf("listed %d projects, want 2", len(listed.Projects))
	}
	byDomain := map[string]string{}
	for _, p := range listed.Projects {
		if p.CreatedAt == "" {
			t.Fatalf("project %q listed without createdAt", p.Domain)
		}
		byDomain[p.Domain] = p.ID
	}
	// The id is the hex of the row's public_id — the encoding /v1/me emits,
	// which is what makes the front's "current row" an equality check.
	for _, id := range []int64{f.projectID, second} {
		var domain string
		var pubID pgtype.UUID
		if err := f.pool.Raw().QueryRow(ctx,
			`SELECT domain, public_id FROM project WHERE id = $1`, id).Scan(&domain, &pubID); err != nil {
			t.Fatalf("read project: %v", err)
		}
		pubHex := uuidStr(pubID)
		if byDomain[domain] != pubHex {
			t.Fatalf("id for %q = %q, want the public_id hex %q", domain, byDomain[domain], pubHex)
		}
	}

	w = f.do(t, http.MethodPost, "/v1/project/switch", `{"id":"`+byDomain["second.example.com"]+`"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("switch = %d (%s), want 204", w.Code, w.Body.String())
	}
	var pick int64
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT project_id FROM session WHERE person_id = $1`, f.personID).Scan(&pick); err != nil || pick != second {
		t.Fatalf("session project_id = %d (err %v), want the switched project %d", pick, err, second)
	}

	// An id this tenant does not hold — another tenant's, or pure noise — is
	// the same 404 unknown_project, and the session row keeps its pick.
	w = f.do(t, http.MethodPost, "/v1/project/switch", `{"id":"deadbeef"}`)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown_project") {
		t.Fatalf("unknown switch = %d (%s), want 404 unknown_project", w.Code, w.Body.String())
	}
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT project_id FROM session WHERE person_id = $1`, f.personID).Scan(&pick); err != nil || pick != second {
		t.Fatalf("session project_id after refused switch = %d (err %v), want %d unchanged", pick, err, second)
	}
}

// At the Free limit (1 project) the POST hits the wall with the cheapest
// lifting plan on the wire, and nothing is written.
func TestCreateProjectAtTheLimitAnswersTheWall(t *testing.T) {
	f := newProjectsFixture(t)

	w := f.do(t, http.MethodPost, "/v1/projects", `{"domain":"second.example.com"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("create at limit = %d (%s), want 402", w.Code, w.Body.String())
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
	if wall.Error.Code != "plan_limit_exceeded" || wall.Error.Message != "Free allows 1 project." {
		t.Fatalf("wall = %q / %q, want plan_limit_exceeded with the Decision 16 wording",
			wall.Error.Code, wall.Error.Message)
	}
	if wall.Error.Upgrade.Plan != "indie" {
		t.Fatalf("upgrade.plan = %q, want indie (the cheapest lifting plan)", wall.Error.Upgrade.Plan)
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE tenant_id = $1`, f.tenantID); n != 1 {
		t.Fatalf("projects after a refused create = %d, want 1 (the seeded one)", n)
	}
}

// A body without a domain is the one 400 this endpoint defines.
func TestCreateProjectMissingDomain(t *testing.T) {
	f := newProjectsFixture(t)
	w := f.do(t, http.MethodPost, "/v1/projects", `{}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "missing_domain") {
		t.Fatalf("empty create = %d (%s), want 400 missing_domain", w.Code, w.Body.String())
	}
}

// The happy path provisions what ensureAccount provisions (project,
// project_seq, api_key) and points the session at the new row.
func TestCreateProjectProvisionsAndSwitches(t *testing.T) {
	f := newProjectsFixture(t)
	// Room for a second project: Indie allows 2.
	if _, err := f.pool.Raw().Exec(context.Background(),
		`UPDATE tenant SET plan = 'Indie' WHERE id = $1`, f.tenantID); err != nil {
		t.Fatalf("set plan: %v", err)
	}

	w := f.do(t, http.MethodPost, "/v1/projects", `{"domain":"born.example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d (%s), want 200", w.Code, w.Body.String())
	}
	var created struct {
		ID        string `json:"id"`
		Domain    string `json:"domain"`
		CreatedAt string `json:"createdAt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Domain != "born.example.com" || created.ID == "" || created.CreatedAt == "" {
		t.Fatalf("created = %+v, want id, domain and createdAt", created)
	}
	ctx := context.Background()
	var id int64
	var pubID pgtype.UUID
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT id, public_id FROM project WHERE tenant_id = $1 AND domain = 'born.example.com'`,
		f.tenantID).Scan(&id, &pubID); err != nil {
		t.Fatalf("created project row: %v", err)
	}
	pubHex := uuidStr(pubID)
	if created.ID != pubHex {
		t.Fatalf("created id = %q, want the public_id hex %q (the /v1/me encoding)", created.ID, pubHex)
	}
	if n := f.count(t, `SELECT count(*) FROM project_seq WHERE project_id = $1`, id); n != 1 {
		t.Fatalf("project_seq rows = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM api_key WHERE project_id = $1 AND state = 'active'`, id); n != 1 {
		t.Fatalf("active api_key rows = %d, want 1 (the ingest door opens with the project)", n)
	}
	var pick int64
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT project_id FROM session WHERE person_id = $1`, f.personID).Scan(&pick); err != nil || pick != id {
		t.Fatalf("session project_id = %d (err %v), want the new project %d", pick, err, id)
	}
}

// A single-user session (self-host fixed identity, no session row) answers
// 204 without writing anything: the resolver's fallback owns its project.
func TestSwitchIsANoOpForASingleUserSession(t *testing.T) {
	f := newProjectsFixture(t)
	sm := session.New(f.pool, session.DefaultTTL, nil).WithFixedIdentity(f.personID, f.tenantID)
	wa := NewWriteAPI(f.pool, nil, sm, false, nil, nil, false)
	mux := http.NewServeMux()
	mux.Handle("POST /v1/project/switch", wa)

	r := httptest.NewRequest(http.MethodPost, "/v1/project/switch", strings.NewReader(`{"id":"deadbeef"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("single-user switch = %d (%s), want 204", w.Code, w.Body.String())
	}
	if n := f.count(t, `SELECT count(*) FROM session WHERE person_id = $1`, f.personID); n != 1 {
		t.Fatalf("the fixture person's session rows = %d, want 1 (the cookie session only, nothing written)", n)
	}
}

// DELETE /v1/project takes the CURRENT project out of the account and leaves
// the rest standing. Before the projects axis this endpoint dropped the whole
// tenant, which was coherent while every plan carried one project; with
// several, a confirmation that names one domain may not take the others.
//
// And it RELEASES rather than erases: the status page keeps its slug and
// becomes ownerless (user decision, 2026-08-27), so a link somebody already
// holds still resolves and anyone may claim the page again.
func TestDeleteProjectReleasesOnlyTheCurrentOne(t *testing.T) {
	f := newProjectsFixture(t)
	ctx := context.Background()
	keep := f.projectID
	doomed := seedProject(t, f.pool, f.tenantID, "doomed.example.com")

	slug := fmt.Sprintf("doomed-%d", time.Now().UnixNano())
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title) VALUES ($1, $2, $3, 'Doomed')`,
		f.tenantID, doomed, slug); err != nil {
		t.Fatalf("seed status_page: %v", err)
	}
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec)
		 VALUES (gen_random_uuid(), $1, $2, 'website', 'Watch', $3, 300)`,
		f.tenantID, doomed, "https://doomed.example.com"); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash)
		 VALUES ($1, $2, $3, $4)`,
		f.tenantID, doomed, fmt.Sprintf("uc_live_d%d", time.Now().UnixNano()), []byte("hash")); err != nil {
		t.Fatalf("seed api_key: %v", err)
	}

	// Point the session at the second project: that is the one that goes.
	s, err := f.sess.LookupSession(ctx, f.cookie.Value)
	if err != nil {
		t.Fatalf("lookup session: %v", err)
	}
	if err := f.pool.Queries().SetSessionProject(ctx, sqlc.SetSessionProjectParams{
		ID: s.ID, ProjectID: &doomed,
	}); err != nil {
		t.Fatalf("point the session: %v", err)
	}

	w := f.do(t, "DELETE", "/v1/project", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d (%s), want 200", w.Code, w.Body.String())
	}
	var got struct {
		AccountDeleted bool `json:"accountDeleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	if got.AccountDeleted {
		t.Fatal("accountDeleted = true, but the tenant still had another project")
	}

	// Out of the account…
	if n := f.count(t, `SELECT count(*) FROM project WHERE id = $1 AND tenant_id = $2`, doomed, f.tenantID); n != 0 {
		t.Fatal("the released project is still in the account")
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE id = $1`, keep); n != 1 {
		t.Fatalf("the OTHER project was destroyed (rows = %d, want 1)", n)
	}
	if n := f.count(t, `SELECT count(*) FROM tenant WHERE id = $1`, f.tenantID); n != 1 {
		t.Fatalf("the account was closed (tenant rows = %d, want 1)", n)
	}

	// …but not erased: the page still answers at the same address, on an
	// UNCLAIMED tenant, so it can be claimed again.
	var pageTenant int64
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT tenant_id FROM status_page WHERE slug = $1`, slug).Scan(&pageTenant); err != nil {
		t.Fatalf("the status page did not survive the delete: %v", err)
	}
	if pageTenant == f.tenantID {
		t.Fatal("the released page is still owned by the account that deleted it")
	}
	if n := f.count(t,
		`SELECT count(*) FROM tenant WHERE id = $1 AND claim_token_hash IS NOT NULL`, pageTenant); n != 1 {
		t.Fatal("the released page's tenant must be claimable, or the page is stranded")
	}
	// Stopped, and stripped of the owner's credentials.
	if n := f.count(t, `SELECT count(*) FROM monitor WHERE project_id = $1 AND NOT paused`, doomed); n != 0 {
		t.Fatal("a released project must stop probing, not keep spending on somebody who left")
	}
	if n := f.count(t, `SELECT count(*) FROM api_key WHERE project_id = $1`, doomed); n != 0 {
		t.Fatal("the owner's key must not travel with the released page")
	}

	// The session's pick died with the release; the resolver falls back.
	if id := currentProjectID(ctx, f.pool, sqlc.Session{TenantID: f.tenantID}, f.tenantID); id != keep {
		t.Fatalf("resolver after the delete = %d, want the surviving %d", id, keep)
	}
}

// The LAST project still closes the account — that is the only way out — and
// its status page is released rather than cascaded away, exactly as any other
// project's is.
func TestDeleteLastProjectClosesTheAccountAndReleasesThePage(t *testing.T) {
	f := newProjectsFixture(t)
	ctx := context.Background()
	slug := fmt.Sprintf("last-%d", time.Now().UnixNano())
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title) VALUES ($1, $2, $3, 'Last')`,
		f.tenantID, f.projectID, slug); err != nil {
		t.Fatalf("seed status_page: %v", err)
	}

	w := f.do(t, "DELETE", "/v1/project", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d (%s), want 200", w.Code, w.Body.String())
	}
	var got struct {
		AccountDeleted bool `json:"accountDeleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	if !got.AccountDeleted {
		t.Fatal("accountDeleted = false, but that was the tenant's only project")
	}
	if n := f.count(t, `SELECT count(*) FROM tenant WHERE id = $1`, f.tenantID); n != 0 {
		t.Fatalf("tenant rows = %d, want 0 (the account closes with its last project)", n)
	}
	if n := f.count(t, `SELECT count(*) FROM status_page WHERE slug = $1`, slug); n != 1 {
		t.Fatal("closing the account erased the status page instead of releasing it")
	}
}
