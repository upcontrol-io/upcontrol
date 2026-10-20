//go:build integration

// Product alert rules against a real database: the CRUD round trip through
// the session doors, the workspace's count and its wall, the lock that keeps
// concurrent creates inside it, the freeze a downgrade computes on read, and
// the plan read. Run with -tags=integration and UC_TEST_POSTGRES set.

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

const silenceRule = `{"event":"signup","kind":"silence","windowHours":6}`

type productAlertOut struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Event       string            `json:"event"`
	Labels      map[string]string `json:"labels"`
	Kind        string            `json:"kind"`
	Direction   string            `json:"direction"`
	Percent     int               `json:"percent"`
	WindowHours int               `json:"windowHours"`
	Paused      bool              `json:"paused"`
	State       string            `json:"state"`
	LastFiredAt string            `json:"lastFiredAt"`
}

func listProductAlertsT(t *testing.T, h *writeAPI) []productAlertOut {
	t.Helper()
	code, body := sessionCall(t, h, http.MethodGet, "/v1/product-alerts", "")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/product-alerts = %d %s", code, body)
	}
	var out []productAlertOut
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the list must be an array of rules: %v (%s)", err, body)
	}
	return out
}

func createProductAlertT(t *testing.T, h *writeAPI, body string) productAlertOut {
	t.Helper()
	code, resp := sessionCall(t, h, http.MethodPost, "/v1/product-alerts", body)
	if code != http.StatusCreated {
		t.Fatalf("POST /v1/product-alerts = %d %s", code, resp)
	}
	var out productAlertOut
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out.ID == "" {
		t.Fatalf("a create answers the rule: %v (%s)", err, resp)
	}
	return out
}

func TestProductAlertsRoundTrip(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Indie", 1)
	h := planTenantAPI(t, pool, tenantID)

	a := createProductAlertT(t, h, `{"event":"signup","kind":"change","direction":"down","percent":40,"windowHours":3,"labels":{"plan":"pro"}}`)
	if a.State != "learning" || a.Direction != "down" || a.Percent != 40 || a.Labels["plan"] != "pro" {
		t.Fatalf("the new rule starts learning with what was sent; got %+v", a)
	}
	if list := listProductAlertsT(t, h); len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("the list holds the one rule; got %+v", list)
	}

	// A rename keeps the episode; a change to what it measures resets it.
	if _, err := pool.Raw().Exec(t.Context(),
		`UPDATE product_alert SET state = 'firing', last_fired_at = now() WHERE tenant_id = $1`, tenantID); err != nil {
		t.Fatalf("put the rule in an episode: %v", err)
	}
	code, body := sessionCall(t, h, http.MethodPatch, "/v1/product-alerts/"+a.ID, `{"name":"Signups"}`)
	var got productAlertOut
	_ = json.Unmarshal([]byte(body), &got)
	if code != http.StatusOK || got.Name != "Signups" || got.State != "firing" || got.LastFiredAt == "" {
		t.Fatalf("a rename keeps the episode = %d %s", code, body)
	}
	code, body = sessionCall(t, h, http.MethodPatch, "/v1/product-alerts/"+a.ID, `{"percent":80}`)
	got = productAlertOut{}
	_ = json.Unmarshal([]byte(body), &got)
	if code != http.StatusOK || got.Percent != 80 || got.State != "learning" || got.LastFiredAt != "" {
		t.Fatalf("a changed threshold resets the episode = %d %s", code, body)
	}
	// A pause closes the episode, so a resume never sends a stale recovery.
	if _, err := pool.Raw().Exec(t.Context(),
		`UPDATE product_alert SET state = 'firing', last_fired_at = now() WHERE tenant_id = $1`, tenantID); err != nil {
		t.Fatalf("put the rule in an episode: %v", err)
	}
	code, body = sessionCall(t, h, http.MethodPatch, "/v1/product-alerts/"+a.ID, `{"paused":true}`)
	got = productAlertOut{}
	_ = json.Unmarshal([]byte(body), &got)
	if code != http.StatusOK || got.State != "paused" || !got.Paused || got.LastFiredAt != "" {
		t.Fatalf("a paused rule reads as paused = %d %s", code, body)
	}
	var stored string
	if err := pool.Raw().QueryRow(t.Context(),
		`SELECT state FROM product_alert WHERE tenant_id = $1`, tenantID).Scan(&stored); err != nil || stored != "learning" {
		t.Fatalf("a paused firing rule is stored %q (err %v), want learning", stored, err)
	}
	if code, body := sessionCall(t, h, http.MethodPatch, "/v1/product-alerts/"+a.ID, `{"currency":"USD"}`); code != http.StatusBadRequest {
		t.Fatalf("a currency without measure = %d %s", code, body)
	}

	// Another workspace's rule is not this project's.
	other := planTenantAPI(t, pool, seedPlanTenant(t, pool, "Indie", 1))
	if code, body := sessionCall(t, other, http.MethodPatch, "/v1/product-alerts/"+a.ID, `{"name":"x"}`); code != http.StatusNotFound {
		t.Fatalf("PATCH from another workspace = %d %s", code, body)
	}
	if code, body := sessionCall(t, other, http.MethodDelete, "/v1/product-alerts/"+a.ID, ""); code != http.StatusNotFound {
		t.Fatalf("DELETE from another workspace = %d %s", code, body)
	}

	if code, body := sessionCall(t, h, http.MethodDelete, "/v1/product-alerts/"+a.ID, ""); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", code, body)
	}
	if list := listProductAlertsT(t, h); len(list) != 0 {
		t.Fatalf("the deleted rule is gone; got %+v", list)
	}
}

func TestFreesThirdProductAlertIsTheWall(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := planTenantAPI(t, pool, tenantID)
	createProductAlertT(t, h, silenceRule)
	createProductAlertT(t, h, silenceRule)
	code, body := sessionCall(t, h, http.MethodPost, "/v1/product-alerts", silenceRule)
	var wall struct {
		Error struct {
			Upgrade struct{ Reason, Plan string } `json:"upgrade"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &wall)
	if code != http.StatusPaymentRequired ||
		wall.Error.Upgrade.Reason != "Your plan carries 2 product alerts. Indie carries them without a limit." ||
		wall.Error.Upgrade.Plan != "indie" {
		t.Fatalf("Free's third rule = %d %s", code, body)
	}
}

func TestConcurrentProductAlertCreatesStayInsideTheCount(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := planTenantAPI(t, pool, tenantID)
	var codes [8]int
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i], _ = sessionCall(t, h, http.MethodPost, "/v1/product-alerts", silenceRule)
		})
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		} else if c != http.StatusPaymentRequired {
			t.Fatalf("every create lands or meets the wall; got %v", codes)
		}
	}
	if created != 2 {
		t.Fatalf("eight concurrent creates on Free land exactly two; got %d (%v)", created, codes)
	}
}

func TestADowngradeFreezesTheNewestProductAlert(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Indie", 1)
	h := planTenantAPI(t, pool, tenantID)
	for range 3 {
		createProductAlertT(t, h, silenceRule)
	}
	plan := func() map[string]json.RawMessage {
		ra := &readAPI{pool: pool, sess: h.sess}
		w := httptest.NewRecorder()
		ra.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/plan", nil))
		var out map[string]json.RawMessage
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("GET /v1/plan = %d %s", w.Code, w.Body.String())
		}
		return out
	}
	if _, ok := plan()["productAlerts"]; ok {
		t.Fatal("an unlimited plan reports no productAlerts")
	}

	setPlan(t, pool, tenantID, "Free")
	list := listProductAlertsT(t, h)
	if len(list) != 3 || list[0].State == "frozen" || list[1].State == "frozen" || list[2].State != "frozen" {
		t.Fatalf("on Free the newest of three reads frozen; got %+v", list)
	}
	var axis struct{ Used, Max int }
	if err := json.Unmarshal(plan()["productAlerts"], &axis); err != nil || axis.Used != 3 || axis.Max != 2 {
		t.Fatalf("Free reports productAlerts {used:3, max:2}; got %+v (%v)", axis, err)
	}
}
