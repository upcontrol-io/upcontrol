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
		d := backoff(attempt)
		base := time.Duration(1<<attempt) * time.Second
		if d < base {
			t.Errorf("backoff(%d) = %v, want >= %v (base)", attempt, d, base)
		}
		if d > base+base/2 {
			t.Errorf("backoff(%d) = %v, want <= %v (base + jitter)", attempt, d, base+base/2)
		}
	}
}

func TestBackoffCaps(t *testing.T) {
	// Attempt 10 should still cap at the 16s base (attempt index 4).
	d := backoff(10)
	if d < 16*time.Second {
		t.Errorf("backoff(10) = %v, want >= 16s", d)
	}
	if d > 24*time.Second {
		t.Errorf("backoff(10) = %v, want <= 24s (16s + 8s jitter)", d)
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		code int
		want outcome
	}{
		{0, outcomeRetryable},   // network
		{200, outcomeOK},        // success
		{400, outcomeFatal},     // bad request
		{403, outcomeFatal},     // bot blocked
		{404, outcomeFatal},     // not found
		{429, outcomeRetryable}, // rate limited
		{500, outcomeRetryable}, // server error
		{502, outcomeRetryable}, // bad gateway
		{503, outcomeRetryable}, // unavailable
	}
	for _, c := range cases {
		if got := classifyError(c.code); got != c.want {
			t.Errorf("classifyError(%d) = %q, want %q", c.code, got, c.want)
		}
	}
}

func TestNextTryAtFatal(t *testing.T) {
	now := time.Now()
	got := nextTryAt(1, outcomeFatal, now)
	if !got.IsZero() {
		t.Error("fatal should return zero time (DLQ)")
	}
}

func TestNextTryAtOK(t *testing.T) {
	now := time.Now()
	got := nextTryAt(1, outcomeOK, now)
	if !got.IsZero() {
		t.Error("ok should return zero time (no retry)")
	}
}

func TestNextTryAtRetryable(t *testing.T) {
	now := time.Now()
	got := nextTryAt(0, outcomeRetryable, now)
	if got.IsZero() {
		t.Fatal("retryable should return a future time")
	}
	if !got.After(now) {
		t.Error("next try should be in the future")
	}
}

func TestNextTryAtMaxAttempts(t *testing.T) {
	now := time.Now()
	got := nextTryAt(maxAttempts, outcomeRetryable, now)
	if !got.IsZero() {
		t.Error("max attempts should return zero time (DLQ)")
	}
}

func TestBreakerOpensAfter5Min(t *testing.T) {
	var b breaker
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
	if !b.IsOpen(start.Add(breakerTimeout)) {
		t.Error("breaker should open after breakerTimeout")
	}
}

func TestBreakerClosesOnSuccess(t *testing.T) {
	var b breaker
	start := time.Now()
	b.RecordFailure(start)
	b.RecordFailure(start.Add(time.Minute))
	// Success closes it.
	b.RecordSuccess()
	if b.IsOpen(start.Add(breakerTimeout + time.Minute)) {
		t.Error("breaker should be closed after success")
	}
}

func TestBreakerFailover(t *testing.T) {
	var b breaker
	start := time.Now()
	b.RecordFailure(start)
	// breaker open → failover.
	if !b.IsOpen(start.Add(breakerTimeout)) {
		t.Error("should failover when breaker is open")
	}
	b.RecordSuccess()
	if b.IsOpen(start.Add(breakerTimeout)) {
		t.Error("should NOT failover when breaker is closed")
	}
}

func TestBreakerStreakExtendsNotResets(t *testing.T) {
	var b breaker
	t0 := time.Now()
	b.RecordFailure(t0)
	// Another failure at t0+1m should NOT reset the streak start.
	b.RecordFailure(t0.Add(time.Minute))
	// The breaker should open at t0+5m, not t0+1m+5m.
	if !b.IsOpen(t0.Add(breakerTimeout)) {
		t.Error("breaker should open based on first failure, not extended streak")
	}
}

func TestFormatTelegram_CarriesTheAnswerAndTheLink(t *testing.T) {
	// The Telegram message IS the product on a phone: what broke, the facts,
	// the raw lines, and a link into the incident.
	got := formatTelegram(AlertPayload{
		Status:     "down",
		Title:      "Checkout is down",
		IncidentID: "7d31b0c4",
		Class:      "page",
		Summary:    "Down for 4 minutes.",
		Fields: []Field{
			{Label: "Target", Value: "https://example.com/health", Mono: true},
			{Label: "Down since", Value: "14:02 UTC"},
		},
		Lines:   []string{"HTTP/1.1 503 Service Unavailable"},
		Actions: []actionButton{{Label: "Runbook", URL: "https://acme.test/runbook"}},
	}, "https://upcontrol.io/app")
	for _, want := range []string{
		"🔴 <b>Checkout is down</b>",
		"Down for 4 minutes.",
		"Target: <code>https://example.com/health</code>",
		"Down since: 14:02 UTC",
		"<pre>HTTP/1.1 503 Service Unavailable</pre>",
		`<a href="https://acme.test/runbook">Runbook</a>`,
		`<a href="https://upcontrol.io/app?incident=7d31b0c4">Open the incident</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("telegram message missing %q:\n%s", want, got)
		}
	}
	// The title names the monitor already; a "Monitor:" row would say it twice
	// on a screen with no height to spare.
	if strings.Contains(got, "Monitor:") {
		t.Fatalf("telegram message repeats the monitor as a row:\n%s", got)
	}
}

func TestFormatTelegram_EscapesEverythingDynamic(t *testing.T) {
	// A log-alert title IS an error message: one '<' in it and the Bot API
	// rejects the whole sendMessage as malformed HTML.
	got := formatTelegram(AlertPayload{
		Status:  "check",
		Title:   "Error in api: Promise<Map<string, T>> rejected",
		Summary: "escape <b>me</b>",
		Fields:  []Field{{Label: "<i>k</i>", Value: "a && b < c", Mono: true}},
		Lines:   []string{`<img src=x onerror="boom">`},
		Actions: []actionButton{{Label: "a<b>c", URL: "https://x.test/?a=1&b=2"}},
	}, "https://upcontrol.io/app")
	for _, banned := range []string{
		"Promise<Map", "<i>k</i>", "<img src=x", "<b>me</b>", "a<b>c",
	} {
		if strings.Contains(got, banned) {
			t.Fatalf("unescaped %q reached the message:\n%s", banned, got)
		}
	}
	for _, want := range []string{
		"Promise&lt;Map&lt;string, T&gt;&gt;",
		"a &amp;&amp; b &lt; c",
		"&lt;img src=x",
		`href="https://x.test/?a=1&amp;b=2"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("escaped form %q missing:\n%s", want, got)
		}
	}
}

func TestFormatTelegram_StatusIsShapePlusWords(t *testing.T) {
	// Colour is never the only channel: each status gets its emoji, and the
	// words beside it carry the same fact.
	for status, emoji := range map[string]string{"down": "🔴", "check": "🟠", "ok": "🟢"} {
		got := formatTelegram(AlertPayload{Status: status, Title: "t"}, "")
		if !strings.HasPrefix(got, emoji+" ") {
			t.Fatalf("status %q: message does not open with %s:\n%s", status, emoji, got)
		}
	}
}

func TestFormatTelegram_LinkFollowsTheClass(t *testing.T) {
	app := "https://upcontrol.io/app"
	cases := []struct {
		name  string
		p     AlertPayload
		href  string
		label string
	}{
		{"page deep-links its incident",
			AlertPayload{Class: "page", Status: "down", Title: "t", IncidentID: "abc"},
			app + "?incident=abc", "Open the incident"},
		{"recovered follow-up closes quietly",
			AlertPayload{Class: "followup", Status: "ok", Title: "t", IncidentID: "abc"},
			app + "?incident=abc", "The incident and its timeline are on the dashboard"},
		{"still-down follow-up stays a call to action",
			AlertPayload{Class: "followup", Status: "down", Title: "t", IncidentID: "abc"},
			app + "?incident=abc", "Open the incident"},
		{"a log alert has no incident to open",
			AlertPayload{Class: "ticket", Status: "check", Title: "t"},
			app, "Open this log group"},
		{"a test points at alert settings",
			AlertPayload{Class: "test", Status: "ok", Title: "t"},
			app + "/alerts", "Alert settings"},
	}
	for _, tc := range cases {
		got := formatTelegram(tc.p, app)
		want := `<a href="` + tc.href + `">` + tc.label + `</a>`
		if !strings.Contains(got, want) {
			t.Fatalf("%s: missing %q:\n%s", tc.name, want, got)
		}
	}
	// No app URL configured — no link, never an <a> with an empty href.
	if got := formatTelegram(AlertPayload{Class: "page", Status: "down", Title: "t", IncidentID: "abc"}, ""); strings.Contains(got, "<a href") {
		t.Fatalf("link rendered with no app URL:\n%s", got)
	}
}

func TestDownForLine_StaysHonestUnderAMinute(t *testing.T) {
	from := time.Date(2026, 8, 23, 14, 2, 10, 0, time.UTC)
	cases := []struct {
		to   time.Time
		want string
	}{
		{from.Add(30 * time.Second), "Down for under a minute, 14:02 to 14:02 UTC."},
		{from.Add(90 * time.Second), "Down for 1 minute, 14:02 to 14:03 UTC."},
		{from.Add(12 * time.Minute), "Down for 12 minutes, 14:02 to 14:14 UTC."},
	}
	for _, tc := range cases {
		if got := downForLine(from, tc.to); got != tc.want {
			t.Fatalf("downForLine = %q, want %q", got, tc.want)
		}
	}
}

// The button invariant: actions go everywhere, authorised by WHO pressed;
// the web_app row is personal-only. Captured at the HTTP boundary.
func TestTelegramButtonsFollowTheChatKind(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	restore := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var v map[string]any
		_ = json.NewDecoder(req.Body).Decode(&v)
		mu.Lock()
		bodies = append(bodies, v)
		mu.Unlock()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	})}
	defer func() { httpClient = restore }()

	ch := &TelegramChannel{Token: func(context.Context) string { return "t" }}
	payload := AlertPayload{Title: "down", Status: "down", IncidentID: "abc123"}

	// Personal (the worker sets Buttons for every telegram channel).
	personal := payload
	personal.Buttons = true
	if _, err := ch.Send(context.Background(), "42", personal); err != nil {
		t.Fatal(err)
	}
	// Broadcast group: Buttons and Group both set.
	group := payload
	group.Buttons = true
	group.Group = true
	if _, err := ch.Send(context.Background(), "-100", group); err != nil {
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
	groupMarkup, groupPresent := bodies[1]["reply_markup"]
	groupJSON, gerr := json.Marshal(groupMarkup)
	if !groupPresent || gerr != nil || !strings.Contains(string(groupJSON), "\"callback_data\":\"ack:abc123\"") {
		t.Fatalf("group alert missing ack button: %v", bodies[1]["reply_markup"])
	}
	if strings.Contains(string(groupJSON), "web_app") {
		t.Fatalf("group alert must carry no web_app button: %v", bodies[1]["reply_markup"])
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestStatusColor_IsStatusNotSeverity(t *testing.T) {
	// Discord renders the colour, so it must follow the same three states the
	// rest of the product uses.
	if statusColor("down") == statusColor("check") || statusColor("check") == statusColor("ok") {
		t.Fatal("down / check / ok must be three distinct colours")
	}
	if statusColor("anything-else") != statusColor("ok") {
		t.Fatal("an unknown status must read as ok, never as down")
	}
}

// keyboardDesc flattens a keyboard to "label=target | label=target" in row
// order; whole strings pin the buttons, their order and everything absent.
func keyboardDesc(kb [][]map[string]any) string {
	var out []string
	for _, row := range kb {
		for _, btn := range row {
			target, _ := btn["callback_data"].(string)
			if app, ok := btn["web_app"].(map[string]string); ok {
				target = app["url"]
			}
			out = append(out, btn["text"].(string)+"="+target)
		}
	}
	return strings.Join(out, " | ")
}

func TestTelegramKeyboard_ButtonsFollowTheIncidentKind(t *testing.T) {
	const app = "https://upcontrol.io/app"
	page := AlertPayload{Buttons: true, IncidentID: "abc123"}
	detect := AlertPayload{Buttons: true, IncidentID: "abc123", Detector: "errorrate"}
	groupPage := AlertPayload{Buttons: true, Group: true, IncidentID: "abc123"}
	groupDetect := AlertPayload{Buttons: true, Group: true, IncidentID: "abc123", Detector: "errorrate"}

	for _, tc := range []struct {
		name    string
		payload AlertPayload
		appURL  string
		want    string
	}{
		{
			name:    "an outage is acknowledged, resolved, or opened",
			payload: page,
			appURL:  app,
			want:    "Acknowledge=ack:abc123 | Resolve=resolve:abc123 | Open=https://upcontrol.io/app?incident=abc123",
		},
		{
			// No Resolve: a detector closes its own incidents, and the button
			// would be one that cannot act.
			name:    "a detector spike is acknowledged or opened",
			payload: detect,
			appURL:  app,
			want:    "Acknowledge=ack:abc123 | Open=https://upcontrol.io/app?incident=abc123",
		},
		{
			// Telegram accepts web_app over https only, so a local stack keeps
			// the callbacks and the message's own text link.
			name:    "a local stack sends callbacks only",
			payload: page,
			appURL:  "http://localhost/app",
			want:    "Acknowledge=ack:abc123 | Resolve=resolve:abc123",
		},
		{
			// A group keeps Acknowledge/Resolve but drops the web_app row: the
			// Bot API refuses web_app buttons outside private chats.
			name:    "a group page is acknowledged or resolved",
			payload: groupPage,
			appURL:  app,
			want:    "Acknowledge=ack:abc123 | Resolve=resolve:abc123",
		},
		{
			// Same rule as the private detector case (no Resolve; a detector
			// closes its own incidents) minus the web_app row, which the Bot
			// API refuses outside private chats.
			name:    "a group detector spike is acknowledged only",
			payload: groupDetect,
			appURL:  app,
			want:    "Acknowledge=ack:abc123",
		},
		// Nothing to act on: a payload without the buttons flag and a recovered
		// follow-up (the worker clears Buttons), and a test alert (no incident).
		{name: "a message without the buttons flag gets no keyboard", payload: AlertPayload{IncidentID: "abc123"}, appURL: app, want: ""},
		{name: "a test alert gets no buttons", payload: AlertPayload{Buttons: true}, appURL: app, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keyboardDesc(telegramKeyboard(tc.payload, tc.appURL)); got != tc.want {
				t.Errorf("keyboard =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}
