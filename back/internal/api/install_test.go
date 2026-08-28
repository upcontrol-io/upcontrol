package api

import (
	"net/http/httptest"
	"testing"
	"time"
)

// The full mint→ingest→status loop runs in cli/e2e; these cover the throttle
// and the pre-auth refusals.

func TestInstallAllow_CooldownPerIP(t *testing.T) {
	h := NewInstall(nil, nil, nil, "https://upcontrol.io", false)
	if !h.allow("1.2.3.4") {
		t.Fatal("first call must pass")
	}
	if h.allow("1.2.3.4") {
		t.Fatal("second call inside the cooldown must be refused")
	}
	if !h.allow("5.6.7.8") {
		t.Fatal("another IP is its own bucket")
	}
	h.mu.Lock()
	h.last["1.2.3.4"] = time.Now().Add(-anonCooldown - time.Second)
	h.mu.Unlock()
	if !h.allow("1.2.3.4") {
		t.Fatal("an expired cooldown must pass again")
	}
}

// A self-host's install command must carry --endpoint: without it the token
// travels to upcontrol.io and dies. The hosted cloud keeps the short form.
func TestInstallCommand_CarriesEndpointOffCloud(t *testing.T) {
	if got := installCommand("tok1", "https://upcontrol.io"); got != "npx upcontrol init --token tok1" {
		t.Fatalf("cloud command grew a flag it does not need: %q", got)
	}
	if got := installCommand("tok1", "https://localhost"); got != "npx upcontrol init --token tok1 --endpoint https://localhost" {
		t.Fatalf("self-host command must carry its origin: %q", got)
	}
	if got := installCommand("tok1", ""); got != "npx upcontrol init --token tok1" {
		t.Fatalf("no origin, no flag — a guessed endpoint is worse than the default: %q", got)
	}
}

func TestInstallAnonymous_SelfHostedIs404(t *testing.T) {
	// A self-host has no use-before-signup story: the anonymous mint answers
	// as if the door does not exist.
	h := NewInstall(nil, nil, nil, "", true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/projects/anonymous", nil))
	if rec.Code != 404 {
		t.Fatalf("anonymous mint on a self-host must be 404, got %d", rec.Code)
	}
}

func TestInstallStatus_MissingKeyIs401(t *testing.T) {
	h := NewInstall(nil, nil, nil, "", false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/install/status", nil))
	if rec.Code != 401 {
		t.Fatalf("status without a key must be 401, got %d", rec.Code)
	}
}

func TestInstall_UnknownPathIs404(t *testing.T) {
	h := NewInstall(nil, nil, nil, "", false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/install/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown path must be 404, got %d", rec.Code)
	}
}
