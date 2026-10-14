//go:build integration

// GET /v1/dashboard/marks against a real database, driven through ServeHTTP
// over the owner's fixed identity: routes_test reads main.go as text, so only a
// request through the session switch proves the door answers rather than
// falling through to its 404. Run with -tags=integration and UC_TEST_POSTGRES
// set.

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apigen "go.upcontrol.io/back/gen/api"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

func TestTheMarksDoorAnswersThroughTheSessionRoute(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := planTenantAPI(t, pool, tenantID)
	h.pgs = pgstore.New(pool.Raw())
	projectID := boardOf(t, pool, tenantID)
	now := time.Now().UTC()

	if err := h.pgs.InsertEvents(t.Context(), []pgstore.EventRow{
		{TenantID: uint64(tenantID), ProjectID: uint64(projectID), TS: now.Add(-time.Hour), Name: "deploy",
			Labels: map[string]string{"sha": "0123456789abcdef0123456789abcdef01234567"}},
		{TenantID: uint64(tenantID), ProjectID: uint64(projectID), TS: now.Add(-2 * time.Hour), Name: "app_started",
			Labels: map[string]string{"version": "1.4.2"}},
	}); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	if _, err := pool.Raw().Exec(t.Context(),
		`INSERT INTO incident (public_id, tenant_id, project_id, detector, fingerprint, title, status, detected_at)
		 VALUES (gen_random_uuid(), $1, $2, 'availability', 1, 'Checkout is down', 'down', $3)`,
		tenantID, projectID, now.Add(-3*time.Hour)); err != nil {
		t.Fatalf("seed incident: %v", err)
	}

	// The slot is for the reads, not for a client slow to take the body.
	w := &slotsAtAnswer{ResponseRecorder: httptest.NewRecorder(), held: -1}
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/dashboard/marks?range=24h", nil))
	code, body := w.Code, strings.TrimSpace(w.Body.String())
	if w.held != 0 {
		t.Fatalf("%d read slots still held when the answer was written, want none", w.held)
	}
	if code != http.StatusOK {
		t.Fatalf("GET /v1/dashboard/marks = %d %s", code, body)
	}
	var got apigen.DashboardMarks
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("the answer must be DashboardMarks: %v (%s)", err, body)
	}
	if len(got.Marks) != 3 ||
		got.Marks[0].Kind != apigen.DashboardMarkKindIncident || got.Marks[0].Label != "Checkout is down" || got.Marks[0].Until != nil ||
		got.Marks[1].Kind != apigen.DashboardMarkKindRelease || got.Marks[1].Label != "1.4.2" ||
		got.Marks[2].Kind != apigen.DashboardMarkKindDeploy || got.Marks[2].Label != "0123456" {
		t.Fatalf("the open incident, the release, then the deploy by its short sha; got %s", body)
	}

	// A range the board does not know is a 400, and so is none at all.
	for _, path := range []string{"/v1/dashboard/marks?range=2h", "/v1/dashboard/marks"} {
		if code, body := sessionCall(t, h, http.MethodGet, path, ""); code != http.StatusBadRequest || !strings.Contains(body, "bad_query") {
			t.Fatalf("GET %s = %d %s, want 400 bad_query", path, code, body)
		}
	}
	// Gated like /v1/series: a week is past Free's day of history.
	if code, body := sessionCall(t, h, http.MethodGet, "/v1/dashboard/marks?range=7d", ""); code != http.StatusPaymentRequired ||
		!strings.Contains(body, `"plan":"indie"`) {
		t.Fatalf("a week on Free = %d %s, want the 402 naming Indie", code, body)
	}
}
