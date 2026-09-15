// Package session manages httpOnly cookies backed by the Postgres session
// table; only sha256(token) is stored, so a DB leak cannot impersonate.
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

	"github.com/jackc/pgx/v5"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/storage/pg"
)

const (
	CookieName = "uc_session"
	DefaultTTL = 30 * 24 * time.Hour
)

// ErrNoSession is the one refusal meaning the reader really has no session, and the only
// one a caller may answer 401 with. Every other error from this package is the CHECK
// failing — a dead pool, a statement timeout — which is a 500: the front reads every 401 as
// a lost session and leaves the app, so a database blip answered 401 signs everyone out.
var ErrNoSession = errors.New("session: no valid session")

// Refusal is the status and error code a session error deserves, and the ONE place that
// decides it: 401 only when the reader really has no session — no cookie, or a token that
// resolves to no row — and 500 when the check itself failed. Every door that reads a session
// answers through this, because the front leaves the app on any 401 and a database blip
// answered 401 signs every signed-in reader out at once (prod, 2026-09-12).
func Refusal(err error) (int, string) {
	if errors.Is(err, ErrNoSession) || errors.Is(err, pgx.ErrNoRows) {
		return http.StatusUnauthorized, "no_session"
	}
	return http.StatusInternalServerError, "internal"
}

// Manager creates and validates sessions against the session table.
type Manager struct {
	pool *pg.Pool
	ttl  time.Duration
	log  *slog.Logger

	// Single-user mode: when set, every request carries this identity and the
	// cookie is never consulted.
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
// raw token (to set as a cookie). projectID is the project the session opens
// on; nil when the person reaches none in that workspace.
func (m *Manager) Create(ctx context.Context, personID, tenantID int64, projectID *int64) (string, error) {
	raw := randomToken()
	hash := sha256.Sum256([]byte(raw))
	err := m.pool.Queries().CreateSession(ctx, sqlc.CreateSessionParams{
		TokenHash: hash[:],
		PersonID:  personID,
		TenantID:  tenantID,
		ProjectID: projectID,
		TtlSecs:   m.ttl.Seconds(),
	})
	return raw, err
}

// LookupSession validates a raw cookie token against the session table.
func (m *Manager) LookupSession(ctx context.Context, rawToken string) (sqlc.Session, error) {
	hash := sha256.Sum256([]byte(rawToken))
	s, err := m.pool.Queries().GetSessionByToken(ctx, hash[:])
	if err != nil {
		// Never log the token or its hash: a log that can be replayed into a
		// session is a second credential store.
		if !errors.Is(err, pgx.ErrNoRows) {
			m.log.Error("session: lookup failed", "err", err)
			return sqlc.Session{}, err
		}
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
// with this identity on every request. Boot-time only.
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

// SetCookie writes the session cookie; secure=false in dev (HTTP server),
// true in prod so it never crosses plain HTTP.
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
