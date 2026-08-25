package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTrackAllowsOneBatchPerSecondPerIP(t *testing.T) {
	// The client coalesces, so one batch per second is the honest reading of
	// the 60/min budget; a same-address burst must not reach the recorder.
	h := &WriteAPI{checkSeenAt: map[string]time.Time{}}
	const ip = "203.0.113.9"
	if !h.trackAllow(ip) {
		t.Fatal("the first track batch from an address must be allowed")
	}
	if h.trackAllow(ip) {
		t.Error("a second batch inside the second must be refused")
	}
	if !h.trackAllow("198.51.100.7") {
		t.Error("a different address must not inherit the throttle")
	}
}

func TestPublicTrackMintsCookieOnceAndAlwaysAnswers204(t *testing.T) {
	// The mint door: a cookieless request gets uc_vid; a carried one must not
	// be re-rolled. nil rec/sess is the no-op contract.
	h := &WriteAPI{checkSeenAt: map[string]time.Time{}, devMode: true}

	body := `{"events":[{"name":"page_view","path":"/","props":{}}]}`
	r := httptest.NewRequest(http.MethodPost, "/public/track", strings.NewReader(body))
	r.RemoteAddr = "203.0.113.9:47000"
	w := httptest.NewRecorder()
	h.publicTrack(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("first POST = %d, want 204", w.Code)
	}
	c := w.Header().Get("Set-Cookie")
	if !strings.Contains(c, "uc_vid=") || !strings.Contains(c, "HttpOnly") ||
		!strings.Contains(c, "SameSite=Lax") || !strings.Contains(c, "Max-Age=31536000") {
		t.Fatalf("Set-Cookie = %q, want uc_vid HttpOnly SameSite=Lax Max-Age=1y", c)
	}
	token := c[strings.Index(c, "uc_vid=")+len("uc_vid="):]
	if i := strings.IndexByte(token, ';'); i >= 0 {
		token = token[:i]
	}

	// The next batch carries the cookie from a different address: no re-mint;
	// the cookie, not the IP, ties the batch to the visitor.
	r2 := httptest.NewRequest(http.MethodPost, "/public/track", strings.NewReader(body))
	r2.RemoteAddr = "198.51.100.7:47000"
	r2.AddCookie(&http.Cookie{Name: "uc_vid", Value: token})
	w2 := httptest.NewRecorder()
	h.publicTrack(w2, r2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("cookie-bearing POST = %d, want 204", w2.Code)
	}
	if got := w2.Header().Get("Set-Cookie"); got != "" {
		t.Errorf("cookie-bearing POST re-set the cookie: %q", got)
	}

	// The throttle still holds for a burst from the same address.
	r3 := httptest.NewRequest(http.MethodPost, "/public/track", strings.NewReader(body))
	r3.RemoteAddr = "203.0.113.9:47000"
	r3.AddCookie(&http.Cookie{Name: "uc_vid", Value: token})
	w3 := httptest.NewRecorder()
	h.publicTrack(w3, r3)
	if w3.Code != http.StatusTooManyRequests {
		t.Fatalf("second POST inside the second = %d, want 429", w3.Code)
	}
}
