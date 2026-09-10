//go:build integration

// The seed door (plan part 2): the node token gate, the mint shape (an
// unclaimed tenant, a page stamped minted_source='seed', a root target, no
// monitors), the reuse of an existing page, and the two ceilings.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedPost posts to the seed door with an optional Authorization header.
func seedPost(h http.Handler, host, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/internal/seed-host", strings.NewReader(`{"host":"`+host+`"}`))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSeedDoorRequiresTheNodeToken(t *testing.T) {
	_, mux, _ := newSurfacesWorld(t)
	host := fmt.Sprintf("auth-%d.example.com", time.Now().UnixNano())
	if w := seedPost(mux, host, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", w.Code)
	}
	if w := seedPost(mux, host, "Bearer wrong-token"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", w.Code)
	}
}

func TestSeedDoorMintsASeedPageWithNoMonitors(t *testing.T) {
	pool, mux, _ := newSurfacesWorld(t)
	host := fmt.Sprintf("mint-%d.example.com", time.Now().UnixNano())
	w := seedPost(mux, host, "Bearer test-node-token")
	if w.Code != http.StatusOK {
		t.Fatalf("seed = %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Slug      string `json:"slug"`
		StatusURL string `json:"statusUrl"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Slug == "" || body.StatusURL != "/status/"+body.Slug {
		t.Fatalf("seed answer = %+v", body)
	}
	ctx := context.Background()
	var source string
	var isHost bool
	var projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT minted_source, is_host_page, project_id FROM status_page WHERE slug = $1`,
		body.Slug).Scan(&source, &isHost, &projectID); err != nil {
		t.Fatalf("the seeded page row: %v", err)
	}
	if source != "seed" || !isHost {
		t.Fatalf("minted_source=%q is_host_page=%v", source, isHost)
	}
	// NO monitor rows: the page's root reference alone keeps the target due.
	var monitors int
	_ = pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM monitor WHERE project_id = $1`, projectID).Scan(&monitors)
	if monitors != 0 {
		t.Fatalf("the seed created %d monitors", monitors)
	}
	// The project's domain is the canonical host, the tenant is unclaimed.
	var domain string
	var unclaimed bool
	_ = pool.Raw().QueryRow(ctx,
		`SELECT p.domain, (t.claim_token_hash IS NOT NULL) FROM project p JOIN tenant t ON t.id = p.tenant_id WHERE p.id = $1`,
		projectID).Scan(&domain, &unclaimed)
	if domain != host || !unclaimed {
		t.Fatalf("domain=%q unclaimed=%v", domain, unclaimed)
	}
}

func TestSeedDoorReusesAnExistingPage(t *testing.T) {
	pool, mux, _ := newSurfacesWorld(t)
	host := fmt.Sprintf("reuse-%d.example.com", time.Now().UnixNano())
	first := seedHostPage(t, mux, host)
	// The second seed of the same host answers the SAME slug and creates
	// nothing, whatever way the host is spelled.
	w := seedPost(mux, "www."+host, "Bearer test-node-token")
	if w.Code != http.StatusOK {
		t.Fatalf("re-seed = %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Slug string `json:"slug"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Slug != first {
		t.Fatalf("re-seed slug = %q, want %q", body.Slug, first)
	}
	var pages int
	_ = pool.Raw().QueryRow(context.Background(),
		`SELECT count(*) FROM status_page sp JOIN project p ON p.id = sp.project_id WHERE p.domain = $1`, host).Scan(&pages)
	if pages != 1 {
		t.Fatalf("the re-seed created a second page (now %d)", pages)
	}
}

func TestSeedDoorRespectsTheInstanceMintCeiling(t *testing.T) {
	_, mux, wa := newSurfacesWorld(t)
	wa.statusKnobs.MintPerDay = 1
	wa.statusKnobs.HostPagesMax = 500
	seedHostPage(t, mux, fmt.Sprintf("cap1-%d.example.com", time.Now().UnixNano()))
	w := seedPost(mux, fmt.Sprintf("cap2-%d.example.com", time.Now().UnixNano()), "Bearer test-node-token")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the instance ceiling = %d %s, want 429", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "mint_ceiling") {
		t.Fatalf("refusal code = %s", w.Body.String())
	}
}

func TestSeedDoorRespectsTheHostPagesCeiling(t *testing.T) {
	_, mux, wa := newSurfacesWorld(t)
	wa.statusKnobs.MintPerDay = 200
	wa.statusKnobs.HostPagesMax = 1
	seedHostPage(t, mux, fmt.Sprintf("hcap1-%d.example.com", time.Now().UnixNano()))
	w := seedPost(mux, fmt.Sprintf("hcap2-%d.example.com", time.Now().UnixNano()), "Bearer test-node-token")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the host pages cap = %d %s, want 429", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "host_pages_ceiling") {
		t.Fatalf("refusal code = %s", w.Body.String())
	}
}

func TestSeedDoorRefusesBlockedHosts(t *testing.T) {
	pool, mux, _ := newSurfacesWorld(t)
	host := fmt.Sprintf("blocked-%d.example.com", time.Now().UnixNano())
	if _, err := pool.Raw().Exec(context.Background(),
		`INSERT INTO blocked_host (domain, reason) VALUES ($1, 'test')`, "example.com"); err != nil {
		t.Fatal(err)
	}
	w := seedPost(mux, host, "Bearer test-node-token")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "blocked_host") {
		t.Fatalf("blocked host = %d %s, want 403 blocked_host", w.Code, w.Body.String())
	}
}
