//go:build integration

// The /v1/me fallback join (docs/plans/projects-axis.md T7; the [T1+T2]
// Critic finding): a session whose pick was deleted or reparented away still
// answers with the tenant's lowest-id project; a zero-project tenant answers
// `"project":null` on the wire (Decision 15) while the query row keeps NULL
// project columns; DELETE FROM project nulls session.project_id (the FK's ON
// DELETE SET NULL). Run: go test -tags=integration ./internal/account/auth/...
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// meFixture: a signed-in tenant whose Provision-seeded project is the tenant's
// min(id), plus two higher-id projects to point the session at.
type meFixture struct {
	pool          *pg.Pool
	personID      int64
	tenantID      int64
	firstDomain   string
	midID         int64
	midDomain     string
	topID         int64
	topDomain     string
	sm            *session.Manager
	token         string
	foreignTenant int64
}

func newMeFixture(t *testing.T) *meFixture {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping /v1/me project integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "", "", "", "", "../../../../db/postgres", ""); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	uniq := time.Now().UnixNano()
	f := &meFixture{pool: pool}
	f.firstDomain = fmt.Sprintf("first-%d.example.com", uniq%100000)
	f.personID, f.tenantID, err = Provision(ctx, pool,
		fmt.Sprintf("mefallback-%d@example.com", uniq), f.firstDomain, nil, false)
	if err != nil || f.tenantID == 0 {
		t.Fatalf("provision: tenantID=%d err=%v", f.tenantID, err)
	}
	seed := func(domain string) int64 {
		var id int64
		if err := pool.Raw().QueryRow(ctx,
			`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
			f.tenantID, domain).Scan(&id); err != nil {
			t.Fatalf("seed project %q: %v", domain, err)
		}
		return id
	}
	f.midDomain = fmt.Sprintf("mid-%d.example.com", uniq%100000)
	f.topDomain = fmt.Sprintf("top-%d.example.com", uniq%100000)
	f.midID = seed(f.midDomain) // ids only ever ascend, so first < mid < top
	f.topID = seed(f.topDomain)

	f.sm = session.New(pool, session.DefaultTTL, nil)
	f.token, err = f.sm.Create(ctx, f.personID, f.tenantID)
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}

	// The tenant a claim would reparent to: distinct id, no shared rows.
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("foreign-%d", uniq)).Scan(&f.foreignTenant); err != nil {
		t.Fatalf("seed foreign tenant: %v", err)
	}
	return f
}

// me calls the real handler with the fixture's session cookie.
func (f *meFixture) me(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/me", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: f.token})
	rec := httptest.NewRecorder()
	NewMe(f.pool, f.sm).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/v1/me = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /v1/me body %q: %v", rec.Body.String(), err)
	}
	return body
}

func meProjectDomain(t *testing.T, body map[string]json.RawMessage) string {
	t.Helper()
	var p struct {
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(body["project"], &p); err != nil {
		t.Fatalf("decode project %s: %v", body["project"], err)
	}
	return p.Domain
}

func (f *meFixture) setPick(t *testing.T, projectID int64) {
	t.Helper()
	if _, err := f.pool.Raw().Exec(context.Background(),
		`UPDATE session SET project_id = $1 WHERE person_id = $2 AND tenant_id = $3`,
		projectID, f.personID, f.tenantID); err != nil {
		t.Fatalf("point session at project %d: %v", projectID, err)
	}
}

func TestMeProjectFallbackJoin(t *testing.T) {
	f := newMeFixture(t)

	// A fresh session carries no pick: the join's fallback arm answers min(id).
	if d := meProjectDomain(t, f.me(t)); d != f.firstDomain {
		t.Fatalf("unset pick answered %q, want the min-id project %q", d, f.firstDomain)
	}

	// The pick is honoured while the project belongs to the tenant.
	f.setPick(t, f.topID)
	if d := meProjectDomain(t, f.me(t)); d != f.topDomain {
		t.Fatalf("session pick answered %q, want the picked project %q", d, f.topDomain)
	}

	// Reparent the pick away (the post-claim state): the id exists but no
	// longer belongs to this tenant, so the join falls back to min(id).
	if _, err := f.pool.Raw().Exec(context.Background(),
		`UPDATE project SET tenant_id = $1 WHERE id = $2`, f.foreignTenant, f.topID); err != nil {
		t.Fatalf("reparent top project: %v", err)
	}
	if d := meProjectDomain(t, f.me(t)); d != f.firstDomain {
		t.Fatalf("reparented pick answered %q, want the min-id fallback %q", d, f.firstDomain)
	}

	// Deleting the picked project nulls session.project_id (FK ON DELETE SET
	// NULL) and the join falls back to min(id) again.
	f.setPick(t, f.midID)
	if d := meProjectDomain(t, f.me(t)); d != f.midDomain {
		t.Fatalf("session pick answered %q, want the picked project %q", d, f.midDomain)
	}
	if _, err := f.pool.Raw().Exec(context.Background(),
		`DELETE FROM project WHERE id = $1`, f.midID); err != nil {
		t.Fatalf("delete picked project: %v", err)
	}
	s, err := f.sm.LookupSession(context.Background(), f.token)
	if err != nil {
		t.Fatalf("re-read session: %v", err)
	}
	if s.ProjectID != nil {
		t.Fatalf("session.project_id = %d after its project was deleted, want NULL (ON DELETE SET NULL)", *s.ProjectID)
	}
	if d := meProjectDomain(t, f.me(t)); d != f.firstDomain {
		t.Fatalf("deleted pick answered %q, want the min-id fallback %q", d, f.firstDomain)
	}
}

func TestMeZeroProjectsAnswersNullProject(t *testing.T) {
	f := newMeFixture(t)

	// The tenant's one project gone (cascades its api_key, project_seq): the
	// LEFT JOIN yields the NULL-project row on BOTH doors.
	if _, err := f.pool.Raw().Exec(context.Background(),
		`DELETE FROM project WHERE tenant_id = $1`, f.tenantID); err != nil {
		t.Fatalf("delete the last project: %v", err)
	}

	hash := sha256.Sum256([]byte(f.token))
	row, err := f.pool.Queries().GetMe(context.Background(), hash[:])
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if row.ProjectID != nil || row.ProjectPublicID.Valid || row.ProjectDomain != nil {
		t.Fatalf("zero-project GetMe row = %+v, want NULL project columns", row)
	}
	byID, err := f.pool.Queries().GetMeByIdentity(context.Background(), sqlc.GetMeByIdentityParams{
		PersonID: f.personID, TenantID: f.tenantID,
	})
	if err != nil {
		t.Fatalf("GetMeByIdentity: %v", err)
	}
	if byID.ProjectID != nil || byID.ProjectPublicID.Valid || byID.ProjectDomain != nil {
		t.Fatalf("zero-project GetMeByIdentity row = %+v, want NULL project columns", byID)
	}

	// On the wire the handler answers "project":null (Decision 15), not a
	// zero-valued object — cookie door first, then the identity conversion.
	if body := f.me(t); string(body["project"]) != "null" {
		t.Fatalf(`/v1/me project = %s, want null`, body["project"])
	}
	rec := httptest.NewRecorder()
	sm := session.New(f.pool, 0, nil).WithFixedIdentity(f.personID, f.tenantID)
	NewMe(f.pool, sm).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/me", nil))
	if rec.Code != 200 {
		t.Fatalf("identity /v1/me = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("identity /v1/me body is not JSON: %s", rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode identity /v1/me body: %v", err)
	}
	if string(body["project"]) != "null" {
		t.Fatalf(`identity /v1/me project = %s, want null (the GetMeByIdentity conversion must emit nil too)`, body["project"])
	}
}
