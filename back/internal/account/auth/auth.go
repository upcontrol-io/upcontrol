// Package auth implements the login methods against the session table.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/analytics"
	"go.upcontrol.io/back/internal/storage/pg"
)

// Magic-link policy: code TTL, attempt cap, per-email cooldown; the per-IP
// throttle is a Postgres sliding window, so it holds across replicas.
const (
	codeTTL       = 10 * time.Minute
	maxAttempts   = 5
	emailCooldown = 60 * time.Second
	ipWindowCap   = 10 // max magic-link requests per IP per 5-minute window
	ipBlockWindow = 5 * time.Minute
)

// Mailer sends the two mails that carry a sign-in credential: the sign-in
// code and the project invitation. Nil means no delivery; dev echoes the code.
type Mailer interface {
	SendCode(ctx context.Context, email, code string) error
	SendInvite(ctx context.Context, to, code, project, invitedBy string) error
}

// MagicLink handles the request/redeem magic-link flow.
type MagicLink struct {
	pool    *pg.Pool
	sess    *session.Manager
	devMode bool
	mailer  Mailer
	log     *slog.Logger
	// Analytics recorder; nil (tests, unwired deployments) is a no-op.
	rec *analytics.Recorder
	// selfHosted (UC_SELF_HOSTED=1): tenants created here land on the
	// 'Self-hosted' plan instead of 'Free'.
	selfHosted bool
}

// NewMagicLink builds a magic-link handler. mailer may be nil (no email delivery
// yet); log may be nil (a discard default is used); rec may be nil.
func NewMagicLink(p *pg.Pool, sm *session.Manager, devMode bool, mailer Mailer, rec *analytics.Recorder, log *slog.Logger) *MagicLink {
	if log == nil {
		log = slog.Default()
	}
	return &MagicLink{pool: p, sess: sm, devMode: devMode, mailer: mailer, log: log, rec: rec}
}

// WithSelfHosted marks the install as self-hosted: every tenant this door
// creates gets the 'Self-hosted' plan. Chainable.
func (h *MagicLink) WithSelfHosted(v bool) *MagicLink { h.selfHosted = v; return h }

type magicLinkReq struct {
	Email string `json:"email"`
	Token string `json:"token,omitempty"`
}

func (h *MagicLink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if crossSitePost(r) {
		// A redeem forged from another site would hand this browser a session
		// in whatever account the attacker has a code for.
		writeErr(w, http.StatusForbidden, "cross_site")
		return
	}
	var req magicLinkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_body")
		return
	}
	// Normalised before anything is stored or looked up: the code is keyed by
	// this string.
	req.Email = NormalizeEmail(req.Email)
	if req.Email == "" {
		writeErr(w, http.StatusBadRequest, "missing_email")
		return
	}
	if req.Token == "" {
		h.request(w, r, req.Email)
	} else {
		h.redeem(w, r, req.Email, req.Token)
	}
}

func (h *MagicLink) request(w http.ResponseWriter, r *http.Request, email string) {
	ctx := analytics.WithScope(r.Context(), analytics.ScopeFromRequest(r))

	// Throttle by IP (sliding window, shared across replicas).
	if cnt, err := h.pool.Queries().RecordMagicLinkIP(ctx, analytics.ClientIP(r)); err == nil && cnt > ipWindowCap {
		writeErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	if _, err := h.ensurePerson(ctx, email); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	code, err := h.issueCode(ctx, email)
	if err != nil {
		if errors.Is(err, ErrCodeCooldown) {
			writeErr(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// Fired only when a code was actually issued, not on throttle or failure.
	h.rec.ServerEvent(ctx, "magic_link_requested", 0, 0, nil)

	// A runtime-configured mailer reports emptiness through the optional
	// Configured interface; an empty one takes the no-mailer path.
	mail := h.mailer
	if c, ok := mail.(interface{ Configured(context.Context) bool }); ok && !c.Configured(ctx) {
		mail = nil
	}

	if h.devMode {
		// Dev only: echo the code so e2e can log in without email. A send
		// failure is logged, not fatal; the code rides the response.
		if mail != nil {
			if err := mail.SendCode(ctx, email, code); err != nil {
				h.log.Warn("magic-link: send code failed", "email", email, "err", err)
			}
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"dev_token": code})
		return
	}

	// Prod: deliver the code. Without a mailer it is stored but unsent; the
	// response never carries it.
	if mail != nil {
		if err := mail.SendCode(ctx, email, code); err != nil {
			h.log.Warn("magic-link: send code failed", "email", email, "err", err)
			writeErr(w, http.StatusServiceUnavailable, "email_unavailable")
			return
		}
	} else {
		// The one exception to "never log the code": with no mailer, the
		// operator's log is the only way in. The response never carries it.
		h.log.Warn("magic-link: no mailer; sign-in code", "email", email, "code", code)
	}
	w.WriteHeader(http.StatusAccepted)
}

// ErrCodeCooldown guards the cooldown: reissuing would let anyone reset
// somebody else's live code, so the caller decides; the sign-in door answers 429.
var ErrCodeCooldown = errors.New("magic-link: a code was issued recently")

// issueCode stores one fresh code for email; both doors obey the same TTL,
// cap and cooldown. It does NOT create the account.
func (h *MagicLink) issueCode(ctx context.Context, email string) (string, error) {
	if existing, err := h.pool.Queries().GetMagicLinkCode(ctx, email); err == nil {
		if existing.CreatedAt.Valid && time.Since(existing.CreatedAt.Time) < emailCooldown {
			return "", ErrCodeCooldown
		}
	}
	code := genCode()
	hash := sha256.Sum256([]byte(code))
	if err := h.pool.Queries().UpsertMagicLinkCode(ctx, sqlc.UpsertMagicLinkCodeParams{
		Email:     email,
		CodeHash:  hash[:],
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(codeTTL), Valid: true},
	}); err != nil {
		return "", err
	}
	return code, nil
}

func (h *MagicLink) redeem(w http.ResponseWriter, r *http.Request, email, token string) {
	// The analytics scope rides the context down, so account_created fires
	// with this request's visitor identity.
	ctx := analytics.WithScope(r.Context(), analytics.ScopeFromRequest(r))

	stored, err := h.pool.Queries().GetMagicLinkCode(ctx, email)
	if err != nil {
		// No code issued (or never requested): reject. Same error for every
		// failure — the endpoint must not be an oracle for which email exists.
		writeErr(w, http.StatusUnauthorized, "invalid_token")
		return
	}

	// Verify in BOTH dev and prod. devMode only relaxes the REQUEST path.
	if !validateCode(codeRecordFrom(stored), token, time.Now()) {
		// Count the failed attempt so repeated guessing trips the cap.
		_, _ = h.pool.Queries().IncMagicLinkAttempts(ctx, email)
		writeErr(w, http.StatusUnauthorized, "invalid_token")
		return
	}

	// One-time: mark redeemed BEFORE creating the session, so a concurrent
	// redeem with the same code cannot also succeed.
	_, _ = h.pool.Queries().MarkMagicLinkRedeemed(ctx, email)

	person, err := h.ensurePerson(ctx, email)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// Ownership is proven here and only here; the request path must never
	// activate invites, it proves nothing about the caller.
	h.activateInvites(ctx, person.ID, email)
	sessToken, err := h.sess.Create(ctx, person.ID, person.TenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	session.SetCookie(w, sessToken, session.DefaultTTL, !h.devMode) // Secure only in prod (dev is HTTP)
	// The sign-in is the moment an anonymous uc_vid becomes a person; no
	// cookie means visitor_id 0, the events still count unlinked.
	h.rec.ServerEvent(ctx, "signed_in", person.ID, person.TenantID, nil)
	h.rec.LinkPerson(ctx, person.ID, person.TenantID)
	writeAccount(w, person)
}

// writeAccount is the body every sign-in door answers with, so two doors can
// never describe the same account differently.
func writeAccount(w http.ResponseWriter, person personInfo) {
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       uuidStr(person.PublicID),
		"name":     person.Name,
		"email":    person.Email,
		"initials": initials(person.Name, person.Email),
		"plan":     "Free",
		"billing":  "annual",
	})
}

type personInfo struct {
	ID       int64
	PublicID pgtype.UUID
	Name     string
	Email    string
	TenantID int64
}

// Provision is ensureAccount for callers outside the sign-in flow, so both
// doors produce the same tenant. domain names a NEW project, never renames.
func Provision(ctx context.Context, pool *pg.Pool, email, domain string, rec *analytics.Recorder, selfHosted bool) (personID, tenantID int64, err error) {
	h := &MagicLink{pool: pool, rec: rec, selfHosted: selfHosted}
	info, err := h.ensureAccount(ctx, email, domain)
	return info.ID, info.TenantID, err
}

// IssueLoginCode stores a fresh code for callers outside the sign-in flow; the
// same credential the sign-in door issues. ip goes through the shared window.
func IssueLoginCode(ctx context.Context, pool *pg.Pool, email, ip string) (string, error) {
	email = NormalizeEmail(email)
	h := &MagicLink{pool: pool}
	if cnt, err := pool.Queries().RecordMagicLinkIP(ctx, ip); err == nil && cnt > ipWindowCap {
		return "", ErrRateLimited
	}
	return h.issueCode(ctx, email)
}

// ErrRateLimited means the caller's IP is past the shared magic-link window.
var ErrRateLimited = errors.New("magic-link: too many requests from this address")

// The sign-in door leaves the domain EMPTY: the first website check names the
// project. A placeholder here would leak into every screen the customer sees.
func (h *MagicLink) ensurePerson(ctx context.Context, email string) (personInfo, error) {
	return h.ensureAccount(ctx, email, "")
}

func (h *MagicLink) ensureAccount(ctx context.Context, email, domain string) (personInfo, error) {
	// Defensive: every caller normalises, and this is where it would matter
	// if one ever forgot — the UNIQUE column below is byte-exact.
	email = NormalizeEmail(email)
	q := h.pool.Queries()
	existing, err := q.GetPersonByEmail(ctx, &email)
	if err == nil {
		tenantID, _ := h.tenantForPerson(ctx, existing.ID)
		return personInfo{
			ID: existing.ID, PublicID: existing.PublicID,
			Name: existing.Name, Email: ptrStr(existing.Email),
			TenantID: tenantID,
		}, nil
	}
	pubID := newUUID()
	p, err := q.CreatePerson(ctx, sqlc.CreatePersonParams{
		PublicID: pubID, Email: &email, Name: NameFromEmail(email),
	})
	if err != nil {
		return personInfo{}, err
	}
	tenantPubID := newUUID()
	var tenantID int64
	// 'Free' is the column default made explicit; a self-hosted install seeds
	// 'Self-hosted' instead. Both doors share this function.
	plan := "Free"
	if h.selfHosted {
		plan = "Self-hosted"
	}
	_ = h.pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, plan) VALUES ($1, $2, $3) RETURNING id`,
		tenantPubID, NameFromEmail(email)+"'s workspace", plan).Scan(&tenantID)
	_ = q.EnsureTenantMember(ctx, sqlc.EnsureTenantMemberParams{
		TenantID: tenantID, PersonID: p.ID,
	})
	// Create a default project and issue an API key.
	var projectID int64
	_ = h.pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES ($1, $2, $3) RETURNING id`,
		newUUID(), tenantID, domain).Scan(&projectID)
	_, _ = h.pool.Raw().Exec(ctx,
		`INSERT INTO project_seq (project_id, next) VALUES ($1, 1) ON CONFLICT DO NOTHING`,
		projectID)
	// Issue an API key so the ingest door is open from the first signup.
	keySecret := randomHexAuth(16)
	keyPrefix := keySecret[:12]
	keyFull := "uc_live_" + keySecret
	keyHash := sha256.Sum256([]byte(keyFull))
	_, _ = h.pool.Raw().Exec(ctx,
		`INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash, state) VALUES ($1, $2, $3, $4, 'active')`,
		tenantID, projectID, keyPrefix, keyHash[:])
	// An account is born with a way to be told: the email channel is seeded
	// once at creation; NOT EXISTS keeps it idempotent on retry.
	_, _ = h.pool.Raw().Exec(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, kind, target)
		 SELECT gen_random_uuid(), $1, 'email', $2
		  WHERE NOT EXISTS (SELECT 1 FROM alert_channel WHERE tenant_id = $1 AND kind = 'email' AND target = $2)`,
		tenantID, email)
	// Fired only on the NEW-account path: the funnel's account_created step,
	// scoped to whichever door created the account.
	h.rec.ServerEvent(ctx, "account_created", p.ID, tenantID, nil)
	h.rec.MarkAccountCreated(ctx, p.ID, tenantID)
	return personInfo{
		ID: p.ID, PublicID: p.PublicID,
		Name: p.Name, Email: ptrStr(p.Email),
		TenantID: tenantID,
	}, nil
}

func (h *MagicLink) tenantForPerson(ctx context.Context, personID int64) (int64, error) {
	var tenantID int64
	err := h.pool.Raw().QueryRow(ctx,
		`SELECT tenant_id FROM tenant_member WHERE person_id = $1 ORDER BY tenant_id LIMIT 1`,
		personID).Scan(&tenantID)
	return tenantID, err
}

// activateInvites is the proof-of-ownership half of an invitation, called only
// from the redeem doors: pending memberships activate, tenants get the channel.
func (h *MagicLink) activateInvites(ctx context.Context, personID int64, email string) {
	// Defensive, same reason as in ensureAccount: the NOT EXISTS below
	// compares target byte for byte, so the one spelling is the normalised one.
	email = NormalizeEmail(email)
	rows, err := h.pool.Raw().Query(ctx,
		`UPDATE tenant_member SET status = 'active' WHERE person_id = $1 AND status = 'pending' RETURNING tenant_id`,
		personID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var tenantID int64
		if err := rows.Scan(&tenantID); err != nil {
			return
		}
		_, _ = h.pool.Raw().Exec(ctx,
			`INSERT INTO alert_channel (public_id, tenant_id, kind, target)
			 SELECT gen_random_uuid(), $1, 'email', $2
			  WHERE NOT EXISTS (SELECT 1 FROM alert_channel WHERE tenant_id = $1 AND kind = 'email' AND target = $2)`,
			tenantID, email)
	}
}

// Me handles GET /v1/me.
type Me struct {
	pool *pg.Pool
	sess *session.Manager
}

func NewMe(p *pg.Pool, sm *session.Manager) *Me { return &Me{pool: p, sess: sm} }

func (h *Me) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, err := h.sess.FromRequest(ctx, r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	// s.TokenHash is the sha256 of the cookie value. Single-user mode carries
	// no token hash, so the aggregate is keyed by identity instead.
	var row sqlc.GetMeRow
	if len(s.TokenHash) == 0 {
		byID, err := h.pool.Queries().GetMeByIdentity(ctx, sqlc.GetMeByIdentityParams{
			PersonID: s.PersonID, TenantID: s.TenantID,
		})
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "no_session")
			return
		}
		row = sqlc.GetMeRow(byID)
	} else {
		var err error
		row, err = h.pool.Queries().GetMe(ctx, s.TokenHash)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "no_session")
			return
		}
	}
	resp := map[string]any{
		"account": map[string]any{
			"id":       uuidStr(row.PersonPublicID),
			"name":     row.PersonName,
			"email":    ptrStr(row.Email),
			"initials": initials(row.PersonName, ptrStr(row.Email)),
			"plan":     row.Plan,
			"billing":  row.Billing,
			// The member's role: notify = read-only everywhere, login/owner =
			// full /app. Web and Mini App gate off this one field.
			"role": row.MemberRole,
		},
		"project": map[string]any{
			"id":        uuidStr(row.ProjectPublicID),
			"domain":    row.ProjectDomain,
			"createdAt": row.ProjectCreatedAt,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// Logout handles POST /v1/auth/logout.
type Logout struct{ sess *session.Manager }

func NewLogout(sm *session.Manager) *Logout { return &Logout{sess: sm} }

func (h *Logout) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(session.CookieName); err == nil && c.Value != "" {
		_ = h.sess.Delete(r.Context(), c.Value)
	}
	session.ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// NotImplemented is the placeholder for the Google + Telegram OAuth endpoints:
// mounted, but answers 501: never a silent 200 that reads as a login.
type NotImplemented struct{ method string }

func NewNotImplemented(method string) *NotImplemented { return &NotImplemented{method: method} }

func (h *NotImplemented) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error": map[string]any{
			"code":    "not_implemented",
			"message": h.method + " login is not available yet — use the magic link.",
		},
	})
}

// codeRecord is the storage-independent view of one outstanding code.
type codeRecord struct {
	Hash     []byte
	Attempts int
	Expires  time.Time
	Redeemed bool
}

// codeRecordFrom maps the stored row to the pure view.
func codeRecordFrom(c sqlc.MagicLinkCode) codeRecord {
	return codeRecord{
		Hash:     c.CodeHash,
		Attempts: int(c.Attempts),
		Expires:  c.ExpiresAt.Time,
		Redeemed: c.RedeemedAt.Valid,
	}
}

// validateCode is the single chokepoint; every failure looks identical to the
// caller (no oracle). Constant-time compare on the hash.
func validateCode(rec codeRecord, submitted string, now time.Time) bool {
	if len(rec.Hash) == 0 || submitted == "" {
		return false
	}
	if now.After(rec.Expires) { // expired
		return false
	}
	if rec.Redeemed { // already used (one-time)
		return false
	}
	if rec.Attempts >= maxAttempts { // guessing cap
		return false
	}
	sum := sha256.Sum256([]byte(submitted))
	return subtle.ConstantTimeCompare(sum[:], rec.Hash) == 1
}

// crossSitePost reports a cross-site POST. Sign-in doors install a session, so
// a browser must send Sec-Fetch-Site and a JSON Content-Type (forms cannot).
func crossSitePost(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site", "none":
		return true
	}
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return !strings.EqualFold(strings.TrimSpace(ct), "application/json")
}

// NormalizeEmail is the one spelling of an address this package stores or looks
// up by: the email column is UNIQUE and byte-exact, casing would split accounts.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]string{"code": msg},
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func genCode() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])[:8]
}

func initials(name, email string) string {
	if name != "" {
		parts := strings.Fields(name)
		if len(parts) >= 2 {
			return strings.ToUpper(parts[0][:1] + parts[1][:1])
		}
		if len(name) >= 2 {
			return strings.ToUpper(name[:2])
		}
	}
	if len(email) >= 2 {
		return strings.ToUpper(email[:2])
	}
	return "U"
}

// NameFromEmail names a new account from the local part of its address; no
// usable local part comes back unchanged rather than empty.
func NameFromEmail(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

func newUUID() pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

func uuidStr(u pgtype.UUID) string {
	return fmt.Sprintf("%x", u.Bytes[:])
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func randomHexAuth(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
