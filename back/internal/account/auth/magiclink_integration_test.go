//go:build integration

// The dev half of the magic-link contract (task 12): a configured mailer IS
// called in dev, and a SendCode failure NEVER blocks the dev_token response.
// Without this pin, a later "fix" that errors the response on a failed send
// would lock out every dev login and still pass the unit suite, because the
// unit tests only cover the pure helpers.
//
// UC_TEST_POSTGRES=postgres://... go test -tags=integration ./internal/account/auth/...
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
