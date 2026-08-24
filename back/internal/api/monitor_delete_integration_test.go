//go:build integration

// The orphaned-incident contract (owner decision, 2026-08-24): deleting a
// check may not leave its open incident behind. incident.monitor_id is
// ON DELETE SET NULL, so the incident row survives the monitor with nothing
// left to close it — the delete handler must close it while the monitor id
// still resolves, the timeline must say what ended it ("Monitor deleted", not
// "Closed: monitor_deleted"), and the public page stops listing a chronicle
// about a component the reader can no longer see.
//
// UC_TEST_POSTGRES=postgres://... go test -tags=integration ./internal/api/...
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// orphanFixture is one account in the state the delete path has to tidy up: a
// login-role member, a website monitor, and an open availability incident on
// that monitor.
type orphanFixture struct {
	pool          *pg.Pool
	projectID     int64
	monitorPubID  string // lowercase hex, the shape the API hands the front
	incidentID    int64
	incidentTitle string
	deleteRoute   http.Handler
	cookie        *http.Cookie
}

func newOrphanFixture(t *testing.T) *orphanFixture {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping monitor-delete integration test")
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

	uniq := time.Now().UnixNano()
	title := fmt.Sprintf("Checkout down %d", uniq%100000)
	var tenantID, projectID, personID, monitorID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("orphan-%d", uniq)).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, '') RETURNING id`,
		tenantID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	// Deleting a check is a settings act: the handler refuses a notify-role
	// session before any row is touched, so the member must carry login.
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Owner') RETURNING id`,
		fmt.Sprintf("owner-%d@example.com", uniq)).Scan(&personID); err != nil {
		t.Fatalf("person: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, 'login', 'active')`,
		tenantID, personID); err != nil {
		t.Fatalf("tenant_member: %v", err)
	}
	// replace() renders the uuid the way the API returns it: lowercase hex, no
	// dashes — the shape parseUUID on the other side of the route expects.
	var pubID string
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec)
		 VALUES (gen_random_uuid(), $1, $2, 'website', 'Checkout', $3, 300)
		 RETURNING id, replace(public_id::text, '-', '')`,
		tenantID, projectID, fmt.Sprintf("https://down-%d.example.com", uniq%100000)).Scan(&monitorID, &pubID); err != nil {
		t.Fatalf("monitor: %v", err)
	}
	incidentID, created, err := incident.New(pool, nil).Open(ctx, monitorID, title)
	if err != nil || !created {
		t.Fatalf("open incident: created=%v err=%v", created, err)
	}

	sm := session.New(pool, session.DefaultTTL, nil)
	token, err := sm.Create(ctx, personID, tenantID)
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("DELETE /v1/monitors/{id}", NewMonitors(pool, sm))
	return &orphanFixture{
		pool: pool, projectID: projectID, monitorPubID: pubID,
		incidentID: incidentID, incidentTitle: title,
		deleteRoute: mux, cookie: &http.Cookie{Name: session.CookieName, Value: token},
	}
}

// deleteMonitor drives the same route the account app uses, session cookie and
// all — the point is proving the handler tidies up, not replaying its steps by
// hand in the order the handler is supposed to do them.
func (f *orphanFixture) deleteMonitor(t *testing.T) {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, "/v1/monitors/"+f.monitorPubID, nil)
	r.AddCookie(f.cookie)
	w := httptest.NewRecorder()
	f.deleteRoute.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE /v1/monitors/%s = %d (%s), want 204", f.monitorPubID, w.Code, w.Body.String())
	}
}

// publicIncidentTitles reads the public page through its route. prj-N is the
// address an account that never configured its page resolves by, so the
// fixture needs no status_page row.
func (f *orphanFixture) publicIncidentTitles(t *testing.T) []string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/public/status/prj-%d", f.projectID), nil)
	w := httptest.NewRecorder()
	(&WriteAPI{pool: f.pool}).public(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /public/status/prj-%d = %d (%s), want 200", f.projectID, w.Code, w.Body.String())
	}
	var page struct {
		Incidents []struct {
			Title string `json:"title"`
		} `json:"incidents"`
	}
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode status payload: %v", err)
	}
	titles := make([]string, 0, len(page.Incidents))
	for _, in := range page.Incidents {
		titles = append(titles, in.Title)
	}
	return titles
}

func TestDeletingAMonitorClosesItsOpenIncident(t *testing.T) {
	f := newOrphanFixture(t)
	f.deleteMonitor(t)

	ctx := context.Background()
	var status string
	// close_reason is NULL on an incident nobody closed, which is the very
	// regression this test catches — scanning it into a plain string turns that
	// failure into "cannot scan NULL", a message about pgx rather than about the
	// bug.
	var closeReason *string
	var resolved bool
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT status, close_reason, resolved_at IS NOT NULL FROM incident WHERE id = $1`,
		f.incidentID).Scan(&status, &closeReason, &resolved); err != nil {
		t.Fatalf("read incident: %v", err)
	}
	if !resolved {
		t.Fatal("resolved_at IS NULL: the incident outlived the monitor that explained it")
	}
	if status != "ok" {
		t.Fatalf("status = %q, want ok", status)
	}
	if closeReason == nil {
		t.Fatal("close_reason IS NULL: nothing closed the incident when its monitor went away")
	}
	if *closeReason != "monitor_deleted" {
		t.Fatalf("close_reason = %q, want monitor_deleted", *closeReason)
	}
	var kind, text string
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT kind, text FROM incident_update WHERE incident_id = $1 ORDER BY id DESC LIMIT 1`,
		f.incidentID).Scan(&kind, &text); err != nil {
		t.Fatalf("read newest timeline entry: %v", err)
	}
	if kind != "resolved" {
		t.Fatalf("newest timeline kind = %q, want resolved", kind)
	}
	if text != "Monitor deleted" {
		t.Fatalf("timeline text = %q, want \"Monitor deleted\" — not \"Closed: monitor_deleted\"", text)
	}
}

func TestADeletedMonitorsIncidentLeavesTheStatusPage(t *testing.T) {
	f := newOrphanFixture(t)
	// Listed before the delete, or the absence below proves nothing: a page
	// that never showed the incident and a page that stopped showing it look
	// the same.
	if !slices.Contains(f.publicIncidentTitles(t), f.incidentTitle) {
		t.Fatalf("the open incident is not on the public page before the delete; fixture is broken")
	}

	f.deleteMonitor(t)

	if slices.Contains(f.publicIncidentTitles(t), f.incidentTitle) {
		t.Fatalf("the public page still lists %q after its monitor was deleted", f.incidentTitle)
	}
}
