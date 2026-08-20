package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Agent sends through an external email-agent service: one
// HTTP POST per message, and the service owns the template, the retry queue
// and the provider. UC_EMAIL_URL set = this mailer; empty = the caller
// stays on SMTP.
type Agent struct {
	url    string // service base, e.g. http://mail-agent:8080, no trailing slash
	key    string // bearer token; empty = the service runs with auth disabled
	base   string // sign-in origin the magic link points at
	log    *slog.Logger
	client *http.Client // one per Agent; every send shares its timeout
}

// sendRequest is the exact body the agent's /send validates: a kind, a template
// name, the recipient and the vars that template renders from.
type sendRequest struct {
	Kind     string            `json:"kind"`
	Template string            `json:"template"`
	To       string            `json:"to"`
	Vars     map[string]string `json:"vars"`
}

// NewAgent refuses an empty URL the way NewSMTP refuses a half config: a
// mailer pointed at nothing must fail at boot, not on the first message.
func NewAgent(url, key string, log *slog.Logger) (*Agent, error) {
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("mailer: UC_EMAIL_URL is empty")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Agent{
		url:    strings.TrimRight(url, "/"),
		key:    key,
		log:    log,
		client: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// WithSignInBase sets the origin the magic link points at (e.g. https://upcontrol.io).
func (a *Agent) WithSignInBase(base string) *Agent { a.base = base; return a }

// SendCode queues one magic-link with the agent. The code crosses the wire to
// the service but is never logged, the same rule SMTP follows: a log line
// carrying it is a second place to steal a session from.
func (a *Agent) SendCode(ctx context.Context, to, code string) error {
	body, err := json.Marshal(sendRequest{
		Kind:     "transactional",
		Template: "magic-link",
		To:       to,
		Vars:     map[string]string{"code": code, "sign_in_base": a.base},
	})
	if err != nil {
		return fmt.Errorf("mailer: encode email-agent request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url+"/send", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mailer: build email-agent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.key != "" {
		req.Header.Set("Authorization", "Bearer "+a.key)
	}
	res, err := a.client.Do(req)
	if err != nil {
		a.log.Warn("mailer: email-agent send failed", "to", to, "err", err)
		return fmt.Errorf("mailer: email-agent request failed: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		// A short body snippet is the only clue a 4xx or 5xx leaves behind;
		// capped so an HTML error page cannot flood the error string.
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		a.log.Warn("mailer: email-agent send failed", "to", to, "status", res.StatusCode)
		return fmt.Errorf("mailer: email-agent %d: %s", res.StatusCode, snippet)
	}
	a.log.Info("mailer: code queued via email agent", "to", to)
	return nil
}
