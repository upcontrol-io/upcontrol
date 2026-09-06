//go:build integration

// The stored board: GET and PUT /v1/dashboard against a real database. The
// handlers are driven directly, with no session manager — a request without a
// cookie resolves to the tenant's own lowest project, which is the single
// project each case seeds. Run with -tags=integration and UC_TEST_POSTGRES set.

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	apigen "go.upcontrol.io/back/gen/api"
	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/storage/pg"
)

// sameBoard compares two answers as documents, not as bytes: the column is
// jsonb, so postgres hands the layout back in its own key order and a
// byte comparison would be testing postgres's spelling rather than the board.
func sameBoard(t *testing.T, got, want string) bool {
	t.Helper()
	var a, b apigen.DashboardLayout
	if err := json.Unmarshal([]byte(got), &a); err != nil {
		t.Fatalf("the answer must be a layout: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("the expectation must be a layout: %v (%s)", err, want)
	}
	return reflect.DeepEqual(a, b)
}

// boardOf is the tenant's only project, the one currentProject resolves to.
func boardOf(t *testing.T, pool *pg.Pool, tenantID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.Raw().QueryRow(t.Context(),
		`SELECT min(id) FROM project WHERE tenant_id = $1`, tenantID).Scan(&id); err != nil {
		t.Fatalf("read the tenant's project: %v", err)
	}
	return id
}

func getBoard(t *testing.T, h *writeAPI, tenantID int64) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.getDashboard(w, httptest.NewRequest(http.MethodGet, "/v1/dashboard", nil), tenantID)
	return w.Code, strings.TrimSpace(w.Body.String())
}

func putBoard(t *testing.T, h *writeAPI, tenantID int64, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/v1/dashboard", bytes.NewBufferString(body))
	h.putDashboard(w, r, tenantID)
	return w.Code, strings.TrimSpace(w.Body.String())
}

const oneWidgetBoard = `{"version":1,"widgets":[{"id":"w_1","kind":"line","title":"Errors by level",` +
	`"metrics":[{"source":"logs","where":{"service":"api","level":"error"}}],` +
	`"range":"24h","x":0,"y":0,"w":6,"h":4}]}`

func TestDashboardRoundTrip(t *testing.T) {
	pool := openProjectsGateDB(t)
	h := &writeAPI{pool: pool}
	tenantID := seedPlanTenant(t, pool, "Free", 1)

	// A project that never saved a board answers the empty layout, not a 404:
	// the front reads a 404 as "this core has no such endpoint".
	if code, body := getBoard(t, h, tenantID); code != http.StatusOK || body != string(emptyLayout) {
		t.Fatalf("a board that was never saved reads as %d %s", code, body)
	}

	code, body := putBoard(t, h, tenantID, oneWidgetBoard)
	if code != http.StatusOK || !sameBoard(t, body, oneWidgetBoard) {
		t.Fatalf("PUT answers the layout as stored; got %d %s", code, body)
	}
	if code, got := getBoard(t, h, tenantID); code != http.StatusOK || !sameBoard(t, got, oneWidgetBoard) {
		t.Fatalf("GET must hand back the document PUT stored; got %d %s", code, got)
	}

	// Last write wins, and it replaces rather than merges: the first board's
	// widget may not survive a PUT that does not mention it.
	if code, got := putBoard(t, h, tenantID, `{"version":1,"widgets":[]}`); code != http.StatusOK {
		t.Fatalf("the second PUT = %d %s", code, got)
	}
	if code, got := getBoard(t, h, tenantID); code != http.StatusOK || !sameBoard(t, got, string(emptyLayout)) {
		t.Fatalf("the second PUT replaces the first; got %d %s", code, got)
	}
}

// A tenant with no project yet has nowhere to put a board, but it still has a
// board to read: the read stays an empty layout, because the front reads a 404
// there as "this core has no such endpoint" and falls back to the browser.
func TestDashboardWithoutAProject(t *testing.T) {
	pool := openProjectsGateDB(t)
	h := &writeAPI{pool: pool}
	tenantID := seedPlanTenant(t, pool, "Free", 0)
	if code, body := getBoard(t, h, tenantID); code != http.StatusOK || body != string(emptyLayout) {
		t.Fatalf("a tenant with no project still reads an empty board; got %d %s", code, body)
	}
	if code, body := putBoard(t, h, tenantID, oneWidgetBoard); code != http.StatusNotFound {
		t.Fatalf("a board needs a project to belong to; got %d %s", code, body)
	}
}

func TestDashboardRefusesABodyPastTheCap(t *testing.T) {
	pool := openProjectsGateDB(t)
	h := &writeAPI{pool: pool}
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	big := `{"version":1,"widgets":[],"pad":"` + strings.Repeat("x", dashboardMaxBody) + `"}`
	code, body := putBoard(t, h, tenantID, big)
	if code != http.StatusBadRequest || !strings.Contains(body, "bad_body") {
		t.Fatalf("a body past %d bytes is a 400 bad_body; got %d %s", dashboardMaxBody, code, body)
	}
	if code, got := getBoard(t, h, tenantID); got != string(emptyLayout) {
		t.Fatalf("a refused write stores nothing; got %d %s", code, got)
	}
}

func TestDashboardRefusesABadEnvelope(t *testing.T) {
	pool := openProjectsGateDB(t)
	h := &writeAPI{pool: pool}
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	code, body := putBoard(t, h, tenantID,
		`{"version":1,"widgets":[{"id":"w_1","kind":"line","title":"x","metrics":[],"x":7,"y":0,"w":6,"h":4}]}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "bad_layout") {
		t.Fatalf("a widget past the 12 columns is a 400 bad_layout; got %d %s", code, body)
	}
}

// The scope is the project, and the project is the caller's own: currentProject
// resolves the session's tenant's project, so one tenant's board is never the
// other's, and the read still carries the tenant so a stale row reads as none.
func TestDashboardIsScopedToItsTenant(t *testing.T) {
	pool := openProjectsGateDB(t)
	h := &writeAPI{pool: pool}
	tenantA := seedPlanTenant(t, pool, "Free", 1)
	tenantB := seedPlanTenant(t, pool, "Free", 1)
	projectA := boardOf(t, pool, tenantA)

	if code, body := putBoard(t, h, tenantA, oneWidgetBoard); code != http.StatusOK {
		t.Fatalf("tenant A's PUT = %d %s", code, body)
	}
	if code, body := putBoard(t, h, tenantB, `{"version":1,"widgets":[]}`); code != http.StatusOK {
		t.Fatalf("tenant B's PUT = %d %s", code, body)
	}
	if code, body := getBoard(t, h, tenantA); code != http.StatusOK || !sameBoard(t, body, oneWidgetBoard) {
		t.Fatalf("tenant B's save lands on B's project, never A's; A now reads %d %s", code, body)
	}
	if _, err := pool.Queries().GetDashboard(t.Context(), sqlc.GetDashboardParams{
		TenantID: tenantB, ProjectID: projectA,
	}); err == nil {
		t.Fatal("tenant B reading tenant A's project id must find no row")
	}
}

// A project can change tenants: releaseProject hands it to an orphan tenant
// and a claim hands it on. The board goes with it, the way its monitors do,
// and even a row the move missed is taken over by the new owner's first save
// rather than kept while that save answers 200.
func TestDashboardFollowsTheProject(t *testing.T) {
	pool := openProjectsGateDB(t)
	h := &writeAPI{pool: pool}
	tenantA := seedPlanTenant(t, pool, "Free", 1)
	tenantB := seedPlanTenant(t, pool, "Free", 0)
	project := boardOf(t, pool, tenantA)
	if code, body := putBoard(t, h, tenantA, oneWidgetBoard); code != http.StatusOK {
		t.Fatalf("tenant A's PUT = %d %s", code, body)
	}

	// The move, as releaseProject and the claim absorb do it: project first,
	// then every table on the reparent list, dashboard among them.
	for _, table := range [...]string{"project", "dashboard"} {
		key := "project_id"
		if table == "project" {
			key = "id"
		}
		if _, err := pool.Raw().Exec(t.Context(),
			`UPDATE `+table+` SET tenant_id = $1 WHERE `+key+` = $2`, tenantB, project); err != nil {
			t.Fatalf("move %s: %v", table, err)
		}
	}
	if code, body := getBoard(t, h, tenantB); code != http.StatusOK || !sameBoard(t, body, oneWidgetBoard) {
		t.Fatalf("the board moves with the project; B reads %d %s", code, body)
	}

	// A row the move missed: the project is B's, the board row still says A.
	if _, err := pool.Raw().Exec(t.Context(),
		`UPDATE dashboard SET tenant_id = $1 WHERE project_id = $2`, tenantA, project); err != nil {
		t.Fatalf("strand the row: %v", err)
	}
	if code, body := getBoard(t, h, tenantB); code != http.StatusOK || body != string(emptyLayout) {
		t.Fatalf("a stranded row reads as no board; B reads %d %s", code, body)
	}
	if code, body := putBoard(t, h, tenantB, `{"version":1,"widgets":[]}`); code != http.StatusOK {
		t.Fatalf("B's save over a stranded row = %d %s", code, body)
	}
	if code, body := getBoard(t, h, tenantB); code != http.StatusOK || !sameBoard(t, body, string(emptyLayout)) {
		t.Fatalf("B's save must take the row over; B reads %d %s", code, body)
	}
}
