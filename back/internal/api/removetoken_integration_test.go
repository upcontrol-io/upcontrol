//go:build integration

// The removal-token door (plan part 2): the per-IP window, the host-page
// gate, the 410 for removed pages, and the idempotent token whose record
// name matches the worker's dns-tokens job.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// removeToken posts to the door from one IP.
func removeToken(h http.Handler, slug, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/public/status/"+slug+"/remove-token", nil)
	if ip != "" {
		r.Header.Set("X-Forwarded-For", ip)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRemoveTokenDoorIssuesTheToken(t *testing.T) {
	pool, mux, _ := newSurfacesWorld(t)
	host := fmt.Sprintf("rm-%d.example.com", time.Now().UnixNano())
	slug := seedHostPage(t, mux, host)

	w := removeToken(mux, slug, "198.51.100.7")
	if w.Code != http.StatusOK {
		t.Fatalf("remove-token = %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Token  string `json:"token"`
		Record string `json:"record"`
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Token) != 32 {
		t.Fatalf("token = %q, want 32 hex chars", body.Token)
	}
	if body.Record != "_upcontrol-remove.example.com" || body.Domain != "example.com" {
		t.Fatalf("record/domain = %q / %q", body.Record, body.Domain)
	}
	// The token is stored: the worker's dns-tokens job reads this column.
	var stored *string
	_ = pool.Raw().QueryRow(t.Context(),
		`SELECT removal_token FROM status_page WHERE slug = $1`, slug).Scan(&stored)
	if stored == nil || *stored != body.Token {
		t.Fatalf("stored token = %v, want %q", stored, body.Token)
	}
	// Idempotent from another IP within the same window: the SAME token.
	w2 := removeToken(mux, slug, "198.51.100.8")
	if w2.Code != http.StatusOK {
		t.Fatalf("second issue = %d %s", w2.Code, w2.Body.String())
	}
	var again struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &again)
	if again.Token != body.Token {
		t.Fatalf("second issue minted a new token %q", again.Token)
	}
}

func TestRemoveTokenDoorRateLimitsPerIP(t *testing.T) {
	_, mux, _ := newSurfacesWorld(t)
	host := fmt.Sprintf("rl-%d.example.com", time.Now().UnixNano())
	slug := seedHostPage(t, mux, host)
	if w := removeToken(mux, slug, "198.51.100.9"); w.Code != http.StatusOK {
		t.Fatalf("first = %d", w.Code)
	}
	if w := removeToken(mux, slug, "198.51.100.9"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second from the same IP = %d, want 429", w.Code)
	}
}

func TestRemoveTokenDoorRefusesNonHostAndRemovedPages(t *testing.T) {
	pool, mux, _ := newSurfacesWorld(t)
	ctx := t.Context()
	host := fmt.Sprintf("nr-%d.example.com", time.Now().UnixNano())
	hostSlug := seedHostPage(t, mux, host)

	// A non-host page answers not_removable: its owner deletes their
	// project instead.
	var tenantID, projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), 'sib') RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		tenantID, host).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title, is_host_page) VALUES ($1, $2, $3, $4, false)`,
		tenantID, projectID, hostSlug+"-500", host); err != nil {
		t.Fatal(err)
	}
	w := removeToken(mux, hostSlug+"-500", "198.51.100.10")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not_removable") {
		t.Fatalf("non-host page = %d %s, want 400 not_removable", w.Code, w.Body.String())
	}

	// Unknown slug.
	if w := removeToken(mux, "no-such-page", "198.51.100.11"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d, want 404", w.Code)
	}

	// Removed page: 410 on every door.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET removed_at = now() WHERE slug = $1`, hostSlug); err != nil {
		t.Fatal(err)
	}
	if w := removeToken(mux, hostSlug, "198.51.100.12"); w.Code != http.StatusGone {
		t.Fatalf("removed = %d, want 410", w.Code)
	}
}
