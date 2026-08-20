// Package webhook handles inbound webhooks (plan §7.1) on two routes that
// share one mount, POST /hooks/{segment}:
//
//   - A segment naming a known provider (stripe, github, vercel) is the legacy
//     globally-secreted route: HMAC-verified against one env secret per
//     provider, written as tenant 0. Kept so nothing configured before the
//     token era breaks; deprecated, not a second product.
//   - Any other segment is a per-connection hook token (universal hooks,
//     Aug 14, 2026 — docs/plans/universal-hooks.md): the token resolves a
//     source_connection, so the event is attributed to its tenant and project
//     and the connection's last_signal_at is finally written by the thing it
//     describes. The token in the URL is the credential (128 bits, revoked by
//     disconnecting) — no HMAC gate, which is exactly what lets any provider
//     that can POST JSON use the endpoint. Signing providers are still
//     recognised by their headers for parsing, just not gated on it.
//
// Both routes deduplicate via webhook_seen (idempotent on (scope, event_id))
// and write the event to ClickHouse's events table for the correlation
// detectors. They are intentionally lenient on shape — the plan says "always
// 2xx on sane input" — but the legacy route stays strict on signature.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// Handler dispatches webhooks by provider.
type Handler struct {
	pool    *pg.Pool
	ch      *ch.Conn
	secrets map[string][]byte // provider → secret (HMAC key)
}

// New builds the handler.
func New(pool *pg.Pool, chConn *ch.Conn, secrets map[string][]byte) *Handler {
	return &Handler{pool: pool, ch: chConn, secrets: secrets}
}

// knownProviders are the legacy globally-secreted routes. Any other path
// segment is treated as a per-connection hook token — a token can therefore
// never collide with a provider name, because tokens are hex and these are not.
var knownProviders = map[string]bool{"stripe": true, "github": true, "vercel": true}

// ServeHTTP is POST /hooks/{segment} — a provider name or a hook token.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segment := r.PathValue("provider")
	if segment == "" {
		// Fall back to path split for non-1.22 routers.
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 2 {
			segment = parts[1]
		}
	}

	// Read the raw body (needed for HMAC verification and the hash fallback).
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if knownProviders[segment] {
		h.serveLegacy(w, r, segment, body)
		return
	}
	h.serveToken(w, r, segment, body)
}

// serveLegacy is the pre-token route: one global secret per provider, events
// written as tenant 0 (docs/backend-gaps.md). Nothing new should point here.
func (h *Handler) serveLegacy(w http.ResponseWriter, r *http.Request, provider string, body []byte) {
	// Verify the signature. The provider-specific verify function knows the
	// header name and algorithm.
	secret, ok := h.secrets[provider]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !verifySignature(provider, r.Header, body, secret) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Parse the event to extract the dedup key and the normalized fields.
	evt, err := parseEvent(provider, body)
	if err != nil {
		// Bad JSON from a provider whose signature we verified is still a 200
		// (the signature was right; the body is just not what we expected).
		w.WriteHeader(http.StatusOK)
		return
	}
	if evt.EventID == "" {
		// Without this, every id-less event deduplicated against the empty
		// string — the second one ever received would have been dropped.
		evt.EventID = bodyHash(body)
	}

	// Dedup: if this (provider, event_id) has been seen, respond 200 (idempotent).
	seen, err := h.checkSeen(r.Context(), provider, evt.EventID)
	if err != nil || seen {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Record the event in ClickHouse for the correlation detectors.
	if evt.HasAmount {
		_ = h.ch.InsertEvents(r.Context(), []ch.EventRow{{
			TenantID:    evt.TenantID,
			ProjectID:   evt.ProjectID,
			TS:          evt.Timestamp,
			Name:        evt.Name,
			Labels:      evt.Labels,
			AmountMinor: evt.AmountMinor,
			Currency:    evt.Currency,
		}})
	} else {
		_ = h.ch.InsertEvents(r.Context(), []ch.EventRow{{
			TenantID:  evt.TenantID,
			ProjectID: evt.ProjectID,
			TS:        evt.Timestamp,
			Name:      evt.Name,
			Labels:    evt.Labels,
		}})
	}

	// Mark as seen (5-minute window before GC).
	_ = h.markSeen(r.Context(), provider, evt.EventID)

	w.WriteHeader(http.StatusOK)
}

// --- the token route (universal hooks) ---

// connection is what a hook token resolves to — everything attribution needs.
type connection struct {
	ID        int64
	TenantID  uint64
	ProjectID uint64
	Kind      string
	Paused    bool
}

func (h *Handler) connectionByToken(ctx context.Context, token string) (*connection, error) {
	var c connection
	err := h.pool.Raw().QueryRow(ctx,
		`SELECT id, tenant_id, project_id, kind, paused
		   FROM source_connection WHERE hook_token = $1`, token).
		Scan(&c.ID, &c.TenantID, &c.ProjectID, &c.Kind, &c.Paused)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (h *Handler) serveToken(w http.ResponseWriter, r *http.Request, token string, body []byte) {
	if token == "" || len(token) > 128 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	conn, err := h.connectionByToken(r.Context(), token)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if conn.Paused {
		// 200, and nothing recorded: a 4xx would make a well-behaved provider
		// retry a feed the owner switched off, and a paused connection must
		// not keep collecting signals (or "paused" quietly means "hidden").
		w.WriteHeader(http.StatusOK)
		return
	}

	// A signing provider is recognised by its headers so its events keep their
	// normalized names (github_push, vercel_deployment_created, payment_*);
	// everything else parses generically. Neither is gated on a signature —
	// the token is the credential here.
	provider := detectProvider(r.Header)
	evt, raw, err := parseEventRaw(provider, body)
	if err != nil {
		// Not JSON. The token was right, so the poster is the owner's tool;
		// answering 4xx would put a retry loop between them and us.
		w.WriteHeader(http.StatusOK)
		return
	}
	if evt.Name == "" || evt.Name == "unknown" {
		evt.Name = genericEventName(r.Header, raw, conn.Kind)
	}
	if evt.EventID == "" {
		evt.EventID = genericEventID(r.Header, raw, body)
	}

	// Dedup scoped to the token, not the provider: two connections fed by the
	// same provider are two sinks, and one must not eat the other's events.
	scope := "t:" + token
	seen, err := h.checkSeen(r.Context(), scope, evt.EventID)
	if err != nil || seen {
		w.WriteHeader(http.StatusOK)
		return
	}

	evt.TenantID = conn.TenantID
	evt.ProjectID = conn.ProjectID
	row := ch.EventRow{
		TenantID:  evt.TenantID,
		ProjectID: evt.ProjectID,
		TS:        evt.Timestamp,
		Name:      evt.Name,
		Labels:    evt.Labels,
	}
	if evt.HasAmount {
		row.AmountMinor = evt.AmountMinor
		row.Currency = evt.Currency
	}
	_ = h.ch.InsertEvents(r.Context(), []ch.EventRow{row})

	_ = h.markSeen(r.Context(), scope, evt.EventID)

	// The green dot on the Sources screen derives from last_signal_at, and
	// this is its writer: the connection is "up" because something arrived,
	// which is the only thing "up" may mean for a feed. last_event is the
	// panel's receipt — it shows WHAT arrived, because the provider's own
	// "send test webhook" button is the tester and the receipt is the proof.
	_, _ = h.pool.Raw().Exec(r.Context(),
		`UPDATE source_connection SET last_signal_at = now(), status = 'ok', last_event = $2 WHERE id = $1`,
		conn.ID, evt.Name)

	w.WriteHeader(http.StatusOK)
}

// detectProvider names the signing provider by the headers it always sends, or
// "" for the generic path. Detection affects parsing only, never acceptance.
func detectProvider(h http.Header) string {
	switch {
	case h.Get("Stripe-Signature") != "":
		return "stripe"
	case h.Get("X-GitHub-Event") != "" || h.Get("X-Hub-Signature-256") != "":
		return "github"
	case h.Get("X-Vercel-Signature") != "":
		return "vercel"
	}
	return ""
}

// genericEventName finds a name for a payload no normalizer claimed: GitHub's
// event header first (push events carry no `action` in the body), then the
// fields half the webhook world uses, then the connection kind — an event
// named after its feed still correlates by time, an "unknown" does not read.
func genericEventName(h http.Header, raw map[string]any, kind string) string {
	if ghEvent := h.Get("X-GitHub-Event"); ghEvent != "" {
		return sanitizeEventName("github_" + ghEvent)
	}
	for _, key := range []string{"event", "type", "action", "event_type"} {
		if s, ok := raw[key].(string); ok && s != "" {
			return sanitizeEventName(s)
		}
	}
	if kind != "" {
		return sanitizeEventName(kind)
	}
	return "webhook"
}

// genericEventID prefers the provider's own idempotency key (header or body),
// falling back to a body hash so dedup works even for providers that never
// heard of one. Identical retries collapse; distinct payloads never do.
func genericEventID(h http.Header, raw map[string]any, body []byte) string {
	if id := h.Get("X-GitHub-Delivery"); id != "" {
		return id
	}
	for _, key := range []string{"id", "event_id", "uuid", "delivery_id"} {
		if s, ok := raw[key].(string); ok && s != "" {
			return s
		}
	}
	return bodyHash(body)
}

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// sanitizeEventName folds arbitrary provider strings into the events table's
// snake_case grammar and caps the length — a name is a label, not a payload.
func sanitizeEventName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	if out == "" {
		return "webhook"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// --- signature verification ---

func verifySignature(provider string, h http.Header, body []byte, secret []byte) bool {
	switch provider {
	case "stripe":
		// Stripe: HMAC-SHA256 of the body with the signing secret, compared to
		// the Stripe-Signature header (format: "t=...,v1=...").
		sig := h.Get("Stripe-Signature")
		return verifyStripe(sig, body, secret)
	case "github":
		// GitHub: HMAC-SHA256 of the body, hex-encoded, in X-Hub-Signature-256.
		sig := h.Get("X-Hub-Signature-256")
		return verifyHMACSHA256(body, secret, sig)
	case "vercel":
		// Vercel: same as GitHub — HMAC-SHA256, hex, in X-Vercel-Signature.
		sig := h.Get("X-Vercel-Signature")
		return verifyHMACSHA256(body, secret, sig)
	default:
		return false
	}
}

func verifyHMACSHA256(body, secret []byte, expectedHex string) bool {
	if expectedHex == "" {
		return false
	}
	// Strip "sha256=" prefix if present.
	expectedHex = strings.TrimPrefix(expectedHex, "sha256=")
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	actual := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(actual), []byte(expectedHex))
}

func verifyStripe(sigHeader string, body, secret []byte) bool {
	// Parse "t=1234567890,v1=abc123..." format.
	var timestamp, signature string
	for _, part := range strings.Split(sigHeader, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "t=") {
			timestamp = part[2:]
		} else if strings.HasPrefix(part, "v1=") {
			signature = part[3:]
		}
	}
	if timestamp == "" || signature == "" {
		return false
	}
	// Stripe signs: HMAC(timestep.body).
	signedPayload := timestamp + "." + string(body)
	return verifyHMACSHA256([]byte(signedPayload), secret, signature)
}

// --- event parsing ---

type Event struct {
	EventID     string
	TenantID    uint64
	ProjectID   uint64
	Name        string
	Timestamp   time.Time
	Labels      map[string]string
	AmountMinor int64
	Currency    string
	HasAmount   bool
}

func parseEvent(provider string, body []byte) (*Event, error) {
	evt, _, err := parseEventRaw(provider, body)
	return evt, err
}

// parseEventRaw also returns the decoded body, so the token route's generic
// fallbacks (name, id) can read it without unmarshalling a second time.
func parseEventRaw(provider string, body []byte) (*Event, map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, err
	}

	evt := &Event{
		Name:      parseEventName(provider, raw),
		EventID:   parseEventID(provider, raw),
		Labels:    parseLabels(provider, raw),
		Timestamp: parseEventTimestamp(provider, raw),
	}

	// Stripe payment events carry an amount.
	if amt, curr, ok := parseStripeAmount(raw); ok {
		evt.AmountMinor = amt
		evt.Currency = curr
		evt.HasAmount = true
	}

	return evt, raw, nil
}

func parseEventID(provider string, raw map[string]any) string {
	switch provider {
	case "stripe":
		if id, ok := raw["id"].(string); ok {
			return id
		}
	case "github", "vercel":
		if id, ok := raw["delivery_id"].(string); ok {
			return id
		}
	}
	// Fall back to a hash of the body.
	return ""
}

func parseEventName(provider string, raw map[string]any) string {
	switch provider {
	case "stripe":
		if t, ok := raw["type"].(string); ok {
			return normalizeStripeEvent(t)
		}
	case "github":
		if a, ok := raw["action"].(string); ok {
			return "github_" + a
		}
	case "vercel":
		if t, ok := raw["type"].(string); ok {
			return "vercel_" + t
		}
	}
	return "unknown"
}

func normalizeStripeEvent(t string) string {
	// Map Stripe's long event names to the 24-event dictionary (plan §4.3).
	switch {
	case strings.Contains(t, "payment_intent.succeeded"):
		return "payment_succeeded"
	case strings.Contains(t, "payment_intent.payment_failed"):
		return "payment_failed"
	case strings.Contains(t, "charge.refunded"):
		return "refund_issued"
	case strings.Contains(t, "customer.subscription.deleted"):
		return "subscription_cancelled"
	case strings.Contains(t, "customer.subscription.created"):
		return "subscription_created"
	default:
		return "stripe_" + strings.ReplaceAll(t, ".", "_")
	}
}

func parseLabels(_ string, raw map[string]any) map[string]string {
	labels := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok && len(s) < 256 {
			labels[k] = s
		}
	}
	return labels
}

func parseEventTimestamp(provider string, raw map[string]any) time.Time {
	// Correlating a deploy with an incident needs the time the PROVIDER says the
	// event happened, not the moment we received it (clock skew + queueing lag
	// would misplace the deploy marker). Fall back to now only if the field is
	// absent or unparseable — never drop the event.
	switch provider {
	case "stripe":
		if v, ok := raw["created"]; ok {
			if ts := toUnix(v); ts > 0 {
				return time.Unix(ts, 0).UTC()
			}
		}
	case "github":
		if s, ok := raw["created_at"].(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
		}
	case "vercel":
		if s, ok := raw["createdAt"].(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
		}
	default:
		// Generic payloads (the token route): the fields half the webhook
		// world uses, RFC3339 strings first, unix seconds second.
		for _, key := range []string{"created_at", "createdAt", "timestamp"} {
			if s, ok := raw[key].(string); ok {
				if t, err := time.Parse(time.RFC3339, s); err == nil {
					return t
				}
			}
		}
		for _, key := range []string{"created", "timestamp", "ts"} {
			if ts := toUnix(raw[key]); ts > 0 {
				return time.Unix(ts, 0).UTC()
			}
		}
	}
	return time.Now().UTC()
}

// toUnix coerces a JSON number (always float64 after encoding/json) into a unix
// second count; 0 means "not a number".
func toUnix(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func parseStripeAmount(raw map[string]any) (int64, string, bool) {
	// Stripe amounts are in the `data.object.amount` field (minor units).
	data, ok := raw["data"].(map[string]any)
	if !ok {
		return 0, "", false
	}
	obj, ok := data["object"].(map[string]any)
	if !ok {
		return 0, "", false
	}
	amt, ok := obj["amount"].(float64)
	if !ok {
		return 0, "", false
	}
	curr, _ := obj["currency"].(string)
	return int64(amt), strings.ToUpper(curr), true
}

// --- dedup ---

func (h *Handler) checkSeen(ctx context.Context, provider, eventID string) (bool, error) {
	var exists int
	err := h.pool.Raw().QueryRow(ctx,
		`SELECT 1 FROM webhook_seen WHERE provider = $1 AND event_id = $2`,
		provider, eventID).Scan(&exists)
	// "No row" is the normal case — the event is NEW. Returning ErrNoRows as
	// an error made the caller's `err != nil || seen` treat every first event
	// as a duplicate and drop it: the handler answered 200 and wrote nothing,
	// for every webhook that was not a retry.
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return exists == 1, nil
}

func (h *Handler) markSeen(ctx context.Context, provider, eventID string) error {
	_, err := h.pool.Raw().Exec(ctx,
		`INSERT INTO webhook_seen (provider, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		provider, eventID)
	return err
}

// Unused import suppression.
var _ = bytes.NewReader
