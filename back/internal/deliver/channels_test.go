// Tests for EmailChannel: the wire contract with the email agent's /send, captured at
// the HTTP boundary (method, path, bearer, JSON body) plus the status
// conventions ClassifyError feeds on.

package deliver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestEmailChannelPostsNotificationToSend(t *testing.T) {
	payload := AlertPayload{
		Title:       "Test title",
		Status:      "down",
		MonitorName: "example.com/checkout",
		Fields:      map[string]string{"Region": "fra"},
	}

	var mu sync.Mutex
	var method, path, auth, ctype string
	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		mu.Lock()
		defer mu.Unlock()
		method, path, auth, ctype = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		body = v
	}))
	defer ts.Close()

	// Trailing slash on purpose: the channel must normalize to /send.
	ch := &EmailChannel{APIURL: ts.URL + "/", APIKey: "sekret"}
	code, err := ch.Send(context.Background(), "ops@example.com", payload)
	if err != nil || code != 200 {
		t.Fatalf("Send = (%d, %v), want (200, nil)", code, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if path != "/send" {
		t.Errorf("path = %q, want /send", path)
	}
	if auth != "Bearer sekret" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer sekret")
	}
	if !strings.HasPrefix(ctype, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ctype)
	}
	if body["kind"] != "notification" {
		t.Errorf("kind = %v, want notification", body["kind"])
	}
	if body["to"] != "ops@example.com" {
		t.Errorf("to = %v, want ops@example.com", body["to"])
	}
	if body["subject"] != "[down] Test title" {
		t.Errorf("subject = %v, want [down] Test title", body["subject"])
	}
	// A single Fields entry keeps formatEmail deterministic; equality is exact.
	if want := formatEmail(payload); body["text"] != want {
		t.Errorf("text = %q, want the formatEmail output %q", body["text"], want)
	}
}

// recordingSender captures what SMTPChannel hands the mailer.
type recordingSender struct {
	to, subject, text string
	err               error
}

func (r *recordingSender) Send(_ context.Context, to, subject, text string) error {
	r.to, r.subject, r.text = to, subject, text
	return r.err
}

func TestSMTPChannelSendsSubjectAndBody(t *testing.T) {
	payload := AlertPayload{
		Title:       "Test title",
		Status:      "down",
		MonitorName: "example.com/checkout",
		Fields:      map[string]string{"Region": "fra"},
	}
	rec := &recordingSender{}
	ch := &SMTPChannel{Mailer: rec}
	if ch.Kind() != "email" {
		t.Fatalf("Kind = %q, want email", ch.Kind())
	}
	code, err := ch.Send(context.Background(), "ops@example.com", payload)
	if err != nil || code != 200 {
		t.Fatalf("Send = (%d, %v), want (200, nil)", code, err)
	}
	if rec.to != "ops@example.com" {
		t.Errorf("to = %q, want ops@example.com", rec.to)
	}
	if rec.subject != "[down] Test title" {
		t.Errorf("subject = %q, want [down] Test title", rec.subject)
	}
	if want := formatEmail(payload); rec.text != want {
		t.Errorf("text = %q, want the formatEmail output %q", rec.text, want)
	}
}

func TestSMTPChannelSendFailureReturnsZero(t *testing.T) {
	// Same convention as the network-failure case of doPost: (0, err) is the
	// retryable outcome ClassifyError expects.
	rec := &recordingSender{err: context.DeadlineExceeded}
	code, err := (&SMTPChannel{Mailer: rec}).Send(context.Background(), "ops@example.com", AlertPayload{Title: "t", Status: "down"})
	if code != 0 || err == nil {
		t.Fatalf("Send = (%d, %v), want (0, non-nil error)", code, err)
	}
}

func TestEmailChannelNoBearerWhenKeyEmpty(t *testing.T) {
	var mu sync.Mutex
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer ts.Close()

	ch := &EmailChannel{APIURL: ts.URL}
	code, err := ch.Send(context.Background(), "ops@example.com", AlertPayload{Title: "t", Status: "down"})
	if err != nil || code != 200 {
		t.Fatalf("Send = (%d, %v), want (200, nil)", code, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "" {
		t.Errorf("Authorization = %q, want empty with no API key", auth)
	}
}

func TestEmailChannelNon2xxReturnsStatusWithNilError(t *testing.T) {
	// Package convention (doPost and worker.processItem): a non-2xx leaves
	// err nil and hands the raw status to ClassifyError; only network
	// failures return an error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()

	ch := &EmailChannel{APIURL: ts.URL, APIKey: "k"}
	code, err := ch.Send(context.Background(), "ops@example.com", AlertPayload{Title: "t", Status: "down"})
	if code != 500 || err != nil {
		t.Fatalf("Send = (%d, %v), want (500, nil)", code, err)
	}
	if got := ClassifyError(code); got != OutcomeRetryable {
		t.Errorf("ClassifyError(500) = %q, want %q", got, OutcomeRetryable)
	}
}

func TestEmailChannelNetworkErrorReturnsZero(t *testing.T) {
	// A closed listener is a refused connection: the channel must report a
	// network failure as status 0 (ClassifyError's retryable case).
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()

	ch := &EmailChannel{APIURL: url}
	code, err := ch.Send(context.Background(), "ops@example.com", AlertPayload{Title: "t", Status: "down"})
	if code != 0 || err == nil {
		t.Fatalf("Send = (%d, %v), want (0, non-nil error)", code, err)
	}
}
