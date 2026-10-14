//go:build integration

// One team per project: a login member edits and deletes its own project's
// checks, and a sibling project's check in the same workspace reads as unknown
// to both doors. Run with -tags=integration, UC_TEST_POSTGRES set.
package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// seedScopedMonitor subscribes one project to a fresh website target and
// answers the check's wire id, its row id and its target's id. probe_target.key
// is globally unique and the shared test database survives runs, hence the
// nanosecond key.
func seedScopedMonitor(t *testing.T, pool *pg.Pool, tenantID, projectID int64, name string) (string, int64, int64) {
	t.Helper()
	url := fmt.Sprintf("https://scope-%d-%d.example.com", projectID, time.Now().UnixNano())
	var pubID string
	var monID, targetID int64
	if err := pool.Raw().QueryRow(context.Background(),
		`WITH tgt AS (
		   INSERT INTO probe_target (key, kind, url) VALUES ($3, 'website', $3) RETURNING id)
		 INSERT INTO monitor (public_id, tenant_id, project_id, target_id, kind, name, target, interval_sec)
		 SELECT gen_random_uuid(), $1, $2, tgt.id, 'website', $4, $3, 300 FROM tgt
		 RETURNING replace(public_id::text, '-', ''), id, target_id`,
		tenantID, projectID, url, name).Scan(&pubID, &monID, &targetID); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	return pubID, monID, targetID
}

func TestMonitorsByIDStayInTheCurrentProject(t *testing.T) {
	pool := openProjectsGateDB(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	owner := seedPerson(t, pool, fmt.Sprintf("scope-owner-%d@example.com", uniq))
	tenantID := seedOwnedTenant(t, pool, owner, fmt.Sprintf("scope-%d", uniq))
	projectA := seedProject(t, pool, tenantID, fmt.Sprintf("a-%d.example.com", uniq))
	projectB := seedProject(t, pool, tenantID, fmt.Sprintf("b-%d.example.com", uniq))
	// An Admin of A alone: the role that edits checks, on one team only.
	admin := seedPerson(t, pool, fmt.Sprintf("scope-admin-%d@example.com", uniq))
	seedProjectMember(t, pool, projectA, admin, tenantID, "login")

	sm := session.New(pool, session.DefaultTTL, nil)
	token, err := sm.Create(ctx, admin, tenantID, &projectA)
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	mon := NewMonitors(pool, pgstore.New(pool.Raw()), sm, "")
	mux := http.NewServeMux()
	mux.Handle("PATCH /v1/monitors/{id}", mon)
	mux.Handle("DELETE /v1/monitors/{id}", mon)
	do := func(method, id, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/monitors/"+id, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	// B's check carries an open incident, because closing it is the first write
	// a DELETE makes.
	theirs, theirsID, _ := seedScopedMonitor(t, pool, tenantID, projectB, "Theirs")
	incidentID, created, err := incident.New(pool, nil).Open(ctx, theirsID, "Theirs down", 0)
	if err != nil || !created {
		t.Fatalf("open incident: created=%v err=%v", created, err)
	}
	if w := do(http.MethodPatch, theirs, `{"name":"Hijacked","paused":true}`); w.Code != http.StatusNotFound ||
		!strings.Contains(w.Body.String(), "not_found") {
		t.Fatalf("PATCH a sibling project's check = %d (%s), want 404 not_found", w.Code, w.Body.String())
	}
	// DELETE answers a sibling's id the way it answers an unknown one, 204 with
	// nothing done, so the answer does not reveal that the id exists elsewhere.
	if w := do(http.MethodDelete, theirs, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE a sibling project's check = %d (%s), want 204 like an unknown id", w.Code, w.Body.String())
	}
	var name string
	var paused, open bool
	if err := pool.Raw().QueryRow(ctx,
		`SELECT m.name, m.paused, i.resolved_at IS NULL
		   FROM monitor m JOIN incident i ON i.monitor_id = m.id
		  WHERE m.id = $1 AND i.id = $2`, theirsID, incidentID).Scan(&name, &paused, &open); err != nil {
		t.Fatalf("the sibling's check is gone after the refused DELETE: %v", err)
	}
	if name != "Theirs" || paused || !open {
		t.Fatalf("sibling's check name=%q paused=%v incident open=%v, want Theirs, running, open", name, paused, open)
	}

	// The same session still edits and deletes a check of its own project.
	mine, mineID, _ := seedScopedMonitor(t, pool, tenantID, projectA, "Mine")
	if w := do(http.MethodPatch, mine, `{"name":"Renamed","paused":true}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"name":"Renamed"`) || !strings.Contains(w.Body.String(), `"paused":true`) {
		t.Fatalf("PATCH own check = %d (%s), want 200 renamed and paused", w.Code, w.Body.String())
	}
	if w := do(http.MethodDelete, mine, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE own check = %d (%s), want 204", w.Code, w.Body.String())
	}
	var n int
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM monitor WHERE id = $1`, mineID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("own check after DELETE: count=%d err=%v, want 0", n, err)
	}
}
