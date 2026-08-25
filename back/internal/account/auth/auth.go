// Package auth implements the three login methods (plan §4.1) against the
// session table. The magic-link flow is the simplest and the first implemented.
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

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/analytics"
	"go.upcontrol.io/back/internal/storage/pg"
)

// Magic-link policy. The code is 8 hex chars, lives 10 minutes, and is capped at
// 5 wrong attempts before the row must be reissued. A request for the same email
// within the cooldown is rejected (per-email throttle); the per-IP throttle is a
// sliding window handled in Postgres (magic_link_ip) so it holds across replicas.
const (
	codeTTL       = 10 * time.Minute
	maxAttempts   = 5
	emailCooldown = 60 * time.Second
	ipWindowCap   = 10 // max magic-link requests per IP per 5-minute window
	ipBlockWindow = 5 * time.Minute
)

// Mailer sends the two mails that carry a sign-in credential: the magic-link
// code the sign-in door issues, and the project invitation (§3.2). The real
// implementation ships with the email daemon (block 7); until then it is nil
// and prod log-in waits for that wiring (dev mode returns the code in the
// response, so e2e works).
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
	// 'Self-hosted' plan instead of 'Free' (public-first-split, Decision 7).
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
// creates gets the 'Self-hosted' plan. Chainable, mirrors the mailer's
// WithSignInBase so NewMagicLink's signature (and its test callers) stay put.
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
	// this string, so a request under one spelling and a redeem under another
	// would never find each other.
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
	// Server-side event (plan: product-analytics §Decision 7): fired only when
	// a code was actually issued, not on throttle or failure.
	h.rec.ServerEvent(ctx, "magic_link_requested", 0, 0, nil)

	// A mailer whose relay arrives at runtime (Settings-set SMTP) reports
	// emptiness through the optional Configured interface — the ai.Configured
	// idiom — and an empty one takes the same path as no mailer at all.
	mail := h.mailer
	if c, ok := mail.(interface{ Configured(context.Context) bool }); ok && !c.Configured(ctx) {
		mail = nil
	}

	if h.devMode {
		// Dev only relaxation: echo the code so e2e can log in without email.
		// A configured mailer (the email agent in compose) still gets the send so
		// the whole delivery path is exercised, but dev never depends on an
		// inbox: a send failure is logged, not fatal, and the code rides the
		// response regardless.
		if mail != nil {
			if err := mail.SendCode(ctx, email, code); err != nil {
				h.log.Warn("magic-link: send code failed", "email", email, "err", err)
			}
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"dev_token": code})
		return
	}

	// Prod: deliver the code. Without a mailer the code is stored but unsent —
	// log-in then waits on the email daemon (block 7). Never return the code.
	if mail != nil {
		if err := mail.SendCode(ctx, email, code); err != nil {
			h.log.Warn("magic-link: send code failed", "email", email, "err", err)
			writeErr(w, http.StatusServiceUnavailable, "email_unavailable")
			return
		}
	} else {
		// Decision 17 (public-first-split): the ONE deliberate exception to the
		// "never log the code" rule — with no mailer configured, the operator's
		// log is the only way in. The HTTP response still never carries it.
		h.log.Warn("magic-link: no mailer; sign-in code", "email", email, "code", code)
	}
	w.WriteHeader(http.StatusAccepted)
}

// ErrCodeCooldown means a code issued inside the cooldown is still outstanding.
// Reissuing on demand would let anyone reset somebody else's live code by typing
// their address, so the caller decides what to do: the sign-in door answers 429,
// the watch door just provisions without handing back a token.
var ErrCodeCooldown = errors.New("magic-link: a code was issued recently")

// issueCode stores one fresh code for email and returns it. Both doors go
// through here, so both obey the same TTL, attempt cap and cooldown. It does NOT
// create the account — callers that need one call ensurePerson/ensureAccount
// first, which is also where the project's domain is decided.
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
	// The analytics scope (uc_vid + IP + UA) rides the context down to
	// ensureAccount, so account_created fires with the same visitor identity
	// this sign-in request carried.
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
	// Ownership is proven here and only here: the code was verified above, so
	// every pending invite for this person activates and each such tenant gets
	// the e-mail channel (Decision 18). ensureAccount must never grow this —
	// the request path runs it without any proof the caller owns the address.
	h.activateInvites(ctx, person.ID, email)
	sessToken, err := h.sess.Create(ctx, person.ID, person.TenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	session.SetCookie(w, sessToken, session.DefaultTTL, !h.devMode) // Secure only in prod (dev is HTTP)
	// Visitor-to-account stitching (plan: product-analytics §Decision 5): the
	// sign-in is the moment an anonymous uc_vid becomes a person. No cookie on
	// the request means visitor_id 0 — the events still count, unlinked.
	h.rec.ServerEvent(ctx, "signed_in", person.ID, person.TenantID, nil)
	h.rec.LinkPerson(ctx, person.ID, person.TenantID)
	writeAccount(w, person)
}

// writeAccount is the body every sign-in door answers with. Shared for the same
// reason ensureAccount is: two doors that build the account payload separately
// are two doors that eventually describe the same account differently.
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

// Provision is ensurePerson for callers outside the sign-in flow: the anonymous
// watch on the landing page creates the same account the magic link would, so
// the two doors cannot drift into producing different-looking tenants.
//
// domain names the project of a NEW account — the watch door knows the host the
// visitor asked about, and a workspace called "example.com" for someone who
// watched mysite.io is a lie on every screen. It is ignored when the account
// already exists: a second watch adds monitors, it never renames a project
// somebody is already using.
//
// rec is nil-safe (a nil Recorder is a documented no-op): pass it so the
// new-account path fires account_created with the caller's visitor scope.
func Provision(ctx context.Context, pool *pg.Pool, email, domain string, rec *analytics.Recorder, selfHosted bool) (personID, tenantID int64, err error) {
	h := &MagicLink{pool: pool, rec: rec, selfHosted: selfHosted}
	info, err := h.ensureAccount(ctx, email, domain)
	return info.ID, info.TenantID, err
}

// IssueLoginCode stores a fresh magic-link code for a caller outside the sign-in
// flow and returns it. The anonymous watch uses it: a visitor who just left an
// address needs a way in, and it must be the same one-time, expiring, attempt-
// capped code the sign-in door issues — a second kind of credential is a second
// thing to get wrong.
//
// ip goes through the same cross-replica window the sign-in door records into.
// Without it this would be a second, unmetered way to make us issue codes for
// arbitrary addresses — harmless while they are stored unsent, a mailbomb the
// day the mailer ships.
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

// The sign-in door has no host to name a project after: the visitor typed an
// address, not a site. It leaves the domain EMPTY and the first website check
// names the project (internal/api/monitors.go), which is the same rule the watch
// door follows — it just knows the host up front.
//
// It used to write "example.com" here, and that placeholder was not confined to
// the database: it became the workspace name in the sidebar, the title of the
// status page, the host in the custom-domain hint, the favicon we fetched, and
// — on the first save — the page's public slug `example-com`. A name nobody
// chose, on a page they hand to their customers. Empty renders as nothing, which
// is what we actually know.
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
	// the generous 'Self-hosted' row instead (Decision 7) — through BOTH doors,
	// since Provision and the sign-in redeem share this function.
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
	// An account is born with a way to be told (prod audit §9): the sign-in door
	// used to provision a tenant with zero channels, so the first incident of a
	// new account notified nobody — the watch door seeds this and the sign-in
	// door did not, two doors into different-looking accounts. Seeded exactly
	// once, at creation; an existing account returns above and never lands here,
	// and the NOT EXISTS keeps it idempotent even if creation is ever retried.
	_, _ = h.pool.Raw().Exec(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, kind, target)
		 SELECT gen_random_uuid(), $1, 'email', $2
		  WHERE NOT EXISTS (SELECT 1 FROM alert_channel WHERE tenant_id = $1 AND kind = 'email' AND target = $2)`,
		tenantID, email)
	// Fired only on the NEW-account path (the existing-account return above
	// never lands here): the funnel's account_created step. The scope on ctx
	// comes from whichever door created the account (redeem here, the watch
	// door via Provision) — without it the event still fires at visitor_id 0.
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

// activateInvites is the proof-of-ownership half of an invitation (Decision
// 18): every pending membership of personID turns active, and each such tenant
// gets the person's e-mail alert channel — the same channel ensureAccount
// seeds a new account with, so an invitee becomes as reachable as a signup.
// Called only from the two redeem doors, right after ensurePerson: the request
// path proves nothing about the caller, and a pending membership a bare
// request could activate would be an invite with no accept step.
//
// Errors are swallowed the way ensureAccount's seeds are: the sign-in itself
// already proved the identity, and a failure here must not refuse it. But the
// retry is free only while a row is still pending: a committed UPDATE whose
// channel INSERT then fails leaves nothing to retry on the next sign-in, so
// that channel is recovered by hand — createChannel, which accepts an active
// member's address.
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
	// s.TokenHash is the sha256 of the cookie value — exactly what GetMe
	// expects. In single-user mode (UC_AUTH=none) the session is synthetic
	// and carries NO token hash — there is no session row to join off, so
	// the same aggregate is keyed by the identity instead. Found in the
	// cold-install rehearsal: the token-hash path 401ed the one mode whose
	// whole point is answering without a cookie.
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
			// The member's role (notify = read-only everywhere, login = full /app).
			// The Mini App and the web gate the same screens off this one field.
			// MemberRole: sqlc-generated (gen/pg, GetMe) — notify = read-only,
			// login/owner = full /app, same gate web and Mini App.
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

// --- code verification (pure, unit-tested) ---

// NotImplemented is the honest placeholder for the Google + Telegram OAuth
// endpoints. They are MOUNTED (so all 30 spec paths resolve and the /sign-in
// buttons reach a real response, not a 404) but answer 501 until the OAuth
// wiring ships — never a silent 200 that reads as a successful login.
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

// validateCode checks a submitted code against the stored record. It is the
// single chokepoint for wrong/expired/redeemed/exhausted/replay — every failure
// looks identical to the caller (no oracle). Constant-time compare on the hash.
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
	if rec.Attempts >= maxAttempts { // guessing cap (block-3 policy const)
		return false
	}
	sum := sha256.Sum256([]byte(submitted))
	return subtle.ConstantTimeCompare(sum[:], rec.Hash) == 1
}

// --- helpers ---

// crossSitePost reports whether this POST came from another site.
//
// Every OTHER write in this API is protected from cross-site forgery by
// accident: SameSite=Lax keeps the victim's session cookie off a cross-site
// request, so the request arrives as nobody. The sign-in doors are the
// exception, because they need no cookie from the victim — they INSTALL one.
// An attacker who obtains a credential for an account THEY control (a Google
// code minted against our public client id, or a magic-link code mailed to
// their own address) can cross-site POST it here and leave the victim holding
// a session in the attacker's tenant, reading their monitors and typing into
// their project from then on.
//
// Two checks, either of which is enough on its own:
//
// Sec-Fetch-Site is set by the browser, cannot be forged by the page, and says
// plainly where the request came from. A non-browser client (the CLI, a test,
// curl) omits it entirely and is allowed through — nothing can forge a request
// from a program that has no ambient session to abuse.
//
// The Content-Type is the second lock. A cross-site HTML form can only send
// three encodings, none of them JSON, and the text/plain one is what makes a
// form POST parse as JSON at all. Demanding application/json forces an
// attacker onto fetch(), which then needs a CORS preflight this server never
// answers.
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

// NormalizeEmail is the one spelling of an address this package stores or
// looks anything up by.
//
// The person table's email column is UNIQUE and compared byte for byte, so
// "Ada@example.com" and "ada@example.com" are two accounts with two tenants
// and two sets of monitors. A phone capitalises the first letter of a text
// field by default and Google returns the address in whatever case it holds
// it, which is how the same person arrives spelled two ways through two doors.
// Normalising at every entrance costs nothing and is the only place it can be
// done once.
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

// NameFromEmail names a new account from the local part of its address:
// "ada@example.com" becomes "ada". An address with no usable local part
// comes back unchanged rather than empty, or the person row is created
// nameless.
func NameFromEmail(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

func newUUID() pgtype.UUID {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return pgtype.UUID{Bytes: u, Valid: true}
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
