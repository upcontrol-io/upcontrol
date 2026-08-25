package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFromRequest_FixedIdentity(t *testing.T) {
	// Single-user mode: a cookieless request carries the boot identity;
	// without WithFixedIdentity the same request is refused.
	m := New(nil, 0, nil).WithFixedIdentity(7, 9)
	s, err := m.FromRequest(context.Background(), httptest.NewRequest("GET", "/v1/me", nil))
	if err != nil {
		t.Fatalf("fixed identity must never refuse: %v", err)
	}
	if s.PersonID != 7 || s.TenantID != 9 {
		t.Fatalf("synthetic session must carry the boot identity, got %+v", s)
	}

	plain := New(nil, 0, nil)
	if _, err := plain.FromRequest(context.Background(), httptest.NewRequest("GET", "/v1/me", nil)); !errors.Is(err, ErrNoSession) {
		t.Fatalf("without fixed identity a cookieless request must be ErrNoSession, got %v", err)
	}
}

func TestSetCookie_Shape(t *testing.T) {
	// The session cookie is the auth boundary: HttpOnly, Secure, SameSite=Lax,
	// Path=/, raw token value. A regression here is a real auth risk.
	rec := httptest.NewRecorder()
	SetCookie(rec, "rawtoken", 30*time.Minute, true)
	resp := rec.Result()
	defer resp.Body.Close()

	cks := resp.Cookies()
	if len(cks) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cks))
	}
	c := cks[0]
	if c.Name != CookieName {
		t.Fatalf("cookie name %q, want %q", c.Name, CookieName)
	}
	if c.Value != "rawtoken" {
		t.Fatalf("cookie must carry the raw token; got %q", c.Value)
	}
	if !c.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Error("cookie must be Secure (HTTPS only)")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Error("cookie must be SameSite=Lax")
	}
	if c.Path != "/" {
		t.Errorf("cookie path %q, want /", c.Path)
	}
}

func TestClearCookie_ExpiresImmediately(t *testing.T) {
	rec := httptest.NewRecorder()
	ClearCookie(rec)
	resp := rec.Result()
	defer resp.Body.Close()
	c := resp.Cookies()
	if len(c) != 1 || c[0].MaxAge >= 0 {
		t.Fatalf("clear cookie must have MaxAge<0 (immediate expiry); got %+v", c)
	}
}

func TestRandomToken_EntropyAndLength(t *testing.T) {
	// 32 bytes hex = 64 chars. Collisions across many draws would mean a broken
	// RNG — and a stolen-session risk if two users shared a token hash.
	seen := map[string]bool{}
	for i := 0; i < 5000; i++ {
		tok := randomToken()
		if len(tok) != 64 {
			t.Fatalf("token %q is %d chars, want 64", tok, len(tok))
		}
		if seen[tok] {
			t.Fatalf("token collision after %d draws", i)
		}
		seen[tok] = true
	}
}
