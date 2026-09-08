package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/ingest/decode"
)

// stubKeys resolves every key to a fixed tenant, the public-gate tests' keyring.
type stubKeys struct {
	tenant Tenant
}

func (s *stubKeys) Resolve(_ context.Context, _ string) (Tenant, error) {
	return s.tenant, nil
}

func publicTenant() Tenant {
	return Tenant{
		TenantID:  7,
		ProjectID: 9,
		Kind:      KeyKindPublic,
		Origins:   []string{"https://example.com"},
	}
}

func newPublicIngester(tn Tenant) (*Ingester, *fakeSink) {
	sink := &fakeSink{}
	return New(Deps{
		Keys:  &stubKeys{tenant: tn},
		Seq:   &fakeSeq{},
		Sink:  sink,
		Idem:  newFakeIdem(),
		Spool: &fakeSpool{pct: 0},
	}), sink
}

func TestLiftActor(t *testing.T) {
	attrs := map[string]string{"uc.actor": "  user@example.com  ", "keep": "me"}
	if got := liftActor(attrs); got != "user@example.com" {
		t.Errorf("liftActor = %q, want trimmed address", got)
	}
	if _, ok := attrs["uc.actor"]; ok {
		t.Error("uc.actor survived the lift")
	}
	if attrs["keep"] != "me" {
		t.Error("lift touched a neighbouring attr")
	}

	// Whitespace-only is a real nobody, not an error.
	if got := liftActor(map[string]string{"uc.actor": "   "}); got != "" {
		t.Errorf("whitespace-only actor = %q, want \"\"", got)
	}
	if got := liftActor(map[string]string{}); got != "" {
		t.Errorf("absent actor = %q, want \"\"", got)
	}
	if got := liftActor(nil); got != "" {
		t.Errorf("nil attrs actor = %q, want \"\"", got)
	}

	// Over-cap is truncated, never refused.
	long := strings.Repeat("a", MaxActorBytes+50)
	if got := liftActor(map[string]string{"uc.actor": long}); len(got) != MaxActorBytes {
		t.Errorf("actor len = %d, want the %d cap", len(got), MaxActorBytes)
	}
}

func TestMatchOrigin(t *testing.T) {
	origins := []string{"https://example.com", "https://app.example.org"}
	if got := matchOrigin("https://example.com", origins); got != "https://example.com" {
		t.Errorf("exact match = %q", got)
	}
	for _, bad := range []string{
		"",                                 // absent
		"https://evil.com",                 // stranger
		"https://app.example.org.evil.com", // suffix trick
		"http://example.com",               // scheme fold
		"example.com",                      // scheme-less
	} {
		if got := matchOrigin(bad, origins); got != "" {
			t.Errorf("matchOrigin(%q) = %q, want \"\"", bad, got)
		}
	}
	if got := matchOrigin("https://example.com", nil); got != "" {
		t.Error("an empty origins list must be a refusal, never any")
	}
}

func TestOnlyEvents(t *testing.T) {
	recs := []decode.Record{
		{Message: "signup", Named: true},
		{Message: "just a log line"},
		{Message: `{"metric":"cpu","value":1}`},
		{Message: "deploy v2"}, // textual fallback still names an event
	}
	ws := newWarningAccumulator()
	kept := onlyEvents(recs, ws)
	if len(kept) != 2 {
		t.Fatalf("kept %d records, want the two that name events", len(kept))
	}
	if kept[0].Message != "signup" || kept[1].Message != "deploy v2" {
		t.Errorf("kept the wrong records: %q, %q", kept[0].Message, kept[1].Message)
	}
	if ws.counts["public_key_logs_refused"] != 2 {
		t.Errorf("refused tally = %d, want 2", ws.counts["public_key_logs_refused"])
	}
}

func TestRateLimiterWindow(t *testing.T) {
	l := newRateLimiter()
	now := time.Now()
	for i := 0; i < publicRateMax; i++ {
		if !l.allow("k", "1.2.3.4", now) {
			t.Fatalf("refused hit %d inside the window", i+1)
		}
	}
	if l.allow("k", "1.2.3.4", now) {
		t.Error("allowed past the window budget")
	}
	// A different IP has its own bucket; the same pair resets next window.
	if !l.allow("k", "5.6.7.8", now) {
		t.Error("the limit is per key AND per IP")
	}
	if !l.allow("k", "1.2.3.4", now.Add(publicRateWindow)) {
		t.Error("the window did not roll over")
	}
}

func TestHandlePublicKeyOriginGate(t *testing.T) {
	ing, _ := newPublicIngester(publicTenant())

	// Absent Origin and a stranger Origin are the same 401, never a hint.
	for name, origin := range map[string]string{"absent": "", "stranger": "https://evil.com"} {
		headers := map[string]string{"X-Upcontrol-Key": "uc_pub_abc"}
		if origin != "" {
			headers["Origin"] = origin
		}
		rr := post(t, ing, `{"msg":"signup","uc.event":true}`, headers)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s origin: status %d, want 401", name, rr.Code)
		}
		if rr.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s origin: CORS header on a refusal", name)
		}
	}
}

func TestHandlePublicKeyAcceptsEventsOnly(t *testing.T) {
	ing, sink := newPublicIngester(publicTenant())
	body := "{\"msg\":\"signup\",\"uc.event\":true,\"uc.actor\":\" user@example.com \"}\n{\"msg\":\"just a log line\"}"
	rr := post(t, ing, body, map[string]string{
		"X-Upcontrol-Key": "uc_pub_abc",
		"Origin":          "https://example.com",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("ACAO = %q, want the matched origin echoed", got)
	}
	if rr.Header().Get("Vary") != "Origin" {
		t.Error("missing Vary: Origin")
	}
	var rec Receipt
	if err := json.Unmarshal(rr.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Accepted != 1 {
		t.Errorf("accepted %d, want 1 (the log line refused)", rec.Accepted)
	}
	var refused int
	for _, w := range rec.Warnings {
		if w.Code == "public_key_logs_refused" {
			refused = w.Count
		}
	}
	if refused != 1 {
		t.Errorf("public_key_logs_refused = %d, want 1", refused)
	}
	if len(sink.rows) != 1 {
		t.Fatalf("%d rows sank, want 1", len(sink.rows))
	}
	var env RowEnvelope
	if err := json.Unmarshal(sink.rows[0], &env); err != nil {
		t.Fatal(err)
	}
	if env.Event != "signup" || env.Actor != "user@example.com" {
		t.Errorf("event=%q actor=%q, want signup + the trimmed actor", env.Event, env.Actor)
	}
	if _, ok := env.Attrs["uc.actor"]; ok {
		t.Error("uc.actor survived into the stored attrs")
	}
}

func TestHandlePublicKeyRateLimit(t *testing.T) {
	ing, _ := newPublicIngester(publicTenant())
	headers := map[string]string{
		"X-Upcontrol-Key": "uc_pub_abc",
		"Origin":          "https://example.com",
	}
	for i := 0; i < publicRateMax; i++ {
		body := "{\"msg\":\"hit\",\"uc.event\":true,\"i\":" + strconv.Itoa(i) + "}"
		if rr := post(t, ing, body, headers); rr.Code != http.StatusOK {
			t.Fatalf("hit %d: status %d, want 200", i+1, rr.Code)
		}
	}
	rr := post(t, ing, `{"msg":"hit","uc.event":true,"i":999}`, headers)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}

func TestHandlePreflight(t *testing.T) {
	ing, _ := newPublicIngester(publicTenant())

	req := httptest.NewRequest(http.MethodOptions, "/i?key=uc_pub_abc", nil)
	req.Header.Set("Origin", "https://example.com")
	rr := httptest.NewRecorder()
	ing.HandlePreflight(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("ACAO = %q, want the echoed origin", got)
	}
	if rr.Header().Get("Access-Control-Allow-Headers") == "" || rr.Header().Get("Access-Control-Max-Age") == "" {
		t.Error("preflight missing allow-headers or max-age")
	}

	// No key on the query string: a bare 204 with no CORS headers, so the
	// browser never fires the POST.
	req = httptest.NewRequest(http.MethodOptions, "/i", nil)
	req.Header.Set("Origin", "https://example.com")
	rr = httptest.NewRecorder()
	ing.HandlePreflight(rr, req)
	if rr.Code != http.StatusNoContent || rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("no key: status %d ACAO %q, want a bare 204", rr.Code, rr.Header().Get("Access-Control-Allow-Origin"))
	}

	// A secret key needs no CORS and gets none.
	secretIng, _ := newPublicIngester(Tenant{TenantID: 7, ProjectID: 9})
	req = httptest.NewRequest(http.MethodOptions, "/i?key=uc_live_xyz", nil)
	req.Header.Set("Origin", "https://example.com")
	rr = httptest.NewRecorder()
	secretIng.HandlePreflight(rr, req)
	if rr.Code != http.StatusNoContent || rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("secret key: status %d ACAO %q, want a bare 204", rr.Code, rr.Header().Get("Access-Control-Allow-Origin"))
	}
}
