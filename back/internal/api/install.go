// The installer's endpoints: anonymous project mint, claim, install status
// poll and install token mint/redeem.

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// anonCooldown throttles the anonymous mint per IP, in-memory and therefore
// per-replica: two replicas mean at worst two mints per IP per window.
const anonCooldown = 30 * time.Second

// install carries the three handlers' dependencies.
type install struct {
	pool         *pg.Pool
	chc          *ch.Conn
	keys         *pg.KeyResolver
	sess         *session.Manager
	publicOrigin string

	// selfHosted (UC_SELF_HOSTED=1): the anonymous mint answers 404; a
	// self-host has no use-before-signup story.
	selfHosted bool

	mu   sync.Mutex
	last map[string]time.Time
}

func NewInstall(pool *pg.Pool, chc *ch.Conn, sm *session.Manager, publicOrigin string, selfHosted bool) *install {
	return &install{
		pool:         pool,
		chc:          chc,
		keys:         pg.NewKeyResolver(pool, nil),
		sess:         sm,
		publicOrigin: strings.TrimRight(publicOrigin, "/"),
		last:         map[string]time.Time{},
		selfHosted:   selfHosted,
	}
}

func (h *install) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/projects/anonymous" && r.Method == http.MethodPost:
		// A self-host has no use-before-signup story: an anonymous tenant there
		// is an invisible orphan. 404: the door does not exist on this install.
		if h.selfHosted {
			writeAPIErr(w, http.StatusNotFound, "not_found")
			return
		}
		h.anonymous(w, r)
	case r.URL.Path == "/v1/claim" && r.Method == http.MethodPost:
		h.claim(w, r)
	case r.URL.Path == "/v1/install/status" && r.Method == http.MethodGet:
		h.status(w, r)
	case r.URL.Path == "/v1/install/token" && r.Method == http.MethodPost:
		h.issueToken(w, r)
	case r.URL.Path == "/v1/install/redeem" && r.Method == http.MethodPost:
		h.redeem(w, r)
	default:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	}
}

// installTokenTTL: the token only has to survive a copy from browser to
// terminal; a leaked screenshot goes stale before it travels.
const installTokenTTL = 10 * time.Minute

// issueToken mints the one-time token for `npx upcontrol init --token`. A
// bare signed-out init would mint an anonymous project and bypass this account.
func (h *install) issueToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, err := h.sess.FromRequest(ctx, r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	// The session's current project (the tenant's first as the fallback).
	projectID := currentProjectID(ctx, h.pool, s, s.TenantID)
	if projectID == 0 {
		writeAPIErr(w, http.StatusInternalServerError, "no_project")
		return
	}
	token := "uct_" + randomHex()
	hash := sha256.Sum256([]byte(token))
	expires := time.Now().UTC().Add(installTokenTTL)
	if _, err := h.pool.Raw().Exec(ctx,
		`INSERT INTO install_token (tenant_id, project_id, token_hash, expires_at) VALUES ($1, $2, $3, $4)`,
		s.TenantID, projectID, hash[:], expires); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"token":     token,
		"command":   installCommand(token, h.publicOrigin),
		"expiresAt": expires.Format(time.RFC3339),
	})
}

// installCommand builds the copy-paste init line. Off the hosted cloud the
// CLI's default endpoint is the wrong machine: carry the minting origin.
func installCommand(token, publicOrigin string) string {
	cmd := "npx upcontrol init --token " + token
	if publicOrigin != "" && publicOrigin != "https://upcontrol.io" {
		cmd += " --endpoint " + publicOrigin
	}
	return cmd
}

type redeemReq struct {
	Token string `json:"token"`
}

// redeem burns the token and issues an ADDITIONAL api_key, once. Not a
// rotation: the atomic UPDATE makes a replay answer the same 404 (no oracle).
func (h *install) redeem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !h.allow("redeem:" + installClientIP(r)) {
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var req redeemReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Token == "" {
		writeAPIErr(w, http.StatusBadRequest, "missing_token")
		return
	}
	hash := sha256.Sum256([]byte(req.Token))
	var tenantID, projectID int64
	if err := h.pool.Raw().QueryRow(ctx,
		`UPDATE install_token SET used_at = now()
		  WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		 RETURNING tenant_id, project_id`, hash[:]).Scan(&tenantID, &projectID); err != nil {
		writeAPIErr(w, http.StatusNotFound, "invalid_token")
		return
	}
	key, err := issueKey(ctx, h.pool, tenantID, projectID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"key": key})
}

// allow throttles by (bucket:ip) key. Mint and redeem are separate buckets:
// a cooldown that refuses the second half of one flow is not a safeguard.
func (h *install) allow(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if t, ok := h.last[ip]; ok && now.Sub(t) < anonCooldown {
		return false
	}
	// The map only grows while distinct IPs arrive inside the window; sweep
	// opportunistically so a scan cannot leak memory forever.
	if len(h.last) > 4096 {
		for k, t := range h.last {
			if now.Sub(t) > anonCooldown {
				delete(h.last, k)
			}
		}
	}
	h.last[ip] = now
	return true
}

type anonymousReq struct {
	CLIVersion   string `json:"cli_version"`
	AgentVersion string `json:"agent_version"`
	Platform     string `json:"platform"`
	Arch         string `json:"arch"`
}

// unclaimedTenant is a tenant nobody has proved they own yet. Two doors mint
// one — the CLI's anonymous project and the landing's e-mail-less watch — so
// the shape they hand out has to be identical.
//
// Both get a claim token even though only the CLI shows one: a terminal has no
// other credential, while a watch-made page is claimed by slug from the page
// itself. The HASH is what matters on that path — a non-NULL
// claim_token_hash IS the "unclaimed" marker that the watch door's reuse
// lookup and POST /v1/claim both test, and that claiming clears.
type unclaimedTenant struct {
	TenantID     int64
	ProjectID    int64
	ProjectPubID pgtype.UUID
	ClaimToken   string
	Key          string
}

// newUnclaimedTenant creates the tenant, its claim token, its project and its
// API key. name titles the workspace; domain names the project ("" when the
// caller has no host to name it after).
func newUnclaimedTenant(ctx context.Context, pool *pg.Pool, name, domain string) (unclaimedTenant, error) {
	raw := pool.Raw()
	var out unclaimedTenant
	if err := raw.QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES ($1, $2) RETURNING id`,
		newUUID(), name).Scan(&out.TenantID); err != nil {
		return unclaimedTenant{}, err
	}
	out.ClaimToken = randomHex()
	claimHash := sha256.Sum256([]byte(out.ClaimToken))
	if _, err := raw.Exec(ctx,
		`UPDATE tenant SET claim_token_hash = $1 WHERE id = $2`, claimHash[:], out.TenantID); err != nil {
		return unclaimedTenant{}, err
	}
	out.ProjectPubID = newUUID()
	if err := raw.QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES ($1, $2, $3) RETURNING id`,
		out.ProjectPubID, out.TenantID, domain).Scan(&out.ProjectID); err != nil {
		return unclaimedTenant{}, err
	}
	_, _ = raw.Exec(ctx,
		`INSERT INTO project_seq (project_id, next) VALUES ($1, 1) ON CONFLICT DO NOTHING`, out.ProjectID)
	key, err := issueKey(ctx, pool, out.TenantID, out.ProjectID)
	if err != nil {
		return unclaimedTenant{}, err
	}
	out.Key = key
	return out, nil
}

func (h *install) anonymous(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !h.allow("mint:" + installClientIP(r)) {
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	// The body carries versions only (no email, no hostname, no paths), and
	// is never trusted for anything but a log line.
	var req anonymousReq
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)

	t, err := newUnclaimedTenant(ctx, h.pool, "unclaimed", "")
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]any{
		"projectId":  "prj_" + hex.EncodeToString(t.ProjectPubID.Bytes[:6]),
		"key":        t.Key, // shown exactly once, like POST /v1/keys/rotate
		"claimToken": t.ClaimToken,
		"claimUrl":   h.publicOrigin + "/claim/" + t.ClaimToken,
	})
}

type claimReq struct {
	ClaimToken string `json:"claimToken"`
	Slug       string `json:"slug"`
}

func (h *install) claim(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, err := h.sess.FromRequest(ctx, r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	var req claimReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil ||
		(req.ClaimToken == "" && req.Slug == "") {
		writeAPIErr(w, http.StatusBadRequest, "missing_claim_token")
		return
	}
	if req.ClaimToken == "" {
		h.claimBySlug(ctx, w, s, req.Slug)
		return
	}
	// The token only locates the tenant: adoptTenant's conditional burn is
	// what makes every claim single-shot, on both paths.
	hash := sha256.Sum256([]byte(req.ClaimToken))
	var anonTenantID int64
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT id FROM tenant WHERE claim_token_hash = $1`, hash[:]).Scan(&anonTenantID); err != nil {
		writeAPIErr(w, http.StatusNotFound, "invalid_claim_token")
		return
	}
	h.adoptTenant(ctx, w, s, anonTenantID)
}

// claimBySlug claims the UNCLAIMED status page the slug names — open claim,
// first caller wins. The slug only locates the tenant: adoptTenant's
// conditional burn is the lock, so a race loser — or the page's own owner,
// whose token is already burned — answers not_claimable with no special
// case for either.
func (h *install) claimBySlug(ctx context.Context, w http.ResponseWriter, s sqlc.Session, slug string) {
	var anonTenantID int64
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT tenant_id FROM status_page WHERE slug = $1`, slug).Scan(&anonTenantID); err != nil {
		writeAPIErr(w, http.StatusNotFound, "not_claimable")
		return
	}
	h.adoptTenant(ctx, w, s, anonTenantID)
}

// adoptTenant moves the anonymous tenant's whole project footprint into the
// claimer's tenant, then deletes the anonymous tenant. Claim adopts, it never
// adds a membership: a second tenant_member row is what the session lookup
// (ORDER BY tenant_id LIMIT 1) can never see — the bug this rewrites
// (docs/plans/projects-axis.md Decision 6). One transaction; the conditional
// burn is the race lock, exactly as the slug path always had it.
func (h *install) adoptTenant(ctx context.Context, w http.ResponseWriter, s sqlc.Session, anonTenantID int64) {
	// Self-adoption is impossible before anything is touched: an anon id that
	// IS the claimer's own tenant answers the burn's 404 locally, so no future
	// wiring mistake can reparent a tenant onto itself and DELETE it.
	if anonTenantID == s.TenantID {
		writeAPIErr(w, http.StatusNotFound, "not_claimable")
		return
	}
	tx, err := h.pool.Raw().Begin(ctx)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer tx.Rollback(ctx)
	// Burn conditionally: the first claim of this tenant wins, a concurrent
	// or replayed one matches no row. The row is deleted below anyway — the
	// WHERE on the hash is the point, not the columns it sets.
	var burned int64
	if err := tx.QueryRow(ctx,
		`UPDATE tenant SET claim_token_hash = NULL, claimed_at = now()
		  WHERE id = $1 AND claim_token_hash IS NOT NULL
		 RETURNING id`, anonTenantID).Scan(&burned); err != nil {
		writeAPIErr(w, http.StatusNotFound, "not_claimable")
		return
	}
	// Serialize the claimer's tenant for the rest of this transaction: the
	// gate below counts projects, and two claims landing together would each
	// count the other's row as absent and both pass. The anon tenant's burn
	// is the other half of the lock, per anon page.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM tenant WHERE id = $1 FOR UPDATE`, s.TenantID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// Absorb the claimer's empty project (Decision 5): a sign-up-via-claim
	// account's lone project is an unused placeholder, and deleting it frees
	// the Free plan's only slot for the page coming in. Empty is exact — one
	// project, no monitors, and nothing ever ingested through it — and it runs
	// BEFORE the gate so the freed slot counts. The foreign keys do the rest:
	// api_key, project_seq and status_page all cascade off project.
	//
	// `project_seq.next > 1` IS the ingest marker: every project is born at 1
	// and only LeaseSeqBlock (internal/ring/seq) moves it, so an SDK-only
	// account — a key wired up, logs flowing, no monitor ever created — is not
	// mistaken for a placeholder and deleted out from under its own key.
	if _, err := tx.Exec(ctx,
		`DELETE FROM project p
		  WHERE p.tenant_id = $1
		    AND (SELECT count(*) FROM project q WHERE q.tenant_id = $1) = 1
		    AND NOT EXISTS (SELECT 1 FROM monitor m WHERE m.project_id = p.id)
		    AND NOT EXISTS (SELECT 1 FROM project_seq ps
		                     WHERE ps.project_id = p.id AND ps.next > 1)`, s.TenantID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// The projects wall, counted after the absorb: at the limit with a USED
	// project the claim stops here (the rollback rides the defer above); with
	// the just-absorbed empty one it does not. The count rides the open
	// transaction (a pool-bound count would only see the deletion after
	// commit); the wall itself has one owner, projectsRefusalQ.
	if msg, plan := projectsRefusalQ(ctx, h.pool, h.pool.Queries().WithTx(tx), s.TenantID); msg != "" {
		writeUpgradeRequired(w, msg, plan)
		return
	}
	// Reparent the footprint. Everything not on this list — alert channels,
	// delivery_queue, error_alert_state — dies with the anonymous tenant
	// below, deliberately (Decision 6). The table names are a fixed list in
	// this file, never input.
	for _, table := range [...]string{
		"project", "monitor", "incident", "status_page",
		"source_connection", "api_key", "install_token",
	} {
		if _, err := tx.Exec(ctx,
			`UPDATE `+table+` SET tenant_id = $1 WHERE tenant_id = $2`, s.TenantID, anonTenantID); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tenant WHERE id = $1`, anonTenantID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"claimed": true})
}

// requestKey reads the project key from either header the CLI may send
// (X-Upcontrol-Key first, then Authorization bearer), as POST /i accepts.
func requestKey(r *http.Request) string {
	key := r.Header.Get("X-Upcontrol-Key")
	if key == "" {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			key = strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
		}
	}
	return key
}

func (h *install) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key := requestKey(r)
	if key == "" {
		writeAPIErr(w, http.StatusUnauthorized, "missing_key")
		return
	}
	tenant, err := h.keys.Resolve(ctx, key)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "bad_key")
		return
	}

	sig, err := h.pool.Queries().TenantSignals(ctx, tenant.TenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	qb := query.New(tenant.TenantID, sig.ProjectID, sig.CutoffSeq)

	verified := false
	var verifiedAt time.Time
	var lines uint64
	recent := make([]map[string]any, 0, 12)
	if h.chc != nil {
		var times uint64
		var firstTS, lastTS time.Time
		eq := qb.EventSeen("install_verified")
		if err := h.chc.Raw().QueryRow(ctx, eq.SQL, eq.Args...).Scan(&times, &firstTS, &lastTS); err == nil && times > 0 {
			verified = true
			verifiedAt = firstTS
		}
		sq := qb.Summary()
		var lastLine time.Time
		_ = h.chc.Raw().QueryRow(ctx, sq.SQL, sq.Args...).Scan(&lines, &lastLine)
		rq := qb.RecentEvents(15*time.Minute, 12)
		if rows, err := h.chc.Raw().Query(ctx, rq.SQL, rq.Args...); err == nil {
			for rows.Next() {
				var msg string
				var times uint64
				var last time.Time
				if rows.Scan(&msg, &times, &last) == nil {
					recent = append(recent, map[string]any{
						"name":   msg,
						"count":  times,
						"lastAt": last.UTC().Format(time.RFC3339),
					})
				}
			}
			_ = rows.Close()
		}
	}

	resp := map[string]any{
		"verified": verified,
		"lines":    lines,
		"recent":   recent,
	}
	if verified {
		resp["verifiedAt"] = verifiedAt.UTC().Format(time.RFC3339)
	}
	writeAPIJSON(w, http.StatusOK, resp)
}

// installClientIP mirrors auth.clientIP without exporting it: the throttle
// wants the peer address, not a spoofable header.
func installClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
