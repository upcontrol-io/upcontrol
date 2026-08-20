package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseLogWindow(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0}, // absent means the whole ring, which is what the window IS
		{"5m", 5 * time.Minute},
		{"1h", time.Hour},
		{"24h", 24 * time.Hour},
		{"3d", 0}, // not in the enum: ignored rather than guessed
		{"garbage", 0},
	}
	for _, c := range cases {
		if got := parseLogWindow(c.in); got != c.want {
			t.Fatalf("parseLogWindow(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The contract pins the numbers, not just the mechanics: Decision 13, the
// handler comment and the openapi description all say six per minute, so a
// change here must be a deliberate contract change, not a constant edit the
// tests silently follow.
func TestExplainThrottleContract(t *testing.T) {
	if explainBurst != 6 {
		t.Fatalf("explainBurst = %d, want 6 — the contract (Decision 13) promises six per minute", explainBurst)
	}
	if explainWindow != time.Minute {
		t.Fatalf("explainWindow = %v, want a minute — the contract (Decision 13) promises a per-minute window", explainWindow)
	}
}

// explainAllow's window mechanics, exercised by pre-seeding the map (no clock
// plumbing): aged-out slots drop before the count, the refusal path writes the
// pruned slice back, a refused burst never extends its own lockout, the window
// is per tenant, and the >512 GC pass evicts stale tenants only.
func TestExplainAllow(t *testing.T) {
	h := &WriteAPI{}
	now := time.Now()
	// slots builds a window oldest-first (the append order explainAllow
	// itself maintains) from ages given in seconds.
	slots := func(secs ...int) []time.Time {
		ts := make([]time.Time, len(secs))
		for i, s := range secs {
			ts[i] = now.Add(-time.Duration(s) * time.Second)
		}
		return ts
	}

	// A full fresh window refuses and reports the wait until its oldest slot
	// ages out (50 s old → at most 10 s left inside the minute).
	h.explainSeenAt = map[int64][]time.Time{7: slots(50, 45, 40, 35, 30, 25)}
	before := append([]time.Time(nil), h.explainSeenAt[7]...)
	ok, wait := h.explainAllow(7)
	if ok || wait <= 0 || wait > 10*time.Second {
		t.Fatalf("full window: ok = %v, wait = %v — want refused with the wait until the oldest slot releases", ok, wait)
	}
	// Refusing records nothing, so a burst of refusals never extends the
	// lockout: the window is byte-identical after twenty of them.
	for i := 0; i < 20; i++ {
		if ok, _ := h.explainAllow(7); ok {
			t.Fatalf("refusal %d admitted a full window", i+1)
		}
	}
	if got := h.explainSeenAt[7]; !reflect.DeepEqual(got, before) {
		t.Fatalf("twenty refusals moved the window: %v — a refused burst must not extend its own lockout", got)
	}

	// Aged-out slots drop before the burst count: three stale plus three
	// fresh still admits, and the stored window is the three kept plus the
	// new slot (this is the refusal path's write-back in reverse).
	h.explainSeenAt = map[int64][]time.Time{7: slots(120, 110, 100, 30, 20, 10)}
	if ok, _ := h.explainAllow(7); !ok {
		t.Fatal("three fresh slots (three aged out) were refused")
	}
	if got := len(h.explainSeenAt[7]); got != 4 {
		t.Fatalf("window holds %d slots after the admit, want 4 — three kept plus the new one", got)
	}

	// A fully aged window admits and resets to one slot.
	h.explainSeenAt = map[int64][]time.Time{7: slots(120, 110, 100, 90, 80, 70)}
	if ok, _ := h.explainAllow(7); !ok {
		t.Fatal("a fully aged-out window was refused")
	}
	if got := len(h.explainSeenAt[7]); got != 1 {
		t.Fatalf("window holds %d slots after the admit, want 1 — an expired window resets", got)
	}

	// The window is per tenant: one tenant's full window does not touch
	// another's empty one.
	h.explainSeenAt = map[int64][]time.Time{7: slots(50, 45, 40, 35, 30, 25)}
	if ok, _ := h.explainAllow(7); ok {
		t.Fatal("tenant 7 was admitted on a full window")
	}
	if ok, _ := h.explainAllow(8); !ok {
		t.Fatal("tenant 8 was refused on an empty window — the throttle is per tenant")
	}

	// Past 512 tenants the next admit GCs: entries whose newest slot is older
	// than five minutes go, fresh ones (the admit's own included) stay.
	h.explainSeenAt = map[int64][]time.Time{}
	for i := 0; i < 600; i++ {
		h.explainSeenAt[int64(i)] = slots(700) // past even the GC horizon
	}
	if ok, _ := h.explainAllow(999); !ok {
		t.Fatal("an empty window was refused")
	}
	if got := len(h.explainSeenAt); got != 1 {
		t.Fatalf("the map holds %d tenants after GC, want 1 — the 600 stale evicted, the fresh admit kept", got)
	}
	if _, ok := h.explainSeenAt[999]; !ok {
		t.Fatal("the admit's own entry was evicted by its GC pass")
	}
}

// The handler's gate placement and the 429's shape. The pool is nil on
// purpose: a request that passed both validation and the throttle would panic
// on the first read, so the 429 here proves the throttle stands before any
// database work — and the 400s prove validation stands before the throttle
// and burns no slot (a request that spends nothing is not an explain).
func TestExplainLogs_Throttle(t *testing.T) {
	h := &WriteAPI{}
	explain := func(body string, tenantID int64) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.explainLogs(w, httptest.NewRequest(http.MethodPost, "/v1/logs/explain", strings.NewReader(body)), tenantID)
		return w
	}
	// A full window, seeded rather than hammered — the mechanics are
	// TestExplainAllow's; this test pins where the gate stands.
	now := time.Now()
	ts := make([]time.Time, explainBurst)
	for i := range ts {
		ts[i] = now.Add(-time.Duration(explainBurst-i) * time.Second)
	}
	h.explainSeenAt = map[int64][]time.Time{42: ts}

	w := explain(`{"lines":["connect ECONNREFUSED"]}`, 42)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("valid request on a full window: status = %d (%s), want 429", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "explain_rate_limited") {
		t.Fatalf("429 body = %s, want the explain_rate_limited envelope", w.Body.String())
	}
	secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || secs < 1 || secs > int(explainWindow.Seconds()) {
		t.Fatalf("Retry-After = %q, want whole seconds inside the window — the shared 429 component promises the header", w.Header().Get("Retry-After"))
	}
	// Validation precedes the throttle: a request that spends nothing answers
	// 400 even on a full window, and leaves the window untouched.
	if w := explain(`{"lines":[]}`, 42); w.Code != http.StatusBadRequest {
		t.Fatalf("empty selection on a full window: status = %d (%s), want 400 — a validation failure is not an explain", w.Code, w.Body.String())
	}
	if got := len(h.explainSeenAt[42]); got != explainBurst {
		t.Fatalf("window holds %d slots after a 400, want %d — validation failures must not burn slots", got, explainBurst)
	}
	// The same is true on an empty window: no slot is created.
	h.explainSeenAt = map[int64][]time.Time{}
	if w := explain(`{"lines":[]}`, 42); w.Code != http.StatusBadRequest {
		t.Fatalf("empty selection, empty window: status = %d (%s), want 400", w.Code, w.Body.String())
	}
	if got := len(h.explainSeenAt[42]); got != 0 {
		t.Fatalf("a 400 burned a slot: %v", h.explainSeenAt[42])
	}
}
