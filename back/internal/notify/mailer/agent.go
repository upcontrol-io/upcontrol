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

// agent sends through an external email-agent service: one HTTP POST per
// message; the service owns the template, queue and provider.
type agent struct {
	url    string // service base, e.g. http://mail-agent:8080, no trailing slash
	key    string // bearer token; empty = the service runs with auth disabled
	base   string // sign-in origin the magic link points at
	log    *slog.Logger
	client *http.Client // one per agent; every send shares its timeout
}

// sendRequest is the exact body the agent's /send validates: a kind, a template
// name, the recipient and the vars that template renders from.
type sendRequest struct {
	Kind     string            `json:"kind"`
	Template string            `json:"template"`
	To       string            `json:"to"`
	Vars     map[string]string `json:"vars"`
}

// NewAgent refuses an empty URL: a mailer pointed at nothing must fail at
// boot, not on the first message.
func NewAgent(url, key string, log *slog.Logger) (*agent, error) {
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("mailer: UC_EMAIL_URL is empty")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &agent{
		url:    strings.TrimRight(url, "/"),
		key:    key,
		log:    log,
		client: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// WithSignInBase sets the origin the magic link points at (e.g. https://upcontrol.io).
func (a *agent) WithSignInBase(base string) *agent { a.base = base; return a }

// SendCode queues one magic-link with the agent. The code crosses the wire
// but is never logged: a log line carrying it is a second place to steal from.
func (a *agent) SendCode(ctx context.Context, to, code string) error {
	if err := a.post(ctx, to, sendRequest{
		Kind:     "transactional",
		Template: "magic-link",
		To:       to,
		Vars:     map[string]string{"code": code, "sign_in_base": a.base},
	}); err != nil {
		return err
	}
	a.log.Info("mailer: code queued via email agent", "to", to)
	return nil
}

// SendInvite queues one project invitation. The code crosses the wire but is
// never logged; `to` stays in the envelope, the agent builds the link itself.
func (a *agent) SendInvite(ctx context.Context, to, code, project, invitedBy string) error {
	if err := a.post(ctx, to, sendRequest{
		Kind:     "transactional",
		Template: "invite",
		To:       to,
		Vars: map[string]string{
			"code":         code,
			"sign_in_base": a.base,
			"project":      project,
			"invited_by":   invitedBy,
		},
	}); err != nil {
		return err
	}
	a.log.Info("mailer: invite queued via email agent", "to", to)
	return nil
}

// post delivers one request to the agent's /send and turns the HTTP outcome
// into an error; both message types ride it.
func (a *agent) post(ctx context.Context, to string, req sendRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("mailer: encode email-agent request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url+"/send", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mailer: build email-agent request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if a.key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.key)
	}
	res, err := a.client.Do(httpReq)
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
	return nil
}
