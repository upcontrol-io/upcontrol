package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The full mint→ingest→status loop runs in cli/e2e against the real stack;
// these cover what is testable without Postgres: the per-IP throttle, the
// pre-auth refusals, and the pure meta whitelist/cap/scrub pipeline.

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

// Pinned after the cold-install rehearsal: a self-host's install command
// without --endpoint sends the token to upcontrol.io, where it dies as
// "already used or expired". The hosted cloud keeps the short command.
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
	// Decision 22 (public-first-split): a self-host has no use-before-signup
	// story — the anonymous mint answers as if the door does not exist, and a
	// bare `npx upcontrol init` cannot create an orphan tenant.
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

func TestInstallSetMeta_MissingKeyIs401(t *testing.T) {
	h := NewInstall(nil, nil, nil, "", false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PUT", "/v1/project/meta",
		strings.NewReader(`{"name":"app"}`)))
	if rec.Code != 401 {
		t.Fatalf("meta upload without a key must be 401, got %d", rec.Code)
	}
}

func TestMetaPayload(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	t.Run("stores the five fields plus provenance", func(t *testing.T) {
		payload, err := metaPayload([]byte(`{"name":"app","description":"d","framework":"next","runtime":"node v24","language":"typescript"}`), now)
		if err != nil {
			t.Fatalf("valid spec must pass: %v", err)
		}
		var stored map[string]string
		if jerr := json.Unmarshal(payload, &stored); jerr != nil {
			t.Fatalf("stored payload must be a JSON object: %v", jerr)
		}
		for _, k := range [...]string{"name", "description", "framework", "runtime", "language"} {
			if _, ok := stored[k]; !ok {
				t.Errorf("field %q must survive into storage", k)
			}
		}
		if stored["source"] != "installer" {
			t.Errorf("source must be installer, got %q", stored["source"])
		}
		if stored["collectedAt"] != "2026-08-17T12:00:00Z" {
			t.Errorf("collectedAt must be RFC3339 now, got %q", stored["collectedAt"])
		}
	})

	t.Run("drops every key outside the whitelist", func(t *testing.T) {
		payload, err := metaPayload([]byte(`{"name":"app","dependencies":{"left-pad":"1"},"git":"git@gh:me/app","path":"/home/me/app","name_length":3}`), now)
		if err != nil {
			t.Fatalf("unknown keys are dropped, not rejected: %v", err)
		}
		var stored map[string]string
		if jerr := json.Unmarshal(payload, &stored); jerr != nil {
			t.Fatalf("stored payload must be a JSON object: %v", jerr)
		}
		if len(stored) != 3 { // name + source + collectedAt
			t.Fatalf("only the whitelisted field may survive, got %v", stored)
		}
		if stored["name"] != "app" {
			t.Errorf("name must survive, got %q", stored["name"])
		}
	})

	t.Run("scrubs secrets before storage", func(t *testing.T) {
		payload, err := metaPayload([]byte(`{"description":"db is at postgres://app:hunter2@db:5432/prod"}`), now)
		if err != nil {
			t.Fatalf("a spec carrying a secret must still pass after scrubbing: %v", err)
		}
		if strings.Contains(string(payload), "hunter2") {
			t.Fatalf("the password must never reach storage: %s", payload)
		}
		if !strings.Contains(string(payload), "[redacted:") {
			t.Fatalf("the scrubber's marker must replace the secret: %s", payload)
		}
	})

	t.Run("rejects a value over the cap by naming it", func(t *testing.T) {
		body := `{"description":"` + strings.Repeat("a", 201) + `"}`
		_, err := metaPayload([]byte(body), now)
		if err == nil {
			t.Fatal("an over-cap value must be rejected, never truncated")
		}
		if err.code != "meta_too_large" {
			t.Fatalf("code must be meta_too_large, got %q", err.code)
		}
		if !strings.Contains(err.message, "description") || !strings.Contains(err.message, "200") {
			t.Fatalf("the rejection must name the field and the cap, got %q", err.message)
		}
	})

	t.Run("accepts a value at exactly the cap", func(t *testing.T) {
		body := `{"name":"` + strings.Repeat("a", 200) + `"}`
		payload, err := metaPayload([]byte(body), now)
		if err != nil {
			t.Fatalf("200 bytes is inside the cap: %v", err)
		}
		var stored map[string]string
		if jerr := json.Unmarshal(payload, &stored); jerr != nil || stored["name"] != strings.Repeat("a", 200) {
			t.Fatalf("the at-cap value must be stored verbatim: %v %v", stored, jerr)
		}
	})

	t.Run("rejects a non-string whitelisted value", func(t *testing.T) {
		_, err := metaPayload([]byte(`{"name":7}`), now)
		if err == nil || err.code != "bad_body" {
			t.Fatalf("a non-string whitelisted value must 400 as bad_body, got %v", err)
		}
	})

	t.Run("rejects a non-object body", func(t *testing.T) {
		for _, body := range []string{`[{"name":"app"}]`, `"app"`, `not json`} {
			if _, err := metaPayload([]byte(body), now); err == nil {
				t.Fatalf("body %q must be rejected", body)
			}
		}
	})

	t.Run("rejects a null body — never a silent wipe", func(t *testing.T) {
		// JSON null decodes into a map as nil with no error, so without the
		// explicit check it would store a provenance-only payload, silently
		// clearing every field the previous spec carried.
		_, err := metaPayload([]byte(`null`), now)
		if err == nil || err.code != "bad_body" {
			t.Fatalf("a null body must 400 as bad_body, got %v", err)
		}
	})

	t.Run("rejects a null field — never a silent wipe", func(t *testing.T) {
		_, err := metaPayload([]byte(`{"name":null,"framework":"next"}`), now)
		if err == nil || err.code != "bad_body" {
			t.Fatalf("a null field must 400 as bad_body, got %v", err)
		}
		if !strings.Contains(err.message, "name") {
			t.Fatalf("the rejection must name the field, got %q", err.message)
		}
	})

	t.Run("counts runes, not bytes", func(t *testing.T) {
		// 100 four-byte runes are 400 bytes: the byte cap rejected this spec,
		// the rune cap ProjectMeta's maxLength always promised accepts it.
		body := `{"name":"` + strings.Repeat("🚀", 100) + `"}`
		payload, err := metaPayload([]byte(body), now)
		if err != nil {
			t.Fatalf("400 bytes in 100 runes is inside the cap: %v", err)
		}
		var stored map[string]string
		if jerr := json.Unmarshal(payload, &stored); jerr != nil || utf8.RuneCountInString(stored["name"]) != 100 {
			t.Fatalf("the multibyte value must be stored verbatim: %v %v", stored, jerr)
		}
		over := `{"name":"` + strings.Repeat("é", 201) + `"}`
		if _, err := metaPayload([]byte(over), now); err == nil || err.code != "meta_too_large" {
			t.Fatalf("201 runes must be rejected even at 2 bytes each, got %v", err)
		}
	})

	t.Run("strips newlines at store time", func(t *testing.T) {
		// Decision 9's store-time half: a stored newline is the one byte that
		// could put a forged </project-spec> on its own line inside the fence
		// the explain prompt wraps the meta line in.
		payload, err := metaPayload([]byte(`{"name":"a\nb\r\nc"}`), now)
		if err != nil {
			t.Fatalf("newlines are stripped, not rejected: %v", err)
		}
		var stored map[string]string
		if jerr := json.Unmarshal(payload, &stored); jerr != nil || stored["name"] != "abc" {
			t.Fatalf("every newline must be gone before storage: %v %v", stored, jerr)
		}
	})

	t.Run("strips the Unicode separators and keeps the digits that spell them", func(t *testing.T) {
		// metaNewlines named U+2028/U+2029 in its comment and stripped the
		// four-digit STRINGS "2028"/"2029" instead, so a project described as
		// "Roadmap 2028" silently lost its year. Both halves are pinned here:
		// the separators go, the digits stay.
		payload, err := metaPayload([]byte(`{"name":"a b c","description":"Roadmap 2028"}`), now)
		if err != nil {
			t.Fatalf("separators are stripped, not rejected: %v", err)
		}
		var stored map[string]string
		if jerr := json.Unmarshal(payload, &stored); jerr != nil {
			t.Fatalf("unmarshal: %v", jerr)
		}
		if stored["name"] != "abc" {
			t.Errorf("U+2028/U+2029 must not survive into a prompt line: %q", stored["name"])
		}
		if stored["description"] != "Roadmap 2028" {
			t.Errorf("a year is not a line separator: %q", stored["description"])
		}
	})

	t.Run("caps the value as sent, so the scrubber's expansion cannot reject a compliant spec", func(t *testing.T) {
		// The scrubber EXPANDS (jo@ex.com, 9 runes → [redacted:email:9], 18).
		// Capping after it rejected values that were inside the published
		// 200-rune cap — a compliant client got a 400 it could not act on.
		body := `{"description":"contact jo@ex.com ` + strings.Repeat("a", 182) + `"}`
		payload, err := metaPayload([]byte(body), now)
		if err != nil {
			t.Fatalf("exactly 200 runes as sent must be accepted: %v", err)
		}
		var stored map[string]string
		if jerr := json.Unmarshal(payload, &stored); jerr != nil {
			t.Fatalf("unmarshal: %v", jerr)
		}
		if !strings.Contains(stored["description"], "[redacted:") {
			t.Errorf("the address is still scrubbed, only the cap moved: %q", stored["description"])
		}
	})
}
