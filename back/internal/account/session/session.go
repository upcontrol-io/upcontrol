// Package session manages httpOnly session cookies backed by the Postgres
// session table. The cookie value is a random token; the table stores only
// sha256(token) so a DB leak cannot impersonate sessions. On every /v1/* request
// the middleware reads the cookie, hashes it, looks up the session, and rejects
// expired or missing sessions with 401.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/storage/pg"
)

const (
	CookieName = "uc_session"
	DefaultTTL = 30 * 24 * time.Hour
)

var ErrNoSession = errors.New("session: no valid session")

// Manager creates and validates sessions against the session table.
type Manager struct {
	pool *pg.Pool
	ttl  time.Duration
	log  *slog.Logger

	// Single-user mode (public-first-split, Decision 16): when set, every
	// request carries this identity and the cookie is never consulted.
	fixedPersonID int64
	fixedTenantID int64
}

// New builds a Manager from a *pg.Pool. `log` may be nil in tests.
func New(p *pg.Pool, ttl time.Duration, log *slog.Logger) *Manager {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{pool: p, ttl: ttl, log: log}
}

// Create generates a random session token, persists its hash, and returns the
// raw token (to set as a cookie).
func (m *Manager) Create(ctx context.Context, personID, tenantID int64) (string, error) {
	raw := randomToken()
	hash := sha256.Sum256([]byte(raw))
	err := m.pool.Queries().CreateSession(ctx, sqlc.CreateSessionParams{
		TokenHash: hash[:],
		PersonID:  personID,
		TenantID:  tenantID,
		TtlSecs:   m.ttl.Seconds(),
	})
	return raw, err
}

// LookupSession validates a raw cookie token against the session table.
func (m *Manager) LookupSession(ctx context.Context, rawToken string) (sqlc.Session, error) {
	hash := sha256.Sum256([]byte(rawToken))
	s, err := m.pool.Queries().GetSessionByToken(ctx, hash[:])
	if err != nil {
		// The one refusal in the system that used to write nothing, and the one
		// the client turns into a redirect to /sign-in. Never log the token or
		// its hash — a log that can be replayed into a session is a second
		// credential store.
		m.log.Info("session: refused", "reason", "no valid session for token")
		return sqlc.Session{}, ErrNoSession
	}
	_ = m.pool.Queries().TouchSession(ctx, s.ID)
	return s, nil
}

// Delete removes a session (logout).
func (m *Manager) Delete(ctx context.Context, rawToken string) error {
	hash := sha256.Sum256([]byte(rawToken))
	return m.pool.Queries().DeleteSession(ctx, hash[:])
}

// WithFixedIdentity puts the Manager in single-user mode: FromRequest answers
// with this identity as a synthetic session on every request, cookie or not.
// Chainable, boot-time only — never call it after the mux is serving.
func (m *Manager) WithFixedIdentity(personID, tenantID int64) *Manager {
	m.fixedPersonID, m.fixedTenantID = personID, tenantID
	return m
}

// FromRequest extracts the session from an HTTP request's cookie.
func (m *Manager) FromRequest(ctx context.Context, r *http.Request) (sqlc.Session, error) {
	// The nil check keeps a cookieless request on a nil Manager answering
	// ErrNoSession, which is what tests that never mint sessions construct.
	if m != nil && m.fixedPersonID != 0 {
		return sqlc.Session{PersonID: m.fixedPersonID, TenantID: m.fixedTenantID}, nil
	}
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return sqlc.Session{}, ErrNoSession
	}
	return m.LookupSession(ctx, c.Value)
}

// SetCookie writes the session cookie on an http.ResponseWriter. secure=false
// in dev so the cookie survives the HTTP dev server (Caddy on :80 with
// auto_https off); prod passes secure=true so it never crosses plain HTTP.
func SetCookie(w http.ResponseWriter, token string, ttl time.Duration, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: token, Path: "/",
		MaxAge: int(ttl.Seconds()), HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie expires the session cookie.
func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("session: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
