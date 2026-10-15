//go:build integration

// The sign-in door's owner rules (2026-10-15): what a sign-in was started for
// is kept with its code on the server, answered once by the redeem, and never
// mailed or logged; account_created is the first sign-in that proves the
// address, by link or by Google, never the request. Shared DSN like
// magiclink_integration_test.go. Run: go test -tags=integration ./internal/account/auth/...
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/analytics"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// recordingMailer delivers nothing and keeps every argument it was handed:
// whatever a mail could carry passes through here.
type recordingMailer struct {
	codes []string
	args  []string
}

func (m *recordingMailer) SendCode(_ context.Context, email, code string) error {
	m.codes = append(m.codes, code)
	m.args = append(m.args, email, code)
	return nil
}

func (m *recordingMailer) SendInvite(_ context.Context, to, code, project, invitedBy string) error {
	m.args = append(m.args, to, code, project, invitedBy)
	return nil
}

func growthPool(t *testing.T) *pg.Pool {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping sign-in integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "../../../../db/postgres"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Every request rides its own client IP: the per-IP window is shared with
// every other run of this suite.
var growthIP atomic.Int64

func init() { growthIP.Store(time.Now().UnixNano()) }

func signIn(h http.Handler, body string) *httptest.ResponseRecorder {
	seq := growthIP.Add(1)
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/magic-link", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-For", fmt.Sprintf("172.%d.%d.%d", byte(seq>>16), byte(seq>>8), byte(seq)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// storedClaim reads the claim columns of email's code row.
func storedClaim(t *testing.T, pool *pg.Pool, email string) (slug, token, next *string) {
	t.Helper()
	if err := pool.Raw().QueryRow(context.Background(),
		`SELECT claim_slug, claim_token, claim_next FROM magic_link_code WHERE email = $1`, email).Scan(&slug, &token, &next); err != nil {
		t.Fatalf("read the code row: %v", err)
	}
	return slug, token, next
}

func devToken(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		DevToken string `json:"dev_token"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp.DevToken == "" {
		t.Fatalf("request = %d (%s), want 202 with a dev_token", w.Code, w.Body.String())
	}
	return resp.DevToken
}

// C1, C2, T2: the request keeps slug, token and next with the code; the mail
// and the operator's log never see the token; the redeem answers the claim
// once and clears it.
func TestGrowthSignInKeepsTheClaimWithTheCode(t *testing.T) {
	pool := growthPool(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	mail := &recordingMailer{}
	sm := session.New(pool, session.DefaultTTL, nil)
	// Prod: the code travels by mail and never in the response.
	h := NewMagicLink(pool, sm, false, mail, nil, logger)
	uniq := time.Now().UnixNano()
	email := fmt.Sprintf("claim-%d@example.com", uniq)
	token := fmt.Sprintf("%032x", uniq)

	w := signIn(h, fmt.Sprintf(`{"email":%q,"claim":"example-com","claimToken":%q,"next":"/app/alerts"}`, email, token))
	if w.Code != http.StatusAccepted {
		t.Fatalf("request = %d (%s), want 202", w.Code, w.Body.String())
	}
	if len(mail.codes) != 1 {
		t.Fatalf("mails sent = %d, want 1", len(mail.codes))
	}
	for _, arg := range mail.args {
		if strings.Contains(arg, token) {
			t.Fatalf("the claim token rode the mail: %q", arg)
		}
	}
	if slug, tok, next := storedClaim(t, pool, email); slug == nil || *slug != "example-com" ||
		tok == nil || *tok != token || next == nil || *next != "/app/alerts" {
		t.Fatalf("stored claim = (%v, %v, %v), want the request's three fields", slug, tok, next)
	}

	w = signIn(h, fmt.Sprintf(`{"email":%q,"token":%q}`, email, mail.codes[0]))
	if w.Code != http.StatusOK {
		t.Fatalf("redeem = %d (%s), want 200", w.Code, w.Body.String())
	}
	var redeemed struct {
		Email        string `json:"email"`
		PendingClaim *struct {
			Slug       string `json:"slug"`
			ClaimToken string `json:"claimToken"`
			Next       string `json:"next"`
		} `json:"pendingClaim"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &redeemed); err != nil || redeemed.Email != email || redeemed.PendingClaim == nil {
		t.Fatalf("redeem answered %s (err %v), want the account and its pendingClaim", w.Body.String(), err)
	}
	if p := redeemed.PendingClaim; p.Slug != "example-com" || p.ClaimToken != token || p.Next != "/app/alerts" {
		t.Fatalf("pendingClaim = %+v, want what the request was started for", *p)
	}
	if slug, tok, next := storedClaim(t, pool, email); slug != nil || tok != nil || next != nil {
		t.Fatalf("after the redeem the code row still holds (%v, %v, %v), want it cleared", slug, tok, next)
	}

	// With no mailer, prod logs the code for the operator: never the token.
	quiet := NewMagicLink(pool, sm, false, nil, nil, logger)
	if w := signIn(quiet, fmt.Sprintf(`{"email":"claim-quiet-%d@example.com","claimToken":%q}`, uniq, token)); w.Code != http.StatusAccepted {
		t.Fatalf("no-mailer request = %d (%s), want 202", w.Code, w.Body.String())
	}
	if !strings.Contains(logs.String(), "no mailer") {
		t.Fatalf("the operator's code line was not logged: %s", logs.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("the claim token reached the log: %s", logs.String())
	}
}

// C10: a field of the wrong shape is dropped, never refused, and a code
// carries only what ITS request asked for: a later request without a claim
// clears an earlier one's.
func TestGrowthSignInDropsBadShapesAndStaleClaims(t *testing.T) {
	pool := growthPool(t)
	h := NewMagicLink(pool, session.New(pool, session.DefaultTTL, nil), true, nil, nil, slog.New(slog.DiscardHandler))
	uniq := time.Now().UnixNano()
	plainRedeem := func(email, code string) {
		t.Helper()
		w := signIn(h, fmt.Sprintf(`{"email":%q,"token":%q}`, email, code))
		var body map[string]any
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("redeem = %d (%s), want 200", w.Code, w.Body.String())
		}
		if _, ok := body["pendingClaim"]; ok {
			t.Fatalf("redeem answered pendingClaim %v, want none", body["pendingClaim"])
		}
	}

	bad := fmt.Sprintf("bad-shape-%d@example.com", uniq)
	code := devToken(t, signIn(h, fmt.Sprintf(
		`{"email":%q,"claim":"Example.com","claimToken":"XYZ","next":"https://evil.example/app"}`, bad)))
	if slug, tok, next := storedClaim(t, pool, bad); slug != nil || tok != nil || next != nil {
		t.Fatalf("badly shaped fields stored as (%v, %v, %v), want none", slug, tok, next)
	}
	plainRedeem(bad, code)

	stale := fmt.Sprintf("stale-claim-%d@example.com", uniq)
	devToken(t, signIn(h, fmt.Sprintf(`{"email":%q,"claim":"stale-page"}`, stale)))
	if _, err := pool.Raw().Exec(context.Background(),
		`UPDATE magic_link_code SET created_at = now() - interval '2 minutes' WHERE email = $1`, stale); err != nil {
		t.Fatalf("backdate the code's cooldown: %v", err)
	}
	code = devToken(t, signIn(h, fmt.Sprintf(`{"email":%q}`, stale)))
	if slug, _, _ := storedClaim(t, pool, stale); slug != nil {
		t.Fatalf("a plain request kept the earlier claim %q", *slug)
	}
	plainRedeem(stale, code)
}

// M3: account_created is the first redeem that proves the address. The
// request proves nothing and a second sign-in is not a second account.
func TestGrowthAccountCreatedAtTheFirstVerifiedSignInOnly(t *testing.T) {
	pool := growthPool(t)
	ctx := context.Background()
	rec := analytics.NewRecorder(analytics.PoolStore{Pool: pool}, pgstore.New(pool.Raw()), nil)
	rec.Start()
	t.Cleanup(func() { _ = rec.Stop(context.Background()) })
	h := NewMagicLink(pool, session.New(pool, session.DefaultTTL, nil), true, nil, rec, slog.New(slog.DiscardHandler))
	email := fmt.Sprintf("born-%d@example.com", time.Now().UnixNano())

	code := devToken(t, signIn(h, fmt.Sprintf(`{"email":%q}`, email)))
	var personID int64
	var verified *time.Time
	if err := pool.Raw().QueryRow(ctx,
		`SELECT id, email_verified_at FROM person WHERE email = $1`, email).Scan(&personID, &verified); err != nil {
		t.Fatalf("the requested person: %v", err)
	}
	if verified != nil {
		t.Fatalf("a bare request stamped email_verified_at %v, want none: it proves nothing", verified)
	}
	for i := range 2 {
		if i > 0 {
			if _, err := pool.Raw().Exec(ctx,
				`UPDATE magic_link_code SET created_at = now() - interval '2 minutes' WHERE email = $1`, email); err != nil {
				t.Fatal(err)
			}
			code = devToken(t, signIn(h, fmt.Sprintf(`{"email":%q}`, email)))
		}
		if w := signIn(h, fmt.Sprintf(`{"email":%q,"token":%q}`, email, code)); w.Code != http.StatusOK {
			t.Fatalf("redeem %d = %d (%s), want 200", i, w.Code, w.Body.String())
		}
	}
	if err := pool.Raw().QueryRow(ctx, `SELECT email_verified_at FROM person WHERE id = $1`, personID).Scan(&verified); err != nil || verified == nil {
		t.Fatalf("after the redeem email_verified_at = %v (err %v), want stamped", verified, err)
	}

	if err := rec.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	count := func(name string) int64 {
		var n int64
		if err := pool.Raw().QueryRow(ctx,
			`SELECT count(*) FROM web_events WHERE person_id = $1 AND name = $2`, personID, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("account_created"); n != 1 {
		t.Fatalf("account_created rows = %d, want 1 (the first redeem only)", n)
	}
	if n := count("signed_in"); n != 2 {
		t.Fatalf("signed_in rows = %d, want 2", n)
	}
}

// M3: Google verified the address, so a person's first Google sign-in is the
// account's birth too, and the second is not.
func TestGrowthGoogleFirstSignInIsTheAccountsBirth(t *testing.T) {
	pool := growthPool(t)
	ctx := context.Background()
	rec := analytics.NewRecorder(analytics.PoolStore{Pool: pool}, pgstore.New(pool.Raw()), nil)
	rec.Start()
	t.Cleanup(func() { _ = rec.Stop(context.Background()) })
	email := fmt.Sprintf("google-born-%d@example.com", time.Now().UnixNano())
	claims := liveClaims()
	claims["email"] = email
	signed := idToken(t, claims)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": signed})
	}))
	t.Cleanup(endpoint.Close)
	g := NewGoogle(pool, session.New(pool, session.DefaultTTL, nil), testClientID, "secret",
		[]string{"http://localhost/sign-in"}, true, rec, discardLogger())
	g.tokenURL = endpoint.URL

	for i := range 2 {
		if rr := postGoogle(g, `{"code":"c","redirect_uri":"http://localhost/sign-in"}`); rr.Code != http.StatusOK {
			t.Fatalf("google sign-in %d = %d (%s), want 200", i, rr.Code, rr.Body.String())
		}
	}
	var personID int64
	var verified *time.Time
	if err := pool.Raw().QueryRow(ctx,
		`SELECT id, email_verified_at FROM person WHERE email = $1`, email).Scan(&personID, &verified); err != nil || verified == nil {
		t.Fatalf("the Google person's email_verified_at = %v (err %v), want stamped", verified, err)
	}
	if err := rec.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM web_events WHERE person_id = $1 AND name = 'account_created'`, personID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("account_created rows = %d (err %v), want 1", n, err)
	}
}
