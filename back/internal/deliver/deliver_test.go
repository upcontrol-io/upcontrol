package deliver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBackoffSchedule(t *testing.T) {
	// The base delays should be 1/2/4/8/16s. Jitter makes them slightly larger
	// but never smaller (jitter is [0, base/2)).
	for attempt := 0; attempt < 5; attempt++ {
		d := Backoff(attempt)
		base := time.Duration(1<<attempt) * time.Second
		if d < base {
			t.Errorf("Backoff(%d) = %v, want >= %v (base)", attempt, d, base)
		}
		if d > base+base/2 {
			t.Errorf("Backoff(%d) = %v, want <= %v (base + jitter)", attempt, d, base+base/2)
		}
	}
}

func TestBackoffCaps(t *testing.T) {
	// Attempt 10 should still cap at the 16s base (attempt index 4).
	d := Backoff(10)
	if d < 16*time.Second {
		t.Errorf("Backoff(10) = %v, want >= 16s", d)
	}
	if d > 24*time.Second {
		t.Errorf("Backoff(10) = %v, want <= 24s (16s + 8s jitter)", d)
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		code int
		want Outcome
	}{
		{0, OutcomeRetryable},   // network
		{200, OutcomeOK},        // success
		{400, OutcomeFatal},     // bad request
		{403, OutcomeFatal},     // bot blocked
		{404, OutcomeFatal},     // not found
		{429, OutcomeRetryable}, // rate limited
		{500, OutcomeRetryable}, // server error
		{502, OutcomeRetryable}, // bad gateway
		{503, OutcomeRetryable}, // unavailable
	}
	for _, c := range cases {
		if got := ClassifyError(c.code); got != c.want {
			t.Errorf("ClassifyError(%d) = %q, want %q", c.code, got, c.want)
		}
	}
}

func TestNextTryAtFatal(t *testing.T) {
	now := time.Now()
	got := NextTryAt(1, OutcomeFatal, now)
	if !got.IsZero() {
		t.Error("fatal should return zero time (DLQ)")
	}
}

func TestNextTryAtOK(t *testing.T) {
	now := time.Now()
	got := NextTryAt(1, OutcomeOK, now)
	if !got.IsZero() {
		t.Error("ok should return zero time (no retry)")
	}
}

func TestNextTryAtRetryable(t *testing.T) {
	now := time.Now()
	got := NextTryAt(0, OutcomeRetryable, now)
	if got.IsZero() {
		t.Fatal("retryable should return a future time")
	}
	if !got.After(now) {
		t.Error("next try should be in the future")
	}
}

func TestNextTryAtMaxAttempts(t *testing.T) {
	now := time.Now()
	got := NextTryAt(MaxAttempts, OutcomeRetryable, now)
	if !got.IsZero() {
		t.Error("max attempts should return zero time (DLQ)")
	}
}

func TestBreakerOpensAfter5Min(t *testing.T) {
	var b Breaker
	start := time.Now()
	// No failures → breaker closed.
	if b.IsOpen(start) {
		t.Error("fresh breaker should be closed")
	}
	// First failure.
	b.RecordFailure(start)
	if b.IsOpen(start) {
		t.Error("breaker should be closed immediately after first failure")
	}
	// After 5 minutes, breaker opens.
	if !b.IsOpen(start.Add(BreakerTimeout)) {
		t.Error("breaker should open after BreakerTimeout")
	}
}

func TestBreakerClosesOnSuccess(t *testing.T) {
	var b Breaker
	start := time.Now()
	b.RecordFailure(start)
	b.RecordFailure(start.Add(time.Minute))
	// Success closes it.
	b.RecordSuccess()
	if b.IsOpen(start.Add(BreakerTimeout + time.Minute)) {
		t.Error("breaker should be closed after success")
	}
}

func TestBreakerFailover(t *testing.T) {
	var b Breaker
	start := time.Now()
	b.RecordFailure(start)
	// Breaker open → failover.
	if !b.ShouldFailover(start.Add(BreakerTimeout)) {
		t.Error("should failover when breaker is open")
	}
	b.RecordSuccess()
	if b.ShouldFailover(start.Add(BreakerTimeout)) {
		t.Error("should NOT failover when breaker is closed")
	}
}

func TestBreakerStreakExtendsNotResets(t *testing.T) {
	var b Breaker
	t0 := time.Now()
	b.RecordFailure(t0)
	// Another failure at t0+1m should NOT reset the streak start.
	b.RecordFailure(t0.Add(time.Minute))
	// The breaker should open at t0+5m, not t0+1m+5m.
	if !b.IsOpen(t0.Add(BreakerTimeout)) {
		t.Error("breaker should open based on first failure, not extended streak")
	}
}

func TestFormatTelegram_CarriesTheAnswerAndTheButtons(t *testing.T) {
	// The Telegram message IS the product on a phone (§4.7): it has to carry what
	// broke, which monitor, the facts, and the buttons — a message that arrives
	// without its actions makes the reader open a laptop, which is the thing the
	// positioning is against.
	got := formatTelegram(AlertPayload{
		Status:      "down",
		Title:       "Checkout is down",
		MonitorName: "example.com/checkout",
		Fields:      map[string]string{"Region": "fra"},
		Actions:     []ActionButton{{Label: "Open incident", URL: "https://upcontrol.io/i/1"}},
	})
	for _, want := range []string{
		"<b>[down] Checkout is down</b>",
		"Monitor: example.com/checkout",
		"Region: fra",
		`<a href="https://upcontrol.io/i/1">Open incident</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("telegram message missing %q:\n%s", want, got)
		}
	}
}

// The broadcast invariant (openspec telegram-broadcast): a PERSONAL channel's
// alert carries Acknowledge/Resolve inline buttons whose callback_data is the
// incident's public id; a broadcast group's carries none — in a group the
// presser of a button cannot be name-verified, so the button must not exist.
// Captured at the HTTP boundary: whatever crosses the wire to Telegram is
// what the worker actually sends.
func TestTelegramButtonsPersonalYesBroadcastNo(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	restore := HTTPClient
	HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var v map[string]any
		_ = json.NewDecoder(req.Body).Decode(&v)
		mu.Lock()
		bodies = append(bodies, v)
		mu.Unlock()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	})}
	defer func() { HTTPClient = restore }()

	ch := &TelegramChannel{Token: func(context.Context) string { return "t" }}
	payload := AlertPayload{Title: "down", Status: "down", IncidentID: "abc123"}

	// Personal (the worker set Buttons from recipient_person_id).
	personal := payload
	personal.Buttons = true
	if _, err := ch.Send(context.Background(), "42", personal); err != nil {
		t.Fatal(err)
	}
	// Broadcast group: Buttons false.
	if _, err := ch.Send(context.Background(), "-100", payload); err != nil {
		t.Fatal(err)
	}

	if len(bodies) != 2 {
		t.Fatalf("sent %d messages, want 2", len(bodies))
	}
	markup, present := bodies[0]["reply_markup"]
	markupJSON, merr := json.Marshal(markup)
	if !present || merr != nil || !strings.Contains(string(markupJSON), "\"callback_data\":\"ack:abc123\"") {
		t.Fatalf("personal alert missing ack button: %v", bodies[0]["reply_markup"])
	}
	if _, ok := bodies[1]["reply_markup"]; ok {
		t.Fatalf("broadcast alert must carry no buttons: %v", bodies[1]["reply_markup"])
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestStatusColor_IsStatusNotSeverity(t *testing.T) {
	// Discord renders the colour, so it must follow the same three states the
	// rest of the product uses — a "check" that came out green would say the
	// opposite of the text beside it.
	if statusColor("down") == statusColor("check") || statusColor("check") == statusColor("ok") {
		t.Fatal("down / check / ok must be three distinct colours")
	}
	if statusColor("anything-else") != statusColor("ok") {
		t.Fatal("an unknown status must read as ok, never as down")
	}
}
