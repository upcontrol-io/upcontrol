package webhook

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseEventTimestamp_PerProvider(t *testing.T) {
	// Stripe carries a unix `created`; GitHub/Vercel an RFC3339 string; each
	// must yield the event's own time, not now().
	stripe := parseEventTimestamp("stripe", map[string]any{"created": float64(1_700_000_000)})
	if want := time.Unix(1_700_000_000, 0).UTC(); !stripe.Equal(want) {
		t.Fatalf("stripe: got %v, want %v", stripe, want)
	}
	gh := parseEventTimestamp("github", map[string]any{"created_at": "2026-08-13T12:00:00Z"})
	if got := gh.UTC().Format(time.RFC3339); got != "2026-08-13T12:00:00Z" {
		t.Fatalf("github: got %s", got)
	}
	vercel := parseEventTimestamp("vercel", map[string]any{"createdAt": "2026-08-13T12:00:00Z"})
	if got := vercel.UTC().Format(time.RFC3339); got != "2026-08-13T12:00:00Z" {
		t.Fatalf("vercel: got %s", got)
	}
}

func TestParseEventTimestamp_FallsBackToNowWithoutDropping(t *testing.T) {
	// Missing/unparseable field must NOT return the zero time (which would drop
	// the event from a time-bounded correlation); it falls back to now.
	before := time.Now()
	got := parseEventTimestamp("stripe", map[string]any{}) // no `created`
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("fallback must be ~now, got %v (before=%v after=%v)", got, before, after)
	}
	// Unknown provider also falls back.
	got2 := parseEventTimestamp("unknown", map[string]any{"created_at": "not-a-date"})
	if got2.Before(before.Add(-time.Second)) {
		t.Fatalf("unknown provider must fall back to now, got %v", got2)
	}
}

func TestToUnix(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want int64
	}{
		{float64(123), 123},
		{int64(456), 456},
		{int(789), 789},
		{"x", 0},
		{nil, 0},
	} {
		if got := toUnix(tc.in); got != tc.want {
			t.Fatalf("toUnix(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseLabels_StripsLongAndNonString(t *testing.T) {
	got := parseLabels("stripe", map[string]any{
		"ok":   "short",
		"big":  string(make([]byte, 300)), // over the 256 cap → dropped
		"num":  123,                       // non-string → dropped
		"plan": "pro",
	})
	if len(got) != 2 || got["ok"] != "short" || got["plan"] != "pro" {
		t.Fatalf("long/non-string labels must be dropped; got %v", got)
	}
}

func TestDetectProvider_ByHeaders(t *testing.T) {
	for _, tc := range []struct {
		header, value, want string
	}{
		{"Stripe-Signature", "t=1,v1=abc", "stripe"},
		{"X-GitHub-Event", "push", "github"},
		{"X-Hub-Signature-256", "sha256=abc", "github"},
		{"X-Vercel-Signature", "abc", "vercel"},
		{"Content-Type", "application/json", ""},
	} {
		h := http.Header{}
		h.Set(tc.header, tc.value)
		if got := detectProvider(h); got != tc.want {
			t.Fatalf("detectProvider(%s: %s) = %q, want %q", tc.header, tc.value, got, tc.want)
		}
	}
}

func TestGenericEventName_HeaderFieldsKindFallback(t *testing.T) {
	// GitHub's push event has no `action` in the body — the header names it.
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	if got := genericEventName(h, map[string]any{}, "deployhooks"); got != "github_push" {
		t.Fatalf("header event: got %q", got)
	}
	// The fields half the webhook world uses, in order.
	if got := genericEventName(http.Header{}, map[string]any{"event": "Deploy Finished"}, ""); got != "deploy_finished" {
		t.Fatalf("event field: got %q", got)
	}
	if got := genericEventName(http.Header{}, map[string]any{"type": "build.ok"}, ""); got != "build_ok" {
		t.Fatalf("type field: got %q", got)
	}
	// Nothing nameable: the connection kind still correlates by time.
	if got := genericEventName(http.Header{}, map[string]any{}, "deployhooks"); got != "deployhooks" {
		t.Fatalf("kind fallback: got %q", got)
	}
	if got := genericEventName(http.Header{}, map[string]any{}, ""); got != "webhook" {
		t.Fatalf("last resort: got %q", got)
	}
}

func TestGenericEventID_PrefersProviderKeyThenHashes(t *testing.T) {
	h := http.Header{}
	h.Set("X-GitHub-Delivery", "gh-123")
	if got := genericEventID(h, map[string]any{"id": "body-id"}, []byte("{}")); got != "gh-123" {
		t.Fatalf("delivery header must win: got %q", got)
	}
	if got := genericEventID(http.Header{}, map[string]any{"event_id": "e-1"}, []byte("{}")); got != "e-1" {
		t.Fatalf("body id: got %q", got)
	}
	// No key anywhere: identical retries collapse, distinct payloads never do.
	a := genericEventID(http.Header{}, map[string]any{}, []byte(`{"n":1}`))
	b := genericEventID(http.Header{}, map[string]any{}, []byte(`{"n":1}`))
	c := genericEventID(http.Header{}, map[string]any{}, []byte(`{"n":2}`))
	if a != b || a == c || a == "" {
		t.Fatalf("hash dedup broken: a=%q b=%q c=%q", a, b, c)
	}
}

func TestSanitizeEventName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Deploy Finished!", "deploy_finished"},
		{"build.ok", "build_ok"},
		{"---", "webhook"},
		{"a__b", "a_b"},
	} {
		if got := sanitizeEventName(tc.in); got != tc.want {
			t.Fatalf("sanitizeEventName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := sanitizeEventName(strings.Repeat("x", 100)); len(got) != 64 {
		t.Fatalf("cap: got len %d", len(got))
	}
}

func TestGenericTimestamp_CommonFields(t *testing.T) {
	got := parseEventTimestamp("", map[string]any{"created_at": "2026-08-14T10:00:00Z"})
	if got.UTC().Format(time.RFC3339) != "2026-08-14T10:00:00Z" {
		t.Fatalf("rfc3339: got %v", got)
	}
	got = parseEventTimestamp("", map[string]any{"ts": float64(1_700_000_000)})
	if !got.Equal(time.Unix(1_700_000_000, 0).UTC()) {
		t.Fatalf("unix: got %v", got)
	}
}

func TestTokenNeverCollidesWithProviderRoute(t *testing.T) {
	// The routing rule: a known provider name goes to the legacy path, anything
	// else is a token. A minted token must therefore never look like one.
	for name := range knownProviders {
		if len(name) == 32 {
			t.Fatalf("provider %q is shaped like a token", name)
		}
	}
}
