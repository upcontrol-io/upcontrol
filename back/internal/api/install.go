// The installer's three endpoints (docs/plans/one-command-install.md, built on
// cli/SPEC.md §7.1/§7.3/§8):
//
//   POST /v1/projects/anonymous — mint a tenant+project+key with no person
//     attached ("use before signup"). The response carries the key ONCE plus a
//     one-time claim token; the CLI writes the key to .env itself, so the key
//     never crosses an agent's context.
//   POST /v1/claim — a signed-in person presents the claim token and becomes a
//     member of the anonymous tenant. One-time; never changes the key.
//   GET /v1/install/status — key-authenticated read the CLI's `verify` polls:
//     has install_verified arrived, how many lines, what names are arriving.
//   PUT /v1/project/meta — key-authenticated spec upload from the installer
//     (ai-provider plan, Decision 15b): five whitelisted fields, capped and
//     scrubbed, stored as Explain context.

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/ingest/scrub"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// anonCooldown throttles the anonymous mint per IP. In-memory and therefore
// per-replica (same documented trade-off as write_api's allowOnce): two
// replicas mean at worst two mints per IP per window, bounded either way.
const anonCooldown = 30 * time.Second

// Install carries the three handlers' dependencies.
type Install struct {
	pool         *pg.Pool
	chc          *ch.Conn
	keys         *pg.KeyResolver
	sess         *session.Manager
	publicOrigin string

	// selfHosted (UC_SELF_HOSTED=1): the anonymous mint answers 404 —
	// a self-host has no use-before-signup story (Decision 22).
	selfHosted bool

	mu   sync.Mutex
	last map[string]time.Time
}

func NewInstall(pool *pg.Pool, chc *ch.Conn, sm *session.Manager, publicOrigin string, selfHosted bool) *Install {
	return &Install{
		pool:         pool,
		chc:          chc,
		keys:         pg.NewKeyResolver(pool, nil),
		sess:         sm,
		publicOrigin: strings.TrimRight(publicOrigin, "/"),
		last:         map[string]time.Time{},
		selfHosted:   selfHosted,
	}
}

func (h *Install) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/projects/anonymous" && r.Method == http.MethodPost:
		// Decision 22 (public-first-split): a self-host has no use-before-signup
		// story, and an anonymous tenant there is an invisible orphan — a bare
		// `npx upcontrol init` would pour logs into an account no one can see.
		// 404: the door does not exist on this install.
		if h.selfHosted {
			writeAPIErr(w, http.StatusNotFound, "not_found")
			return
		}
		h.anonymous(w, r)
	case r.URL.Path == "/v1/claim" && r.Method == http.MethodPost:
		h.claim(w, r)
	case r.URL.Path == "/v1/install/status" && r.Method == http.MethodGet:
		h.status(w, r)
	case r.URL.Path == "/v1/project/meta" && r.Method == http.MethodPut:
		h.setMeta(w, r)
	case r.URL.Path == "/v1/install/token" && r.Method == http.MethodPost:
		h.issueToken(w, r)
	case r.URL.Path == "/v1/install/redeem" && r.Method == http.MethodPost:
		h.redeem(w, r)
	default:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	}
}

// installTokenTTL: the token only has to survive a copy from browser to
// terminal. Long enough for a coffee, short enough that a leaked screenshot
// goes stale before it travels.
const installTokenTTL = 10 * time.Minute

// issueToken is POST /v1/install/token (session-authed): mints the one-time
// token the dashboard's install card embeds in `npx upcontrol init --token …`.
// The dashboard NEVER shows a bare `npx upcontrol` — run signed-out it would
// mint an anonymous project and route this account's logs past it (plan §2.1).
func (h *Install) issueToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, err := h.sess.FromRequest(ctx, r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	var projectID int64
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT id FROM project WHERE tenant_id = $1 ORDER BY id LIMIT 1`, s.TenantID).Scan(&projectID); err != nil {
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

// installCommand builds the copy-paste line the dashboard's install card
// shows. Off the hosted cloud the CLI's default endpoint is the wrong
// machine: without --endpoint a self-host's token travels to upcontrol.io
// and dies as "already used or expired" (cold-install rehearsal) — the
// command must carry the origin it was minted on.
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

// redeem is POST /v1/install/redeem (no session — the CLI calls it from the
// terminal): burns the token and issues an ADDITIONAL api_key for the
// project, returning the secret exactly once. Not a rotation — nothing
// already deployed breaks. Single-use is enforced by the atomic UPDATE, so a
// replayed token gets the same 404 as a wrong one (no oracle).
func (h *Install) redeem(w http.ResponseWriter, r *http.Request) {
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
	key, err := IssueKey(ctx, h.pool, tenantID, projectID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"key": key})
}

// allow throttles by (bucket:ip) key — mint and redeem are separate buckets on
// purpose (the "two anonymous endpoints, two throttle buckets" lesson: a
// cooldown that refuses the second half of one flow is not a safeguard).
func (h *Install) allow(ip string) bool {
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

func (h *Install) anonymous(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !h.allow("mint:" + installClientIP(r)) {
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	// The body carries versions only (SPEC §7.1: no email, no hostname, no
	// paths). It is optional and never trusted for anything but a log line.
	var req anonymousReq
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)

	raw := h.pool.Raw()
	var tenantID int64
	if err := raw.QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES ($1, 'unclaimed') RETURNING id`,
		newUUID()).Scan(&tenantID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	claimToken := randomHex()
	claimHash := sha256.Sum256([]byte(claimToken))
	if _, err := raw.Exec(ctx,
		`UPDATE tenant SET claim_token_hash = $1 WHERE id = $2`, claimHash[:], tenantID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	projectPub := newUUID()
	var projectID int64
	if err := raw.QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES ($1, $2, '') RETURNING id`,
		projectPub, tenantID).Scan(&projectID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	_, _ = raw.Exec(ctx,
		`INSERT INTO project_seq (project_id, next) VALUES ($1, 1) ON CONFLICT DO NOTHING`, projectID)
	key, err := IssueKey(ctx, h.pool, tenantID, projectID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]any{
		"projectId":  "prj_" + hex.EncodeToString(projectPub.Bytes[:6]),
		"key":        key, // shown exactly once, like POST /v1/keys/rotate
		"claimToken": claimToken,
		"claimUrl":   h.publicOrigin + "/claim/" + claimToken,
	})
}

type claimReq struct {
	ClaimToken string `json:"claimToken"`
}

func (h *Install) claim(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, err := h.sess.FromRequest(ctx, r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	var req claimReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.ClaimToken == "" {
		writeAPIErr(w, http.StatusBadRequest, "missing_claim_token")
		return
	}
	hash := sha256.Sum256([]byte(req.ClaimToken))
	var tenantID int64
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT id FROM tenant WHERE claim_token_hash = $1`, hash[:]).Scan(&tenantID); err != nil {
		writeAPIErr(w, http.StatusNotFound, "invalid_claim_token")
		return
	}
	// Membership first, then burn the token: the failure mode of the reverse
	// order is a token spent on a claim that attached nobody.
	if err := h.pool.Queries().EnsureTenantMember(ctx, sqlc.EnsureTenantMemberParams{
		TenantID: tenantID, PersonID: s.PersonID,
	}); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	_, _ = h.pool.Raw().Exec(ctx,
		`UPDATE tenant SET claim_token_hash = NULL, claimed_at = now() WHERE id = $1`, tenantID)
	writeAPIJSON(w, http.StatusOK, map[string]any{"claimed": true})
}

// requestKey reads the project key from either header the CLI may send
// (X-UpControl-Key first, then an Authorization bearer) — the same extraction
// POST /i accepts.
func requestKey(r *http.Request) string {
	key := r.Header.Get("X-UpControl-Key")
	if key == "" {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			key = strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
		}
	}
	return key
}

func (h *Install) status(w http.ResponseWriter, r *http.Request) {
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

// The five fields the installer may send (contract: ProjectMeta schema).
// Anything else in the body is dropped, not rejected — the spec is
// deliberately this narrow (Decision 15b: never versions, paths, git remotes,
// env values or code).
var metaFields = [5]string{"name", "description", "framework", "runtime", "language"}

// metaMaxRunes caps one spec value, counted in runes on the value AS SENT,
// before the scrubber runs. That ordering is deliberate: the scrubber
// EXPANDS (jo@ex.com, 9 runes, becomes [redacted:email:9], 18), so capping
// after it rejected values that were inside the published cap — a compliant
// client got a 400 it could neither act on nor explain. 200 is the number
// ProjectMeta's schema publishes and the only one a caller can honour, so it
// is the number enforced; redaction may push the stored value past it, still
// bounded because every marker replaces text it is derived from. Over-cap is
// a 400 naming the field, never a silent cut (the no-silent-truncation rule).
const metaMaxRunes = 200

// metaNewlines flattens every line break a spec value can carry: \n, \r and
// the Unicode line/paragraph separators U+2028/U+2029, which reach the
// provider as line breaks even though they are not ASCII newlines. The
// escapes are the code points, NOT the digit strings "2028"/"2029" — spelt
// as literals this silently deleted the year out of any project described as
// "Roadmap 2028". The fence tags themselves are neutralized at render time
// (ai.UserMessage), so this strip keeps a stored value one line — it is not
// the injection boundary (Decision 9's store-time half).
var metaNewlines = strings.NewReplacer(
	"\n", "", "\r", "",
	"\u2028", "", "\u2029", "",
)

// metaError is a rejection the response can quote verbatim: code plus the
// sentence that tells the caller what to fix.
type metaError struct {
	code    string
	message string
}

// metaPayload turns a PUT /v1/project/meta body into the stored project.meta
// JSON: only the whitelisted fields survive, each value is capped as sent,
// then scrubbed and flattened to one line (the spec is customer-authored text
// headed for prompts, so it is scrubbed before storage, never on read).
//
// PUT replaces the whole spec — an omitted field is dropped exactly as
// thoroughly as an explicit null would be, which is what PUT means and what
// the installer sends (the complete spec, every run). So the null check below
// is NOT wipe-prevention, and must not be justified as such: it is a type
// check. JSON null decodes into a string as "" with no error, and accepting
// it would record `"name": ""` — a claim that the project is named the empty
// string, which differs from "this spec carries no name".
//
// Pure so the privacy contract is testable without Postgres.
func metaPayload(body []byte, now time.Time) ([]byte, *metaError) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, &metaError{code: "bad_body", message: "body must be a JSON object"}
	}
	out := make(map[string]string, len(metaFields)+2)
	for _, f := range metaFields {
		v, ok := raw[f]
		if !ok {
			continue
		}
		// A *string separates "absent/null" from "a string" using the decoder
		// itself: unmarshalling null into *string yields nil without error,
		// so no raw-bytes literal check is needed beside a working decoder.
		var s *string
		if err := json.Unmarshal(v, &s); err != nil || s == nil {
			return nil, &metaError{code: "bad_body", message: f + " must be a string"}
		}
		if utf8.RuneCountInString(*s) > metaMaxRunes {
			return nil, &metaError{code: "meta_too_large",
				message: fmt.Sprintf("%s exceeds the %d-character cap", f, metaMaxRunes)}
		}
		out[f] = metaNewlines.Replace(scrub.Scrub(*s).Cleaned)
	}
	out["source"] = "installer"
	out["collectedAt"] = now.Format(time.RFC3339)
	b, _ := json.Marshal(out) // string values only: cannot fail
	return b, nil
}

// setMeta is PUT /v1/project/meta: the installer's project-spec upload
// (Decision 15b). Key-authenticated exactly like install status; nothing is
// stored before the whitelist, cap and scrubber have all passed.
func (h *Install) setMeta(w http.ResponseWriter, r *http.Request) {
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
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		writeAPIErr(w, http.StatusBadRequest, "bad_body")
		return
	}
	payload, merr := metaPayload(body, time.Now().UTC())
	if merr != nil {
		writeAPIErrMsg(w, http.StatusBadRequest, merr.code, merr.message)
		return
	}
	if err := h.pool.Queries().SetProjectMeta(ctx, sqlc.SetProjectMetaParams{
		ID: tenant.ProjectID, TenantID: tenant.TenantID, Meta: payload,
	}); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
