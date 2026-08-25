//go:build integration

// A SendCode failure never blocks the dev_token response; only the redeem
// activates invites. Run: go test -tags=integration ./internal/account/auth/...
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// failingMailer is a mailer that is down: it records the one code it was
// handed and always fails, like an email agent that is unreachable.
type failingMailer struct {
	calls int
	email string
	code  string
}

func (m *failingMailer) SendCode(_ context.Context, email, code string) error {
	m.calls++
	m.email, m.code = email, code
	return errors.New("agent down")
}

// The invitation carries the same code the sign-in door mints, so a mailer
// that is down fails it the same way.
func (m *failingMailer) SendInvite(_ context.Context, to, code, _, _ string) error {
	m.calls++
	m.email, m.code = to, code
	return errors.New("agent down")
}

func TestDevModeFailingSendStillReturnsDevToken(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping magic-link integration test")
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

	// A unique address per run: the database may hold earlier runs' accounts.
	email := fmt.Sprintf("devsend-%d@example.com", time.Now().UnixNano())
	mailer := &failingMailer{}
	h := NewMagicLink(pool, session.New(pool, session.DefaultTTL, nil), true, mailer, nil,
		slog.New(slog.DiscardHandler))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/magic-link",
		strings.NewReader(`{"email":"`+email+`"}`))
	// What the page sends, and what the cross-site guard requires.
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202: a failed send must not error the dev response",
			w.Code, w.Body.String())
	}
	var resp struct {
		DevToken string `json:"dev_token"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DevToken == "" {
		t.Fatal("dev_token missing: dev log-in without an inbox is impossible")
	}
	if mailer.calls != 1 {
		t.Fatalf("SendCode called %d times, want exactly 1", mailer.calls)
	}
	if mailer.email != email {
		t.Fatalf("mailer saw email %q, want %q", mailer.email, email)
	}
	if resp.DevToken != mailer.code {
		t.Fatalf("dev_token %q is not the code the mailer was handed (%q)", resp.DevToken, mailer.code)
	}
}

// Activation is proof of ownership only: the request proves nothing, the
// redeem activates and seeds. A second sign-in adds no second channel.
func TestRedeemActivatesInviteAndSeedsEmailChannel(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping magic-link integration test")
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

	// Tenant A and its owner, the recipients harness's shape.
	var tenantID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("redeem-activate-%d", time.Now().UnixNano())).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var ownerID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'owner') RETURNING id`,
		fmt.Sprintf("owner-%d@example.com", time.Now().UnixNano())).Scan(&ownerID); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, 'login', 'active')`,
		tenantID, ownerID); err != nil {
		t.Fatalf("seed owner membership: %v", err)
	}

	// The invited teammate: a person whose membership in tenant A is pending.
	// A unique address per run — the database holds earlier runs' accounts.
	email := fmt.Sprintf("invitee-%d@example.com", time.Now().UnixNano())
	var personID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'invitee') RETURNING id`,
		email).Scan(&personID); err != nil {
		t.Fatalf("seed invitee: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, 'notify', 'pending')`,
		tenantID, personID); err != nil {
		t.Fatalf("seed pending membership: %v", err)
	}

	// No mailer needed: dev mode rides the code on the response.
	h := NewMagicLink(pool, session.New(pool, session.DefaultTTL, nil), true, nil, nil,
		slog.New(slog.DiscardHandler))

	// Every request rides its own client IP: the per-IP window is shared with
	// every other run of this suite.
	seq := time.Now().UnixNano()
	post := func(body string) *httptest.ResponseRecorder {
		seq++
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/auth/magic-link", strings.NewReader(body))
		// What the page sends, and what the cross-site guard requires.
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("10.%d.%d.%d", byte(seq>>16), byte(seq>>8), byte(seq)))
		h.ServeHTTP(w, r)
		return w
	}
	memberStatus := func() string {
		var status string
		if err := pool.Raw().QueryRow(ctx,
			`SELECT status FROM tenant_member WHERE tenant_id = $1 AND person_id = $2`, tenantID, personID).Scan(&status); err != nil {
			t.Fatalf("read membership: %v", err)
		}
		return status
	}
	channels := func() int {
		var n int
		if err := pool.Raw().QueryRow(ctx,
			`SELECT count(*) FROM alert_channel WHERE tenant_id = $1 AND kind = 'email' AND target = $2`,
			tenantID, email).Scan(&n); err != nil {
			t.Fatalf("count channels: %v", err)
		}
		return n
	}

	// Request: dev mode hands back the code — and must activate nothing.
	w := post(`{"email":"` + email + `"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("request: status = %d (%s), want 202", w.Code, w.Body.String())
	}
	var resp struct {
		DevToken string `json:"dev_token"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode request response: %v", err)
	}
	if resp.DevToken == "" {
		t.Fatal("dev_token missing: dev mode must return the code")
	}
	if s := memberStatus(); s != "pending" {
		t.Fatalf("after a bare request the membership is %q, want pending — a request proves no ownership (Decision 18)", s)
	}

	// Redeem: the one step that proves the address is owned.
	w = post(`{"email":"` + email + `","token":"` + resp.DevToken + `"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("redeem: status = %d (%s), want 200", w.Code, w.Body.String())
	}
	if s := memberStatus(); s != "active" {
		t.Fatalf("after the redeem the membership is %q, want active", s)
	}
	if n := channels(); n != 1 {
		t.Fatalf("email channels for the invitee = %d, want exactly 1", n)
	}

	// A second sign-in mints a fresh code; the cooldown is backdated first.
	// The point under test is idempotence across sign-ins, not the throttle.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE magic_link_code SET created_at = now() - interval '2 minutes' WHERE email = $1`, email); err != nil {
		t.Fatalf("backdate the code's cooldown: %v", err)
	}
	w = post(`{"email":"` + email + `"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("second request: status = %d (%s), want 202", w.Code, w.Body.String())
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode second request response: %v", err)
	}
	w = post(`{"email":"` + email + `","token":"` + resp.DevToken + `"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("second redeem: status = %d (%s), want 200", w.Code, w.Body.String())
	}
	if s := memberStatus(); s != "active" {
		t.Fatalf("after the second redeem the membership is %q, want active", s)
	}
	if n := channels(); n != 1 {
		t.Fatalf("a second sign-in seeded a duplicate: %d email channels, want 1", n)
	}
}
