// Write API: channels, recipients, sources CRUD + public endpoints. All
// session-scoped (tenant_id from the cookie), matching mockData.ts shapes.

package api

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"

	"go.upcontrol.io/back/internal/account/auth"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/analytics"
	notifysettings "go.upcontrol.io/back/internal/channel/notify"
	"go.upcontrol.io/back/internal/discover"
	"go.upcontrol.io/back/internal/probe/executor"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// writeAPI handles all the POST/PATCH/DELETE + public endpoints.
type writeAPI struct {
	pool *pg.Pool
	pgs  *pgstore.Store
	sess *session.Manager
	exec *executor.Executor
	// selfHosted (UC_SELF_HOSTED=1): the anonymous watch door provisions
	// 'Self-hosted' tenants, same as the sign-in door.
	selfHosted bool
	// One mutex for the two in-memory per-replica maps below: two replicas
	// give 2× each limit. Keys: "bucket:ip", host.
	checkMu     sync.Mutex
	checkSeenAt map[string]time.Time
	// Answers already computed, keyed by host. A check spends up to
	// discover.MaxRequests of somebody else's bandwidth.
	checkCache map[string]cachedCheck
	// Dev relaxation, and the only one: the anonymous watch echoes the login
	// code it issued. Never true in prod — see publicWatch.
	devMode bool
	// Async analytics recorder. nil (tests, unwired deployments) is a no-op.
	rec *analytics.Recorder
	// Sends the watch door's login code by e-mail. nil means no mailer: prod
	// stores the code unsent; a fresh link still signs the visitor in.
	mailer auth.Mailer
}

// checkCacheTTL is short enough that a reader who just fixed their site sees the
// fix, long enough that a link doing the rounds does not re-probe per visitor.
const checkCacheTTL = 10 * time.Minute

type cachedCheck struct {
	body map[string]any
	at   time.Time
}

func NewWriteAPI(p *pg.Pool, pgs *pgstore.Store, sm *session.Manager, devMode bool, mail auth.Mailer, rec *analytics.Recorder, selfHosted bool) *writeAPI {
	return &writeAPI{pool: p, pgs: pgs, sess: sm, exec: executor.New(), checkSeenAt: map[string]time.Time{}, checkCache: map[string]cachedCheck{}, devMode: devMode, mailer: mail, rec: rec, selfHosted: selfHosted}
}

func (h *writeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Public endpoints don't need a session.
	if strings.HasPrefix(r.URL.Path, "/public/") {
		h.public(w, r)
		return
	}

	// Caddy's on-demand TLS ask. The production Caddyfile does not proxy the
	// /internal/ prefix, so this door answers only inside the compose network;
	// it sits before the session gate because the ask carries no cookie.
	if r.URL.Path == "/internal/domain-allowed" && r.Method == http.MethodGet {
		h.domainAllowed(w, r)
		return
	}

	// Everything else needs a session.
	s, err := h.sess.FromRequest(r.Context(), r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	// Notify members read (GETs below); every mutation needs login.
	if r.Method != http.MethodGet && !roleAtLeastLogin(r.Context(), h.pool, s.PersonID, s.TenantID) {
		writeAPIErr(w, http.StatusForbidden, "notify_role")
		return
	}
	tenantID := s.TenantID

	switch {
	case r.URL.Path == "/v1/channels" && r.Method == http.MethodPost:
		h.createChannel(w, r, tenantID)
	case strings.HasPrefix(r.URL.Path, "/v1/channels/") && r.Method == http.MethodDelete:
		h.deleteChannel(w, r, tenantID)
	case strings.HasPrefix(r.URL.Path, "/v1/channels/") && r.Method == http.MethodPatch:
		h.patchChannel(w, r, tenantID)
	case strings.Contains(r.URL.Path, "/test") && r.Method == http.MethodPost:
		h.testChannel(w, r, tenantID)
	case strings.HasPrefix(r.URL.Path, "/v1/deliveries/") && r.Method == http.MethodGet:
		h.getDelivery(w, r, tenantID)

	// The invite carries the session's person id: the mail names the inviter.
	case r.URL.Path == "/v1/recipients" && r.Method == http.MethodPost:
		h.createRecipient(w, r, tenantID, s.PersonID)
	// Resend is matched before the patch/delete arms and by its suffix, not
	// pathLast: those arms would read "resend" as the person id.
	case strings.HasPrefix(r.URL.Path, "/v1/recipients/") && strings.HasSuffix(r.URL.Path, "/resend") && r.Method == http.MethodPost:
		h.resendInvite(w, r, tenantID, s.PersonID)
	case strings.HasPrefix(r.URL.Path, "/v1/recipients/") && r.Method == http.MethodPatch:
		h.patchRecipient(w, r, tenantID)
	case strings.HasPrefix(r.URL.Path, "/v1/recipients/") && r.Method == http.MethodDelete:
		h.deleteRecipient(w, r, tenantID)

	case strings.Contains(r.URL.Path, "/connect") && r.Method == http.MethodPost:
		h.connectSource(w, r, tenantID)
	case strings.HasPrefix(r.URL.Path, "/v1/sources/") && r.Method == http.MethodDelete:
		h.deleteSource(w, r, tenantID)

	case r.URL.Path == "/v1/status-page":
		switch r.Method {
		case http.MethodGet:
			h.getStatusPage(w, r, tenantID)
		case http.MethodPut:
			h.putStatusPage(w, r, tenantID)
		default:
			writeAPIErr(w, http.StatusMethodNotAllowed, "method_not_allowed")
		}

	case r.URL.Path == "/v1/status-page/domain/verify" && r.Method == http.MethodPost:
		h.verifyStatusPageDomain(w, r, tenantID)

	case r.URL.Path == "/v1/export" && r.Method == http.MethodGet:
		h.exportAll(w, r, tenantID)

	case r.URL.Path == "/v1/project" && r.Method == http.MethodDelete:
		h.deleteProject(w, r, tenantID)

	case r.URL.Path == "/v1/projects" && r.Method == http.MethodGet:
		h.listProjects(w, r, tenantID)
	case r.URL.Path == "/v1/projects" && r.Method == http.MethodPost:
		h.createProject(w, r, tenantID, s)
	case r.URL.Path == "/v1/project/switch" && r.Method == http.MethodPost:
		h.switchProject(w, r, tenantID, s)

	case strings.HasPrefix(r.URL.Path, "/v1/sources/") && r.Method == http.MethodPatch:
		h.patchSource(w, r, tenantID)

	case r.URL.Path == "/v1/logs" && r.Method == http.MethodGet:
		h.getLogs(w, r, tenantID)

	case strings.HasPrefix(r.URL.Path, "/v1/incidents/") && r.Method == http.MethodGet:
		h.getIncident(w, r, tenantID)

	default:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	}
}

func (h *writeAPI) createChannel(w http.ResponseWriter, r *http.Request, tenantID int64) {
	var req struct {
		Kind   string `json:"kind"`
		Target string `json:"target"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	// An e-mail channel may only address an active member of this workspace:
	// anything else makes us a free mailer to strangers.
	if req.Kind == "email" {
		var known bool
		_ = h.pool.Raw().QueryRow(r.Context(),
			`SELECT EXISTS (
			   SELECT 1 FROM tenant_member tm JOIN person p ON p.id = tm.person_id
			    WHERE tm.tenant_id = $1 AND lower(p.email) = lower($2)
			      AND tm.status = 'active')`,
			tenantID, req.Target).Scan(&known)
		if !known {
			writeAPIErr(w, http.StatusBadRequest, "unknown_recipient")
			return
		}
	}
	// Returns the SAME id shape GET /v1/channels hands out (public_id hex):
	// two id shapes for one entity is how callers parsed a uuid as an integer.
	var pubID pgtype.UUID
	_ = h.pool.Raw().QueryRow(r.Context(),
		`INSERT INTO alert_channel (public_id, tenant_id, kind, target) VALUES (gen_random_uuid()::text::uuid, $1, $2, $3) RETURNING public_id`,
		tenantID, req.Kind, req.Target).Scan(&pubID)
	writeAPIJSON(w, http.StatusCreated, map[string]any{
		"id":     uuidStr(pubID),
		"kind":   req.Kind,
		"target": req.Target,
	})
}

// channelRowID resolves a public_id or numeric id to the row's own id.
// Returns 0 when nothing matches: "not yours", never "deleted".
func (h *writeAPI) channelRowID(ctx context.Context, tenantID int64, raw string) int64 {
	if n := parseID(raw); n > 0 {
		var id int64
		_ = h.pool.Raw().QueryRow(ctx,
			`SELECT id FROM alert_channel WHERE id = $1 AND tenant_id = $2`, n, tenantID).Scan(&id)
		return id
	}
	var id int64
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT id FROM alert_channel WHERE public_id = $1 AND tenant_id = $2`,
		parseUUID(raw), tenantID).Scan(&id)
	return id
}

func (h *writeAPI) deleteChannel(w http.ResponseWriter, r *http.Request, tenantID int64) {
	// Deleting the last e-mail channel is allowed: nobody has to be reachable
	// by mail.
	id := h.channelRowID(r.Context(), tenantID, pathLast(r.URL.Path))
	if id == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_channel")
		return
	}
	_, _ = h.pool.Raw().Exec(r.Context(),
		`DELETE FROM alert_channel WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	w.WriteHeader(http.StatusNoContent)
}

// patchChannel changes what the channel is notified about: only `notify`
// is patchable; a chat-set mute window is lifted here, never set.
func (h *writeAPI) patchChannel(w http.ResponseWriter, r *http.Request, tenantID int64) {
	id := h.channelRowID(r.Context(), tenantID, pathLast(r.URL.Path))
	if id == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_channel")
		return
	}
	var req struct {
		Notify notifysettings.Patch `json:"notify"`
		Muted  *bool                `json:"muted"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	// Muting is the reader's act in the chat; only lifting travels this path.
	if req.Muted != nil {
		if *req.Muted {
			writeAPIErr(w, http.StatusBadRequest, "bad_request")
			return
		}
		// The unmute CTE the bot's /unmute runs, keyed by the resolved row:
		// clear the window, release what it parked. The bot keeps its own copy.
		if _, err := h.pool.Raw().Exec(r.Context(),
			`WITH muted AS (
			   SELECT id, muted_until FROM alert_channel
			    WHERE id = $1 AND tenant_id = $2
			      AND muted_until IS NOT NULL
			 ), cleared AS (
			   UPDATE alert_channel SET muted_until = NULL
			    WHERE id IN (SELECT id FROM muted)
			 ), released AS (
			   UPDATE delivery_queue d SET next_try_at = now()
			     FROM muted m
			    WHERE d.channel_id = m.id AND d.state = 'pending'
			      AND d.leased_by IS NULL AND d.class <> 'followup'
			      AND d.next_try_at >= m.muted_until
			 )
			 SELECT count(*) FROM muted`,
			id, tenantID); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	// PAID ONLY: the resolve follow-up answers 402 in the upgrade shape the
	// client reads. The screen's gate is a courtesy; this is the gate.
	if req.Notify.ResolveFollowUp != nil && *req.Notify.ResolveFollowUp {
		if plan, _ := h.pool.Queries().GetTenantPlan(r.Context(), tenantID); plan == "" || plan == "Free" {
			writeAPIJSON(w, http.StatusPaymentRequired, map[string]any{
				"error": map[string]any{
					"code":    "plan_limit_exceeded",
					"message": "The 15-minute follow-up is on every paid plan.",
					"upgrade": map[string]string{"reason": "The 15-minute follow-up is on every paid plan."},
				},
			})
			return
		}
	}
	// Merge onto the row's CURRENT settings: pointer fields tell "not sent"
	// apart from "sent as false". The resolved object is stored and returned.
	var pubID pgtype.UUID
	var kind, target string
	var raw []byte
	_ = h.pool.Raw().QueryRow(r.Context(),
		`SELECT public_id, kind, target, notify FROM alert_channel WHERE id = $1`, id).Scan(&pubID, &kind, &target, &raw)
	resolved := req.Notify.Apply(notifysettings.Resolve(raw))
	buf, _ := json.Marshal(resolved)
	_, _ = h.pool.Raw().Exec(r.Context(),
		`UPDATE alert_channel SET notify = $1 WHERE id = $2`, buf, id)
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"id":     uuidStr(pubID),
		"kind":   kind,
		"target": target,
		"notify": resolved,
	})
}

func (h *writeAPI) testChannel(w http.ResponseWriter, r *http.Request, tenantID int64) {
	// Queue a test delivery through the production path.
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		writeAPIErr(w, http.StatusBadRequest, "bad_path")
		return
	}
	chID := h.channelRowID(r.Context(), tenantID, parts[3])
	if chID == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_channel")
		return
	}
	// The delivery's id travels back so the caller can poll the outcome via
	// GET /v1/deliveries/{id}: "queued" is the only honest synchronous answer.
	var deliveryID int64
	if err := h.pool.Raw().QueryRow(r.Context(),
		`INSERT INTO delivery_queue (tenant_id, channel_id, idem_key, class, payload)
		 VALUES ($1, $2, $3, 'test', '{"title":"Test alert","status":"ok"}')
		 RETURNING id`,
		tenantID, chID, "test-"+strconv.FormatInt(chID, 10)+"-"+strconv.FormatInt(timeNow(), 10)).Scan(&deliveryID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "queue_unavailable")
		return
	}
	writeAPIJSON(w, http.StatusAccepted, map[string]any{
		"id":    strconv.FormatInt(deliveryID, 10),
		"state": "pending",
	})
}

// getDelivery answers GET /v1/deliveries/{id}: the queue's own vocabulary
// (pending/sent/dead); a dead row carries the reason it died.
func (h *writeAPI) getDelivery(w http.ResponseWriter, r *http.Request, tenantID int64) {
	id := parseID(pathLast(r.URL.Path))
	if id == 0 {
		writeAPIErr(w, http.StatusBadRequest, "bad_id")
		return
	}
	var state string
	var deadReason *string
	if err := h.pool.Raw().QueryRow(r.Context(),
		`SELECT state, dead_reason FROM delivery_queue WHERE id = $1 AND tenant_id = $2`,
		id, tenantID).Scan(&state, &deadReason); err != nil {
		writeAPIErr(w, http.StatusNotFound, "no_such_delivery")
		return
	}
	resp := map[string]any{"id": strconv.FormatInt(id, 10), "state": state}
	if deadReason != nil {
		resp["deadReason"] = *deadReason
	}
	writeAPIJSON(w, http.StatusOK, resp)
}

// createRecipient invites one address: membership and invitation mail go in
// one transaction, so a send failure rolls the whole write back (503).
func (h *writeAPI) createRecipient(w http.ResponseWriter, r *http.Request, tenantID, inviterID int64) {
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	// One spelling before storing: person.email is UNIQUE and byte-exact, and
	// NormalizeEmail is the same fold the sign-in doors use.
	req.Email = auth.NormalizeEmail(req.Email)
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		writeAPIErr(w, http.StatusBadRequest, "bad_email")
		return
	}
	if req.Role == "" {
		req.Role = "notify"
	}
	ctx := r.Context()
	tx, err := h.pool.Raw().Begin(ctx)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Find or create person by email.
	var personID int64
	if err := tx.QueryRow(ctx,
		`SELECT id FROM person WHERE email = $1`, req.Email).Scan(&personID); err != nil {
		_ = tx.QueryRow(ctx,
			`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
			req.Email, auth.NameFromEmail(req.Email)).Scan(&personID)
	}
	_, _ = tx.Exec(ctx,
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, $3, 'pending')
		 ON CONFLICT DO NOTHING`, tenantID, personID, req.Role)
	// Read the status inside the same transaction: an already-active invitee
	// needs no mail and no code.
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM tenant_member WHERE tenant_id = $1 AND person_id = $2`,
		tenantID, personID).Scan(&status); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	body := map[string]any{
		"id":     strconv.FormatInt(personID, 10),
		"email":  req.Email,
		"role":   req.Role,
		"status": status,
	}
	if status == "active" {
		if err := tx.Commit(ctx); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		writeAPIJSON(w, http.StatusCreated, body)
		return
	}
	// Minted on the POOL, not this transaction: IssueLoginCode owns its own
	// writes. A rollback leaves a stored-but-unsent code.
	code, err := auth.IssueLoginCode(ctx, h.pool, req.Email, analytics.ClientIP(r))
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		// The caller's IP is past the shared magic-link window: no mail, no
		// membership — the rollback makes "nobody was added" true here too.
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	case errors.Is(err, auth.ErrCodeCooldown):
		// A live code went out moments ago: the invite lands, no second mail
		// is sent.
		if err := tx.Commit(ctx); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		writeAPIJSON(w, http.StatusCreated, body)
		return
	case err != nil:
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var project, invitedBy string
	// The mail names the session's current project (the tenant's first as
	// fallback); a tenant with no project keeps the tenant name, the old
	// LEFT JOIN answer.
	s, _ := h.sess.FromRequest(ctx, r)
	_ = tx.QueryRow(ctx,
		`SELECT COALESCE(NULLIF(p.domain, ''), t.name)
		   FROM tenant t LEFT JOIN project p ON p.tenant_id = t.id
		  WHERE t.id = $1 AND (p.id = $2 OR p.id IS NULL)`,
		tenantID, currentProjectID(ctx, h.pool, s, tenantID)).Scan(&project)
	_ = tx.QueryRow(ctx,
		`SELECT COALESCE(NULLIF(name, ''), email, '') FROM person WHERE id = $1`,
		inviterID).Scan(&invitedBy)
	// A mailer whose relay arrives at runtime reports emptiness through the
	// optional Configured interface; empty takes the no-mailer path.
	mail := h.mailer
	if c, ok := mail.(interface{ Configured(context.Context) bool }); ok && !c.Configured(ctx) {
		mail = nil
	}
	if mail == nil {
		// The deliberate exception to "never log the code": with no mailer the
		// operator's log is the only way in. The response never carries it.
		slog.Warn("invite: no mailer; sign-in code", "email", req.Email, "code", code)
	} else if err := mail.SendInvite(ctx, req.Email, code, project, invitedBy); err != nil {
		// The mail did not leave: the invite rolls back with it, answering 503.
		writeAPIErr(w, http.StatusServiceUnavailable, "email_unavailable")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// Dev-only relaxation: the code rides the response so e2e can accept an
	// invite with no inbox. Never in prod.
	if h.devMode {
		body["dev_token"] = code
	}
	writeAPIJSON(w, http.StatusCreated, body)
}

// resendInvite sends the invitation mail again for a pending membership: no
// writes, so a failed send leaves exactly the state it found.
func (h *writeAPI) resendInvite(w http.ResponseWriter, r *http.Request, tenantID, inviterID int64) {
	ctx := r.Context()
	// The id is one segment up from /resend; pathLast alone would read
	// "resend". Resolution is the patch/delete arms' own: public or row id.
	personID := h.personRowID(ctx, tenantID, pathLast(strings.TrimSuffix(r.URL.Path, "/resend")))
	if personID == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_person")
		return
	}
	// Only a pending membership is a resend target; anything else answers the
	// 404 the patch and delete arms use.
	var email, status string
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT p.email, tm.status FROM person p JOIN tenant_member tm ON tm.person_id = p.id
		  WHERE p.id = $1 AND tm.tenant_id = $2`, personID, tenantID).Scan(&email, &status); err != nil || status != "pending" {
		writeAPIErr(w, http.StatusNotFound, "no_such_person")
		return
	}
	// createRecipient's mint-and-send on the pool: IssueLoginCode owns its
	// tables, and no transaction is held to contend with them.
	code, err := auth.IssueLoginCode(ctx, h.pool, email, analytics.ClientIP(r))
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		// The caller's IP is past the shared magic-link window: no mail, and
		// unlike the invite there is no rollback to make true — nothing moved.
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	case errors.Is(err, auth.ErrCodeCooldown):
		// A resend inside the cooldown answers 429: a 202 would claim "Sent!"
		// while no second mail can go out.
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	case err != nil:
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var project, invitedBy string
	// Same project naming as the invite: the session's current project, the
	// tenant's first as fallback, tenant name when no project exists.
	s, _ := h.sess.FromRequest(ctx, r)
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT COALESCE(NULLIF(p.domain, ''), t.name)
		   FROM tenant t LEFT JOIN project p ON p.tenant_id = t.id
		  WHERE t.id = $1 AND (p.id = $2 OR p.id IS NULL)`,
		tenantID, currentProjectID(ctx, h.pool, s, tenantID)).Scan(&project)
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT COALESCE(NULLIF(name, ''), email, '') FROM person WHERE id = $1`,
		inviterID).Scan(&invitedBy)
	// The same optional-Configured check the magic-link door and the invite
	// make: a runtime-empty relay takes the no-mailer path.
	mail := h.mailer
	if c, ok := mail.(interface{ Configured(context.Context) bool }); ok && !c.Configured(ctx) {
		mail = nil
	}
	if mail == nil {
		// The deliberate exception: with no mailer the operator's log is the
		// only inbox this code will ever reach.
		slog.Warn("invite: no mailer; sign-in code", "email", email, "code", code)
	} else if err := mail.SendInvite(ctx, email, code, project, invitedBy); err != nil {
		writeAPIErr(w, http.StatusServiceUnavailable, "email_unavailable")
		return
	}
	// 202 and an empty body: the row does not change, so the answer is a fact
	// about the mail. Dev mode adds the code, only on paths that minted.
	if h.devMode {
		writeAPIJSON(w, http.StatusAccepted, map[string]any{"dev_token": code})
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// personRowID is channelRowID for People: it accepts the public_id that
// GET /v1/recipients hands out as well as the numeric row id.
func (h *writeAPI) personRowID(ctx context.Context, tenantID int64, raw string) int64 {
	var id int64
	if n := parseID(raw); n > 0 {
		_ = h.pool.Raw().QueryRow(ctx,
			`SELECT tm.person_id FROM tenant_member tm WHERE tm.person_id = $1 AND tm.tenant_id = $2`,
			n, tenantID).Scan(&id)
		return id
	}
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT p.id FROM person p JOIN tenant_member tm ON tm.person_id = p.id
		  WHERE p.public_id = $1 AND tm.tenant_id = $2`,
		parseUUID(raw), tenantID).Scan(&id)
	return id
}

func (h *writeAPI) patchRecipient(w http.ResponseWriter, r *http.Request, tenantID int64) {
	idStr := pathLast(r.URL.Path)
	var req struct {
		Role string `json:"role"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	personID := h.personRowID(r.Context(), tenantID, idStr)
	if personID == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_person")
		return
	}
	_, _ = h.pool.Raw().Exec(r.Context(),
		`UPDATE tenant_member SET role = $1 WHERE person_id = $2 AND tenant_id = $3`,
		req.Role, personID, tenantID)
	writeAPIJSON(w, http.StatusOK, map[string]any{"id": idStr, "role": req.Role})
}

func (h *writeAPI) deleteRecipient(w http.ResponseWriter, r *http.Request, tenantID int64) {
	personID := h.personRowID(r.Context(), tenantID, pathLast(r.URL.Path))
	if personID == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_person")
		return
	}
	// Full revocation, one transaction: membership, destinations, unused
	// invites and sessions die together. The person row stays.
	tx, err := h.pool.Raw().Begin(r.Context())
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	for _, q := range []string{
		`DELETE FROM tenant_member WHERE person_id = $1 AND tenant_id = $2`,
		`DELETE FROM alert_channel WHERE tenant_id = $2 AND kind = 'telegram' AND recipient_person_id = $1`,
		`DELETE FROM alert_channel WHERE tenant_id = $2 AND kind = 'email' AND lower(target) = (SELECT lower(email) FROM person WHERE id = $1)`,
		// Both invite directions: either redeemed after removal would attach a
		// chat to a person no longer here.
		`UPDATE telegram_invite SET expires_at = now()
		  WHERE tenant_id = $2 AND (invited_by = $1 OR person_id = $1) AND redeemed_at IS NULL`,
		`DELETE FROM session WHERE person_id = $1 AND tenant_id = $2`,
	} {
		if _, err := tx.Exec(r.Context(), q, personID, tenantID); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /v1/export: everything this account owns, one JSON document. Secrets
// are not included: an export copies the record, not the credentials.
func (h *writeAPI) exportAll(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	out := map[string]any{
		"exportedAt": time.Now().UTC().Format(time.RFC3339),
		"monitors":   []any{},
		"incidents":  []any{},
		"channels":   []any{},
		"people":     []any{},
	}
	if rows, err := h.pool.Queries().ListMonitorsByTenant(ctx, tenantID); err == nil {
		mons := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			mons = append(mons, map[string]any{
				"name": row.Name, "target": row.Target, "kind": row.Kind,
				"intervalSec": row.IntervalSec, "status": ptrStrSafe(row.Status),
			})
		}
		out["monitors"] = mons
	}
	// SinceDays 0 = no clamp: the export is the tenant's own data — the plan
	// window clamps the screens, never the takeout.
	if rows, err := h.pool.Queries().ListIncidentsByTenant(ctx,
		sqlc.ListIncidentsByTenantParams{TenantID: tenantID, RowLimit: 1000, SinceDays: 0}); err == nil {
		incs := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			inc := map[string]any{"title": row.Title, "status": row.Status, "affected": row.AffectedCount}
			if row.DetectedAt.Valid {
				inc["detectedAt"] = row.DetectedAt.Time.UTC().Format(time.RFC3339)
			}
			if row.ResolvedAt.Valid {
				inc["resolvedAt"] = row.ResolvedAt.Time.UTC().Format(time.RFC3339)
			}
			if lines, lerr := h.pool.Queries().ListIncidentSlice(ctx, row.ID); lerr == nil && len(lines) > 0 {
				slice := make([]string, 0, len(lines))
				for _, l := range lines {
					slice = append(slice, l.Message)
				}
				inc["logSlice"] = slice
			}
			incs = append(incs, inc)
		}
		out["incidents"] = incs
	}
	if rows, err := h.pool.Queries().ListChannelsByTenant(ctx, tenantID); err == nil {
		chans := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			chans = append(chans, map[string]any{"kind": row.Kind, "target": row.Target})
		}
		out["channels"] = chans
	}
	if rows, err := h.pool.Queries().ListRecipientsByTenant(ctx, tenantID); err == nil {
		people := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			people = append(people, map[string]any{
				"email": ptrStrSafe(row.Email), "role": row.Role, "status": row.Status,
			})
		}
		out["people"] = people
	}
	w.Header().Set("Content-Disposition", `attachment; filename="upcontrol-export.json"`)
	writeAPIJSON(w, http.StatusOK, out)
}

// DELETE /v1/project deletes the session's CURRENT project, not the account:
// with several projects on a plan, a confirmation that names one domain may
// not take the other four with it. The LAST project is still how an account
// is closed — the tenant goes, and only then the session cookie — because
// leaving an unreachable tenant behind is not a way out either.
//
// `accountDeleted` says which of the two happened. The caller knows its own
// project count, but it read that BEFORE the write; a destructive action is
// the last place to let the client guess.
func (h *writeAPI) deleteProject(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	s, _ := h.sess.FromRequest(ctx, r)
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	count, err := h.pool.Queries().CountProjectsByTenant(ctx, tenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	last := projectID == 0 || count <= 1

	tx, err := h.pool.Raw().Begin(ctx)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if projectID != 0 {
		if err := releaseProject(ctx, tx, projectID); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if last {
		// The project is out of the way by now, so this cascade takes only what
		// belonged to the ACCOUNT: members, channels, quotas.
		if _, err := tx.Exec(ctx, `DELETE FROM tenant WHERE id = $1`, tenantID); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if last {
		session.ClearCookie(w)
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"accountDeleted": last})
}

// releaseProject hands a project to a fresh UNCLAIMED tenant instead of
// deleting it: **removing a project does not remove its status page — the page
// simply becomes ownerless** (user decision, 2026-08-27). It keeps its slug and
// its address, so a link somebody already holds still resolves, and anyone may
// claim it again through the same door the landing's pages use. This is
// `adoptTenant` run backwards.
//
// What does NOT travel: the api_key and install_token are the person's
// credentials and die here, and the monitors are paused — the owner asked for
// this to stop, and a released page that keeps probing spends our money on
// somebody who left. Channels, members and quotas are tenant-scoped and stay
// with the account, so the page alerts nobody, which is what ownerless means.
//
// Deleting the key is also what lets the reaper collect the page later: its
// exclusion spares an anonymous tenant that has BOTH ingested and still holds a
// key, which is the `uc init` install in use and not this.
func releaseProject(ctx context.Context, tx pgx.Tx, projectID int64) error {
	var domain string
	if err := tx.QueryRow(ctx, `SELECT domain FROM project WHERE id = $1`, projectID).Scan(&domain); err != nil {
		return err
	}
	claimHash := sha256.Sum256([]byte(randomHex()))
	var orphanTenant int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, claim_token_hash)
		 VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		domain, claimHash[:]).Scan(&orphanTenant); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE project SET tenant_id = $1 WHERE id = $2`, orphanTenant, projectID); err != nil {
		return err
	}
	for _, table := range [...]string{"monitor", "status_page", "incident", "source_connection"} {
		if _, err := tx.Exec(ctx,
			`UPDATE `+table+` SET tenant_id = $1 WHERE project_id = $2`, orphanTenant, projectID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE monitor SET paused = true WHERE project_id = $1`, projectID); err != nil {
		return err
	}
	for _, table := range [...]string{"api_key", "install_token"} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE project_id = $1`, projectID); err != nil {
			return err
		}
	}
	return nil
}

func (h *writeAPI) connectSource(w http.ResponseWriter, r *http.Request, tenantID int64) {
	// Persist the connection: records the source_connection row and returns
	// its id so the front's source list reflects it.
	ctx := r.Context()
	parts := strings.Split(strings.TrimRight(r.URL.Path, "/"), "/")
	kind := ""
	if len(parts) >= 2 {
		kind = parts[len(parts)-2]
	}
	// `activate` is the copy button: a plain connect keeps the draft hidden;
	// copying promotes it to the visible card.
	var body struct {
		Activate bool `json:"activate"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	// The connection lands in the session's current project (the tenant's
	// first as the fallback).
	s, _ := h.sess.FromRequest(ctx, r)
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	var id int64
	var paused bool
	var lastSignal pgtype.Timestamptz
	var hookToken string
	// 'draft' hides the row until an arriving event promotes it; ON CONFLICT
	// returns the existing row unchanged (paused, token, status never reset).
	_ = h.pool.Raw().QueryRow(ctx,
		`INSERT INTO source_connection (tenant_id, project_id, kind, status, hook_token) VALUES ($1, $2, $3, 'draft', $4)
		 ON CONFLICT (project_id, kind) DO UPDATE SET kind = source_connection.kind
		 RETURNING id, paused, last_signal_at, hook_token`,
		tenantID, projectID, kind, newHookToken()).Scan(&id, &paused, &lastSignal, &hookToken)
	if body.Activate {
		_, _ = h.pool.Raw().Exec(ctx,
			`UPDATE source_connection SET status = 'waiting' WHERE id = $1 AND status = 'draft'`, id)
	}
	mark := strings.ToUpper(kind)
	if len(mark) > 3 {
		mark = mark[:3]
	}
	// The same rule the list follows: a connection is only "up" once something
	// has arrived through it.
	status, signal := "nodata", "waiting..."
	if lastSignal.Valid {
		status, signal = "ok", agoLabel(lastSignal.Time)
	}
	if paused {
		status, signal = "nodata", "paused"
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"id": "src_" + strconv.FormatInt(id, 10), "mark": mark, "kind": kind,
		"name": sourceName(kind), "status": status, "lastSignal": signal, "paused": paused,
		"hookToken": hookToken,
	})
}

// newHookToken mints the per-connection inbound-hook credential. The URL
// carrying it is the whole auth for the event sink: it must be unguessable.
func newHookToken() string {
	b := make([]byte, 16)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}

func (h *writeAPI) deleteSource(w http.ResponseWriter, r *http.Request, tenantID int64) {
	id := sourceID(pathLast(r.URL.Path))
	if id == 0 {
		// src_checks and src_logs are facts, not connections: there is no row to
		// delete, and answering 204 would tell the screen it removed something.
		writeAPIErr(w, http.StatusBadRequest, "not_disconnectable")
		return
	}
	_, _ = h.pool.Raw().Exec(r.Context(),
		`DELETE FROM source_connection WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	w.WriteHeader(http.StatusNoContent)
}

// PATCH /v1/sources/{id} — pause or resume a connected source. Pausing keeps the
// row: it means "stop reading this for now", not "forget it".
func (h *writeAPI) patchSource(w http.ResponseWriter, r *http.Request, tenantID int64) {
	id := sourceID(pathLast(r.URL.Path))
	if id == 0 {
		writeAPIErr(w, http.StatusBadRequest, "not_pausable")
		return
	}
	var req struct {
		Paused bool `json:"paused"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := h.pool.Queries().SetSourcePaused(r.Context(), sqlc.SetSourcePausedParams{
		Paused: req.Paused, ID: id, TenantID: tenantID,
	}); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"id": pathLast(r.URL.Path), "paused": req.Paused})
}

// sourceID parses the `src_<n>` id the list hands out. The two derived sources
// (`src_checks`, `src_logs`) have no row and return 0.
func sourceID(s string) int64 {
	return parseID(strings.TrimPrefix(s, "src_"))
}

// GET /v1/status-page: what this tenant publishes. The components ARE the
// tenant's checks, and each row's uptime is measured, not typed in.
func (h *writeAPI) getStatusPage(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	s, _ := h.sess.FromRequest(ctx, r)
	cfg, domain, verified := h.statusConfig(ctx, s, tenantID)
	writeAPIJSON(w, http.StatusOK, h.statusPageResponse(ctx, tenantID, cfg, domain, verified))
}

// PUT /v1/status-page: persist the settings. Components are not stored: they
// are the monitors; only the choice to publish is kept.
func (h *writeAPI) putStatusPage(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	var req struct {
		Title         string          `json:"title"`
		Domain        string          `json:"domain"`
		Shown         map[string]bool `json:"shown"`
		ShowNetwork   *bool           `json:"showNetwork"`
		ShowPoweredBy *bool           `json:"showPoweredBy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "bad_body")
		return
	}
	domain, err := normalizeStatusDomain(req.Domain)
	if err != nil {
		writeAPIErr(w, http.StatusBadRequest, "bad_domain")
		return
	}
	s, _ := h.sess.FromRequest(ctx, r)
	cfg, current, verified := h.statusConfig(ctx, s, tenantID)
	if req.Shown != nil {
		cfg.Shown = req.Shown
	}
	if req.ShowNetwork != nil {
		cfg.ShowNetwork = *req.ShowNetwork
	}
	if req.ShowPoweredBy != nil {
		cfg.ShowPoweredBy = *req.ShowPoweredBy
	}
	cfg.Title = req.Title
	// PAID ONLY: the domain is the one setting a plan pays for. Only a CHANGE
	// pays — re-saving the domain already stored is free, like every setting.
	if domain != "" && domain != current {
		var allowed bool
		_ = h.pool.Raw().QueryRow(ctx,
			`SELECT custom_domain FROM plan_entitlement
			  WHERE plan = (SELECT plan FROM tenant WHERE id = $1)`, tenantID).Scan(&allowed)
		if !allowed {
			writeAPIJSON(w, http.StatusPaymentRequired, map[string]any{
				"error": map[string]any{
					"code":    "upgrade_required",
					"message": "A status page on your own domain is on every paid plan.",
					"upgrade": map[string]string{"reason": "A status page on your own domain is on every paid plan."},
				},
			})
			return
		}
	}
	// A proof survives only an unchanged domain: the upsert below resets it
	// when the host moves.
	verified = verified && domain == current
	raw, _ := json.Marshal(cfg)
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	var projectDomain string
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT id, domain FROM project WHERE id = $1`, projectID).Scan(&projectID, &projectDomain)
	// First save of a page that never existed: name it after the domain, not
	// the id. An existing slug is never rewritten.
	if cfg.Slug == "prj-"+strconv.FormatInt(projectID, 10) {
		if claimed := h.claimSlug(ctx, projectDomain, projectID); claimed != "" {
			cfg.Slug = claimed
		}
	}
	// "" is stored as NULL: pages without a domain must never collide on the
	// UNIQUE index.
	var domainVal any
	if domain != "" {
		domainVal = domain
	}
	if _, err := h.pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title, domain, config)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (slug) DO UPDATE SET
		   title = EXCLUDED.title,
		   domain = EXCLUDED.domain,
		   domain_verified_at = CASE WHEN EXCLUDED.domain = status_page.domain
		                             THEN status_page.domain_verified_at END,
		   config = EXCLUDED.config`,
		tenantID, projectID, cfg.Slug, cfg.Title, domainVal, raw); err != nil {
		// The slug conflict is arbitrated above, so a unique violation here is
		// the domain: another page already rides that host.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeAPIErr(w, http.StatusConflict, "domain_taken")
			return
		}
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// DNS that was already in place needs no second step: verify once, so a
	// customer who pre-configured their records saves and is done. Skipped
	// when already verified — a resolver hiccup must not wipe a standing proof.
	if domain != "" && !verified {
		verified = h.verifyStatusDomain(ctx, tenantID, domain)
	}
	writeAPIJSON(w, http.StatusOK, h.statusPageResponse(ctx, tenantID, cfg, domain, verified))
}

// statusPageConfig is the owner's decisions about the page. Everything else on
// it is measured. The domain is deliberately absent: it is not a display
// setting but the host we route by, so it lives in its own column.
type statusPageConfig struct {
	Slug  string `json:"slug"`
	Title string
	Shown map[string]bool `json:"shown"`
	// The network section switch is real: the owner may hide performance metrics.
	ShowNetwork bool `json:"showNetwork"`
	// Honoured only on a self-hosted instance. Somebody running their own copy
	// under the AGPL may take our name off it; a cloud tenant may not, because
	// there the plan buys the page's address and nothing about the branding
	// (owner decision, 2026-08-29). poweredBy() is the one reader.
	ShowPoweredBy bool `json:"showPoweredBy"`
}

// poweredBy answers whether the credit line is published. On the cloud it is
// always yes, whatever is stored or submitted, so a hand-written API call
// cannot buy what no plan sells.
func (h *writeAPI) poweredBy(cfg statusPageConfig) bool {
	return !h.selfHosted || cfg.ShowPoweredBy
}

// statusConfig loads the saved settings, defaulting a page that has never been
// configured to "publish everything" — the page exists to be public. The
// project it reads is the session's current one (the tenant's first as the
// fallback); the gate-authenticated tenantID decides the tenant, so a failed
// session re-read cannot send the lookup to tenant 0. The domain and its proof
// come from the columns, never the config blob.
func (h *writeAPI) statusConfig(ctx context.Context, s sqlc.Session, tenantID int64) (statusPageConfig, string, bool) {
	cfg := statusPageConfig{ShowNetwork: true, ShowPoweredBy: true, Shown: map[string]bool{}}
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	var domain, title, slug *string
	var verifiedAt *time.Time
	var raw []byte
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT p.id, s.slug, s.title, s.domain, s.domain_verified_at, s.config
		   FROM project p LEFT JOIN status_page s ON s.tenant_id = p.tenant_id
		  WHERE p.id = $1`, projectID).Scan(&projectID, &slug, &title, &domain, &verifiedAt, &raw)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	// The stored slug is the address already handed out: it wins over the body
	// and the id-shaped default ("prj-N", which publicStatus also accepts).
	cfg.Slug = "prj-" + strconv.FormatInt(projectID, 10)
	if slug != nil && *slug != "" {
		cfg.Slug = *slug
	}
	if cfg.Shown == nil {
		cfg.Shown = map[string]bool{}
	}
	if title != nil && cfg.Title == "" {
		cfg.Title = *title
	}
	stored := ""
	if domain != nil {
		stored = *domain
	}
	return cfg, stored, verifiedAt != nil
}

// statusPageResponse is the one shape both /v1/status-page handlers answer
// with: the stored decisions plus the measured components and network.
func (h *writeAPI) statusPageResponse(ctx context.Context, tenantID int64, cfg statusPageConfig, domain string, verified bool) map[string]any {
	return map[string]any{
		"slug":           cfg.Slug,
		"title":          cfg.Title,
		"domain":         domain,
		"domainVerified": verified,
		"components":     h.statusComponents(ctx, tenantID, cfg, false),
		"network":        h.statusNetwork(ctx, tenantID),
		"showNetwork":    cfg.ShowNetwork,
		"showPoweredBy":  h.poweredBy(cfg),
	}
}

var errBadStatusDomain = errors.New("unusable status domain")

// normalizeStatusDomain canonicalizes what the owner typed into the host we
// would route and issue a certificate for: scheme, path and port dropped, case
// folded, trailing dot gone. Empty means "no domain" and is valid. Everything
// else is refused: a page here is a SUBDOMAIN (status.example.com), never the
// apex the customer's own site lives on, and our own host is not theirs to
// claim — neither outright nor as a subdomain of it.
func normalizeStatusDomain(raw string) (string, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return "", nil
	}
	labels := strings.Split(s, ".")
	if len(labels) < 3 {
		return "", errBadStatusDomain
	}
	for _, label := range labels {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", errBadStatusDomain
		}
		for _, c := range label {
			if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
				continue
			}
			return "", errBadStatusDomain
		}
	}
	if u, err := url.Parse(os.Getenv("UC_PUBLIC_ORIGIN")); err == nil {
		if ours := strings.ToLower(u.Hostname()); ours != "" && (s == ours || strings.HasSuffix(s, "."+ours)) {
			return "", errBadStatusDomain
		}
	}
	return s, nil
}

// POST /v1/status-page/domain/verify — re-check the stored domain's DNS and
// stamp the proof. DNS that is not ready yet is an expected answer, not a
// server fault: it returns verified:false, never a 500.
func (h *writeAPI) verifyStatusPageDomain(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	s, _ := h.sess.FromRequest(ctx, r)
	_, domain, _ := h.statusConfig(ctx, s, tenantID)
	if domain == "" {
		writeAPIErr(w, http.StatusBadRequest, "no_domain")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"domain":   domain,
		"verified": h.verifyStatusDomain(ctx, tenantID, domain),
	})
}

// verifyStatusDomain resolves the stored domain and our own host and stamps
// domain_verified_at when the two answers intersect. LookupHost, deliberately
// not LookupCNAME: several DNS providers flatten the CNAME away and would
// fail this check while serving the traffic correctly.
func (h *writeAPI) verifyStatusDomain(ctx context.Context, tenantID int64, domain string) bool {
	verified := false
	if u, err := url.Parse(os.Getenv("UC_PUBLIC_ORIGIN")); err == nil && u.Hostname() != "" {
		if ours, err := net.DefaultResolver.LookupHost(ctx, u.Hostname()); err == nil {
			if theirs, err := net.DefaultResolver.LookupHost(ctx, domain); err == nil {
				verified = ipsIntersect(ours, theirs)
			}
		}
	}
	// The CASE with no ELSE yields NULL, so a failed resolve clears the proof
	// in the same statement that would have stamped it.
	_, _ = h.pool.Raw().Exec(ctx,
		`UPDATE status_page SET domain_verified_at = CASE WHEN $1 THEN now() END
		  WHERE tenant_id = $2 AND domain = $3`,
		verified, tenantID, domain)
	return verified
}

// ipsIntersect reports whether two DNS answers share an address: the
// customer's record points where ours does. Linear scans on purpose - a host
// answers with a handful of addresses, and a set would cost more to build.
func ipsIntersect(a, b []string) bool {
	return slices.ContainsFunc(a, func(ip string) bool { return slices.Contains(b, ip) })
}

// The strip's window climbs a ladder as history accumulates (owner decision,
// 2026-08-27): an account with forty minutes of checks is shown its forty
// minutes, never a day of grey. Each rung doubles the window AND the bar count,
// so a bar keeps covering the same slice of time and simply gets thinner. The
// ladder stops at a day — this page answers "how has today gone", and the
// checks table keeps a week only so the window has room to reach 24 h.
var statusWindows = [...]time.Duration{
	time.Hour,
	2 * time.Hour,
	4 * time.Hour,
	8 * time.Hour,
	16 * time.Hour,
	24 * time.Hour,
}

const (
	// The first rung's bar count: 1 h over 12 bars is one bar per five minutes,
	// and every rung above keeps that five minutes by doubling the count.
	statusBarsBase = 12
	// Where the doubling has to stop. Past this the bars are hairlines — on a
	// 350px phone column 96 of them are already under 2px — so the top rungs
	// widen the bucket instead of multiplying bars nobody can hit.
	statusBarsMax = 96
)

// barPlanFor sizes one monitor's strip: how far back it reaches, what one bar
// covers, and how many there are. `oldest` is the first check held (zero when
// there are none).
func barPlanFor(oldest time.Time, intervalSec int32, now time.Time) (window, bucket time.Duration, count int) {
	// A rung is earned by having filled the one below it: an hour of history
	// buys the two-hour window, sixteen hours buy the day.
	window = statusWindows[0]
	if !oldest.IsZero() {
		have := now.Sub(oldest)
		for i, w := range statusWindows[:len(statusWindows)-1] {
			if have >= w {
				window = statusWindows[i+1]
			}
		}
	}

	// A bucket shorter than the check's own interval draws gaps that are not
	// outages: most of those bars would hold no check at all.
	bucket = statusWindows[0] / statusBarsBase
	if interval := time.Duration(intervalSec) * time.Second; interval > bucket {
		bucket = interval
	}
	count = int(window / bucket)
	if count > statusBarsMax {
		count = statusBarsMax
		bucket = window / time.Duration(count)
	}
	// An interval wider than the whole window: one bar, honestly one bar.
	if count < 1 {
		count, bucket = 1, window
	}
	return window, bucket, count
}

// statusComponents renders one component per monitor with measured uptime and
// bars. `publicOnly` drops the owner's unpublished ones.
func (h *writeAPI) statusComponents(ctx context.Context, tenantID int64, cfg statusPageConfig, publicOnly bool) []map[string]any {
	type monRow struct {
		id          int64
		key         string
		name        string
		intervalSec int32
	}
	var mons []monRow
	rows, err := h.pool.Raw().Query(ctx,
		`SELECT id, public_id, name, interval_sec FROM monitor WHERE tenant_id = $1 ORDER BY id`, tenantID)
	if err == nil {
		for rows.Next() {
			var m monRow
			var pub [16]byte
			if rows.Scan(&m.id, &pub, &m.name, &m.intervalSec) == nil {
				m.key = fmt.Sprintf("%x", pub[:])
				mons = append(mons, m)
			}
		}
		rows.Close()
	}
	if len(mons) == 0 {
		return []map[string]any{}
	}

	now := time.Now().UTC()

	// The first check held per monitor: it decides which rung of the ladder the
	// strip is on. Bounded by the retention window, which is all the table has.
	type day struct{ ok, total uint64 }
	oldest := map[int64]time.Time{}
	if h.pgs != nil {
		chRows, cerr := h.pgs.Raw().Query(ctx, `
			SELECT monitor_id, min(ts) FROM checks
			 WHERE tenant_id = $1 AND ts >= now() - INTERVAL '7 days'
			 GROUP BY monitor_id`, tenantID)
		if cerr == nil {
			for chRows.Next() {
				var monID int64
				var first time.Time
				if chRows.Scan(&monID, &first) == nil {
					oldest[monID] = first.UTC()
				}
			}
			chRows.Close()
		}
	}

	// Every strip's plan, and how far back the widest of them reaches — that
	// bounds the one raw query below.
	type barPlan struct {
		bucket time.Duration
		count  int
	}
	plans := map[int64]barPlan{}
	var rawFrom time.Time
	for _, m := range mons {
		window, bucket, count := barPlanFor(oldest[m.id], m.intervalSec, now)
		plans[m.id] = barPlan{bucket: bucket, count: count}
		if from := now.Add(-window); rawFrom.IsZero() || from.Before(rawFrom) {
			rawFrom = from
		}
	}

	// Buckets counted BACKWARDS from now: a probe is not clock-aligned, and
	// wall-clock buckets would draw false gaps.
	recent := map[int64]map[int]day{}
	if h.pgs != nil && !rawFrom.IsZero() {
		chRows, cerr := h.pgs.Raw().Query(ctx, `
			SELECT monitor_id, ts, ok FROM checks
			 WHERE tenant_id = $1 AND ts >= $2`, tenantID, rawFrom)
		if cerr == nil {
			for chRows.Next() {
				var monID int64
				var ts time.Time
				var okFlag bool
				if chRows.Scan(&monID, &ts, &okFlag) != nil {
					continue
				}
				id := monID
				p := plans[id]
				if p.bucket <= 0 {
					continue
				}
				bucket := int(now.Sub(ts.UTC()) / p.bucket)
				if bucket < 0 || bucket >= p.count {
					continue
				}
				m := recent[id]
				if m == nil {
					m = map[int]day{}
					recent[id] = m
				}
				entry := m[bucket]
				entry.total++
				if okFlag {
					entry.ok++
				}
				m[bucket] = entry
			}
			chRows.Close()
		}
	}

	out := make([]map[string]any, 0, len(mons))
	for _, m := range mons {
		shown, ok := cfg.Shown[m.key]
		if !ok {
			shown = true // a new check is published unless the owner says otherwise
		}
		if publicOnly && !shown {
			continue
		}
		p := plans[m.id]
		bars := make([]string, p.count)
		var okTotal, total uint64
		for i := range p.count {
			d := recent[m.id][p.count-1-i] // bucket 0 is the newest, so it lands last
			switch {
			case d.total == 0:
				bars[i] = "nodata"
			case d.ok == d.total:
				bars[i] = "ok"
			case d.ok*100 >= d.total*95:
				bars[i] = "check"
			default:
				bars[i] = "down"
			}
			okTotal += d.ok
			total += d.total
		}
		out = append(out, map[string]any{
			"key": m.key, "name": m.name, "shown": shown,
			"uptime": pctLabelAPI(okTotal, total), "bars": bars,
			// What one bar covers. The page multiplies it by the bar count to
			// print its own axis, so a strip can never be labelled a window it
			// does not reach.
			"barSpanSec": int(p.bucket / time.Second),
		})
	}
	return out
}

// pctLabelAPI is read_api's pctLabel — same rendering, same "—" for no data.
func pctLabelAPI(ok, total uint64) string { return pctLabel(ok, total) }

// statusNetwork renders the probe phases as medians over the retained window:
// three tiles, DNS, TCP and RESPONSE. The handshake is deliberately absent
// (owner decision, 2026-08-27) — do not add a TLS tile back thinking it was
// dropped by accident. tls_ms is still measured and still stored; it is only
// not published here.
func (h *writeAPI) statusNetwork(ctx context.Context, tenantID int64) []map[string]any {
	if h.pgs == nil {
		return []map[string]any{}
	}
	var dns, connect, total float64
	var samples uint64
	err := h.pgs.Raw().QueryRow(ctx, `
		SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY dns_ms),
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY connect_ms),
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY total_ms),
		       count(*)
		  FROM checks
		 WHERE tenant_id = $1 AND ts >= now() - INTERVAL '24 hours' AND ok
		   AND region <> 'heartbeat'`,
		tenantID).Scan(&dns, &connect, &total, &samples)
	if err != nil || samples == 0 {
		// Nothing measured in the window: no tiles. An empty section is the
		// honest answer for an account whose first probe has not run yet.
		return []map[string]any{}
	}
	rows := []struct {
		label string
		ms    float64
		note  string
	}{
		{"dns", dns, "name lookup"},
		{"tcp", connect, "connection"},
		{"response", total, "start to finish"},
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		// Zero is silence: a reused connection records no lookup and no connect.
		// Only `response` is always real.
		if r.ms == 0 && r.label != "response" {
			continue
		}
		out = append(out, map[string]any{
			"label": r.label,
			"value": msLabel(r.ms),
			"note":  r.note,
			// Green is a claim about health, and a timing is not one: whether the
			// site is up is the components section's job.
			"status": "ok",
		})
	}
	return out
}

// msLabel writes a median the way the landing does: a measured sub-millisecond
// phase is "<1 ms", never "0 ms", because the probe times in whole milliseconds.
func msLabel(ms float64) string {
	if ms < 1 {
		return "<1 ms"
	}
	return fmt.Sprintf("%d ms", int(ms+0.5))
}

// logQueryBuilder binds the tenant's ring cutoff: the window is whatever
// project_window.cutoff_seq is; below it is displaced, not served.
func (h *writeAPI) logQueryBuilder(ctx context.Context, r *http.Request, tenantID int64) *query.QueryBuilder {
	s, _ := h.sess.FromRequest(ctx, r)
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	var cutoffSeq int64
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT p.id, COALESCE(pw.cutoff_seq, 0)
		   FROM project p LEFT JOIN project_window pw ON pw.project_id = p.id
		  WHERE p.id = $1`, projectID).Scan(&projectID, &cutoffSeq)
	return query.New(tenantID, projectID, cutoffSeq)
}

func (h *writeAPI) getLogs(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	qb := h.logQueryBuilder(ctx, r, tenantID)
	q := r.URL.Query()
	// Absent or unrecognised window means the whole ring: a bad value must not
	// invent a narrower window than was asked for.
	window := parseLogWindow(q.Get("window"))
	// The dragged range bounds the lines and count, deliberately NOT the
	// volume: the strip is the map the range was picked on.
	within := parseLogRange(q)
	// Both filters are repeatable. A service value may be empty (the
	// unlabelled service is a real row): presence means "filtered".
	levels := parseLogLevels(q["level"])
	services := q["service"]
	lines := h.runLogRows(ctx, qb.Stream(streamLines, levels, services, q.Get("q"), within))
	volume := h.runBucketRows(ctx, qb.Volume(levels, services), "minute")
	total := h.runWindowCount(ctx, qb, countRange(window, within), levels, services, q.Get("q"))
	if lines == nil {
		lines = []map[string]any{}
	}
	if volume == nil {
		volume = []map[string]any{}
	}
	body := map[string]any{
		"lines": lines, "volume": volume,
		// The panel prints "showing N of total" rather than implying the window
		// is what fits on screen.
		"total": total,
	}
	// Sub-minute detail for the held range only; `volume` above stays the
	// whole-ring map. Absent when it adds nothing finer than that map.
	if size := query.DetailBucketSeconds(parseBucketSeconds(q.Get("bucketSeconds")), within); size > 0 {
		if rows := h.runBucketRows(ctx, qb.VolumeDetail(size, within, levels, services), "bucket"); rows != nil {
			body["detail"] = map[string]any{"bucketSeconds": size, "buckets": rows}
		}
	}
	// The window's services, unfiltered: the picker is built from this. Absent
	// when empty; one service is still sent.
	if services := h.runServiceRows(ctx, qb.Services(window)); len(services) > 0 {
		body["services"] = services
	}
	writeAPIJSON(w, http.StatusOK, body)
}

// parseLogLevels keeps the enum deduplicated; picking all three is normalised
// to nil so the builder skips the predicate.
func parseLogLevels(raw []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, level := range raw {
		switch level {
		case "error", "warn", "info":
			if !seen[level] {
				seen[level] = true
				out = append(out, level)
			}
		}
	}
	if len(out) == 3 {
		return nil
	}
	return out
}

// Lines per read of /v1/logs: a cap on one answer, not the window (`total`
// says so), and a DOM budget: the list is not virtualised.
const streamLines = 1000

// parseLogRange reads the `from`/`to` bounds; either may come alone. Anything
// unparseable is dropped, never defaulted: a bad value must not narrow the ask.
func parseBucketSeconds(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func parseLogRange(q url.Values) query.Range {
	var out query.Range
	if t, err := time.Parse(time.RFC3339, q.Get("from")); err == nil {
		out.From = t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, q.Get("to")); err == nil {
		out.To = t.UTC()
	}
	// A range that does not move forwards is a bug on the wire, not a request for
	// zero lines — answering it literally would report an empty window.
	if !out.From.IsZero() && !out.To.IsZero() && !out.To.After(out.From) {
		return query.Range{}
	}
	return out
}

// countRange picks what `total` is counted over: an explicit range always
// wins over the `window` enum; honouring both counts a slice of a slice.
func countRange(window time.Duration, within query.Range) query.Range {
	if within.Bounded() {
		return within
	}
	if window > 0 {
		return query.Range{From: time.Now().UTC().Add(-window)}
	}
	return query.Range{}
}

// parseLogWindow maps the spec's window enum to a duration. Anything outside
// the enum returns 0, which means "the whole ring".
func parseLogWindow(s string) time.Duration {
	switch s {
	case "5m":
		return 5 * time.Minute
	case "15m":
		return 15 * time.Minute
	case "30m":
		return 30 * time.Minute
	case "1h":
		return time.Hour
	case "2h":
		return 2 * time.Hour
	case "24h":
		return 24 * time.Hour
	default:
		return 0
	}
}

// runWindowCount counts the window's lines before the stream limit. Same
// filters and cutoff as Stream: a different predicate would make it a lie.
func (h *writeAPI) runWindowCount(ctx context.Context, qb *query.QueryBuilder, within query.Range, levels, services []string, search string) int {
	if h.pgs == nil {
		return 0
	}
	// The builder owns the predicate so it cannot drift from Stream's.
	lq := qb.WindowCount(within, levels, services, search)
	rows, err := h.pgs.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return 0
	}
	defer rows.Close()
	for rows.Next() {
		var n uint64
		if err := rows.Scan(&n); err != nil {
			continue
		}
		return int(n)
	}
	return 0
}

// runLogRows executes a stream query and maps rows to the front's log-line
// shape; nil on error, so the caller substitutes an empty array.
func (h *writeAPI) runLogRows(ctx context.Context, lq query.LogQuery) []map[string]any {
	if h.pgs == nil {
		return nil
	}
	rows, err := h.pgs.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var seq uint64
		var ts time.Time
		var level, service, message string
		if err := rows.Scan(&seq, &ts, &level, &service, &message); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"seq":     strconv.FormatUint(seq, 10),
			"ts":      ts.UTC().Format(time.RFC3339Nano),
			"level":   level,
			"service": service,
			"message": message,
		})
	}
	return out
}

// runServiceRows executes the window's service tally — the picker's options.
func (h *writeAPI) runServiceRows(ctx context.Context, lq query.LogQuery) []map[string]any {
	if h.pgs == nil {
		return nil
	}
	rows, err := h.pgs.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var name string
		var lines uint64
		if err := rows.Scan(&name, &lines); err != nil {
			continue
		}
		out = append(out, map[string]any{"name": name, "lines": lines})
	}
	return out
}

// runBucketRows executes a histogram query and names the timestamp column for
// the answer (`minute` vs `bucket`): the two measure different widths.
func (h *writeAPI) runBucketRows(ctx context.Context, lq query.LogQuery, key string) []map[string]any {
	if h.pgs == nil || lq.SQL == "" {
		return nil
	}
	rows, err := h.pgs.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var at time.Time
		var level string
		var lines uint64
		if err := rows.Scan(&at, &level, &lines); err != nil {
			continue
		}
		out = append(out, map[string]any{
			key:     at.UTC().Format(time.RFC3339),
			"level": level,
			"lines": lines,
		})
	}
	return out
}

func (h *writeAPI) getIncident(w http.ResponseWriter, r *http.Request, tenantID int64) {
	idStr := pathLast(r.URL.Path)
	// The public id: `incidentToAPI` only ever sends the uuid; the serial id
	// never reaches a caller.
	var title, status string
	var affected int
	if err := h.pool.Raw().QueryRow(r.Context(),
		`SELECT title, status, affected_count FROM incident WHERE public_id = $1 AND tenant_id = $2`,
		parseUUID(idStr), tenantID).Scan(&title, &status, &affected); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("get incident: read failed", "err", err, "tenant_id", tenantID)
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"id":            idStr,
		"title":         title,
		"status":        status,
		"affectedCount": affected,
		"ongoing":       status == "down" || status == "check",
		"timeline":      []any{},
		"logSlice":      []string{},
	})
}

func (h *writeAPI) public(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/public/check" && r.Method == http.MethodPost:
		h.publicCheck(w, r)
	case r.URL.Path == "/public/watch" && r.Method == http.MethodPost:
		h.publicWatch(w, r)
	case r.URL.Path == "/public/track" && r.Method == http.MethodPost:
		h.publicTrack(w, r)
	case r.URL.Path == "/public/status" && r.Method == http.MethodGet:
		h.publicStatus(w, r)
	case strings.HasPrefix(r.URL.Path, "/public/status/") && r.Method == http.MethodGet:
		h.publicStatus(w, r)
	default:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	}
}

// GET /internal/domain-allowed?domain=... — Caddy's on-demand TLS ask: may a
// certificate be issued for this host? 200 with an empty body only when this
// exact domain is stored, verified, and owned by a plan that pays for it;
// anything else is 404. The strictness is load-bearing — no prefix matching,
// no wildcards, no unverified shortcut — because this one answer is what
// keeps Let's Encrypt's rate limits from being spent on hosts nobody proved.
func (h *writeAPI) domainAllowed(w http.ResponseWriter, r *http.Request) {
	var allowed bool
	if err := h.pool.Raw().QueryRow(r.Context(),
		`SELECT EXISTS (
		   SELECT 1 FROM status_page sp
		   JOIN tenant t ON t.id = sp.tenant_id
		   JOIN plan_entitlement pe ON pe.plan = t.plan
		    WHERE sp.domain = $1 AND sp.domain_verified_at IS NOT NULL AND pe.custom_domain)`,
		r.URL.Query().Get("domain")).Scan(&allowed); err != nil || !allowed {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *writeAPI) publicCheck(w http.ResponseWriter, r *http.Request) {
	// The scope (uc_vid cookie + IP + UA) rides the context so the analytics
	// event fired below resolves the same visitor the request carried.
	ctx := analytics.WithScope(r.Context(), analytics.ScopeFromRequest(r))
	var req struct {
		Host string `json:"host"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Host == "" {
		writeAPIErr(w, http.StatusBadRequest, "missing_host")
		return
	}
	host := req.Host
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	// A fresh answer is served to everyone. Checked before the IP cooldown:
	// a cached answer costs the far side nothing.
	if cached, ok := h.cachedCheck(host); ok {
		h.rec.ServerEvent(ctx, "public_check_run", 0, 0, map[string]string{
			"host": bareHost(host), "cached": "true",
		})
		writeAPIJSON(w, http.StatusOK, cached)
		return
	}
	// Anonymous endpoint: throttled per source-IP, per-replica (two replicas
	// admit 2× the cooldown).
	if !h.checkAllow(analytics.ClientIP(r), host) {
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	// Real probe behind the SSRF guard: internal ranges answer
	// error_class=blocked_target. CollectBody feeds page discovery's fallback.
	res := h.exec.Execute(ctx, executor.CheckSpec{
		URL: host, Method: "GET", TimeoutMs: 8000, MaxRedirects: 3,
		CollectExpiry: true, CollectBody: true,
	})
	h.rec.ServerEvent(ctx, "public_check_run", 0, 0, map[string]string{
		"host": bareHost(host), "cached": "false",
	})
	status := "ok"
	if !res.OK {
		status = "down"
	}
	meta := fmt.Sprintf("%d ms · HTTP %d", res.TotalMs, res.StatusCode)
	if !res.OK {
		meta = fmt.Sprintf("%d ms · %s", res.TotalMs, res.ErrorClass)
	}
	if res.ErrorClass == "blocked_target" {
		meta = "blocked — internal address refused"
	}
	// Facts one request cannot answer, on the same host and behind the same
	// guard. Bounded, not a crawl: see internal/discover.
	facts := discover.Run(r.Context(), h.exec, net.DefaultResolver, host, res)
	network := append(networkRowsFrom(res, status), discoveredRows(facts)...)

	// Every pickable row's id IS its URL, so the watch request can send the
	// ids back as targets with no lookup table in between.
	groups := []map[string]any{
		{"title": "Live probe", "source": "from a real request", "rows": []map[string]any{
			{"id": host, "name": host, "meta": meta, "status": status, "recommended": res.OK},
		}},
	}
	// Reading order, widest first: the typed address, then its other hosts,
	// then pages (an api. host is its own failure domain).
	if rows := pageRows(facts.Hosts); len(rows) > 0 {
		groups = append(groups, map[string]any{
			"title": "Hosts", "source": facts.Hosts[0].Source, "rows": rows,
		})
	}
	// The API carries a checkbox like Hosts' rows: a path-based API is the
	// same thing on a site that routes instead of subdomaining.
	if row := apiRow(host, facts.API); row != nil {
		groups = append(groups, map[string]any{
			"title": "API", "source": facts.API.Source, "rows": []map[string]any{row},
		})
	}
	if rows := pageRows(facts.Pages); len(rows) > 0 {
		groups = append(groups, map[string]any{
			"title": "Pages", "source": facts.Pages[0].Source, "rows": rows,
		})
	}
	body := map[string]any{
		"groups":        groups,
		"networkChecks": network,
		// Raw result exposed for transparency (and so the SSRF acceptance is
		// observable in the response: blocked_target shows up here verbatim).
		"probe": map[string]any{
			"ok":          res.OK,
			"status_code": res.StatusCode,
			"error_class": res.ErrorClass,
			"total_ms":    res.TotalMs,
		},
	}
	if stages := stagesFrom(res); len(stages) > 0 {
		body["stages"] = stages
	}
	// How many rows an account can watch, sent by the server: the plan's
	// numbers live in one place.
	body["watchLimit"] = h.freeWatchLimit(r.Context())

	// A CLAIMED page for the host, when one exists: the landing then opens it
	// instead of promising a watch it cannot perform.
	var claimedSlug string
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT sp.slug FROM status_page sp
		   JOIN tenant t ON t.id = sp.tenant_id
		   JOIN project p ON p.id = sp.project_id
		  WHERE p.domain = $1 AND t.claim_token_hash IS NULL
		  ORDER BY sp.id LIMIT 1`, bareHost(host)).Scan(&claimedSlug); err == nil {
		body["claimedSlug"] = claimedSlug
	}

	h.cacheCheck(host, body)
	writeAPIJSON(w, http.StatusOK, body)
}

// freeWatchLimit is what a brand-new account may watch. The anonymous flow can
// only ever create a Free tenant, so that is the plan to ask about.
func (h *writeAPI) freeWatchLimit(ctx context.Context) int32 {
	limit, err := h.pool.Queries().GetPlanHTTPChecks(ctx, "Free")
	if err != nil || limit <= 0 {
		return 3
	}
	return limit
}

// stagesFrom turns phase timings into the waterfall. TTFBMs is measured from
// request start and already holds dns+tcp+tls, so wait/html are derived.
func stagesFrom(res executor.Result) []map[string]any {
	stages := make([]map[string]any, 0, 5)
	add := func(label string, ms uint32) {
		if ms > 0 {
			stages = append(stages, map[string]any{"label": label, "ms": ms})
		}
	}
	// A zero from the recorder means the hook never fired: no name to resolve, no
	// handshake. Those phases did not happen, so they are omitted.
	add("dns", res.DNSMs)
	add("tcp", res.ConnectMs)
	add("tls", res.TLSMs)

	// The derived two ARE measured once a response arrived: emitted at 0
	// rather than dropped. The guards prevent uint32 underflow.
	preTTFB := res.DNSMs + res.ConnectMs + res.TLSMs
	if res.TTFBMs >= preTTFB {
		stages = append(stages, map[string]any{"label": "wait", "ms": res.TTFBMs - preTTFB})
	}
	if res.TotalMs >= res.TTFBMs {
		stages = append(stages, map[string]any{"label": "html", "ms": res.TotalMs - res.TTFBMs})
	}
	return stages
}

// networkRowsFrom reports one row per fact the probe established. Facts it
// cannot measure yet are absent; the landing renders absence as unknown.
func networkRowsFrom(res executor.Result, status string) []map[string]any {
	rows := make([]map[string]any, 0, 4)
	if res.DNSAddrs > 0 {
		rows = append(rows, map[string]any{
			"label": "dns", "value": fmt.Sprintf("%d ms", res.DNSMs),
			"note": pluralAddrs(res.DNSAddrs), "status": "ok",
		})
	}
	if res.TLSVersion != "" {
		note, tlsStatus := "certificate expiry not read", "ok"
		if !res.SSLExpiresAt.IsZero() {
			days := int(time.Until(res.SSLExpiresAt).Hours() / 24)
			note = fmt.Sprintf("certificate expires %s, in %d days", res.SSLExpiresAt.Format("Jan 2"), days)
			// The landing's own threshold for "look at this soon".
			if days <= 40 {
				tlsStatus = "check"
			}
		}
		rows = append(rows, map[string]any{
			"label": "tls", "value": res.TLSVersion, "note": note, "status": tlsStatus,
		})
	}
	// RESPONSE is the whole request as the visitor experiences it.
	responseNote := fmt.Sprintf("HTTP %d", res.StatusCode)
	if !res.OK {
		responseNote = res.ErrorClass
	}
	rows = append(rows, map[string]any{
		"label": "response", "value": fmt.Sprintf("%d ms", res.TotalMs),
		"note": responseNote, "status": status,
	})
	// Zero hops is a measured fact (the URL answered directly), not an absence,
	// so this row is always sent once the request itself completed.
	if res.ErrorClass != "blocked_target" {
		rows = append(rows, map[string]any{
			"label": "redirects", "value": fmt.Sprintf("%d hops", res.RedirectCount),
			"note": redirectNote(res.RedirectCount), "status": "ok",
		})
	}
	return rows
}

// pageRows renders discovered pages as pickable rows; `recommended` is the
// server's opinion, so the landing keeps no list of its own.
func pageRows(pages []discover.Page) []map[string]any {
	rows := make([]map[string]any, 0, len(pages))
	for _, p := range pages {
		// Every branch below writes meta, including the default.
		var meta string
		rowStatus := "ok"
		switch {
		case p.Status == 0 && p.Error != "":
			// Asked, nothing came back: a linked host that does not answer is
			// already broken, not a gap.
			meta, rowStatus = p.Source+" · no answer ("+p.Error+")", "down"
		case p.Status == 0:
			// Found, never probed: the budget ran out. Not down.
			meta, rowStatus = p.Source+" · not probed", "nodata"
		case p.Slowest:
			meta, rowStatus = fmt.Sprintf("%s · %d ms, the slowest here", p.Source, p.TotalMs), "check"
		case !p.OK:
			meta, rowStatus = fmt.Sprintf("%s · HTTP %d", p.Source, p.Status), "down"
		default:
			meta = fmt.Sprintf("%s · %d ms", p.Source, p.TotalMs)
		}
		rows = append(rows, map[string]any{
			"id": p.URL, "name": p.Path, "meta": meta, "status": rowStatus,
			// A page that did not answer is a poor default to watch.
			"recommended": p.OK,
		})
	}
	return rows
}

// apiRow renders the API as a pickable row, or nil when there is nothing to
// offer: the no-data marker is for facts, not for things you tick.
func apiRow(host string, a *discover.API) map[string]any {
	if a == nil || a.Path == "" {
		return nil
	}
	note, rowStatus := a.Source, "ok"
	switch {
	case a.Status == 401 || a.Status == 403:
		// This is how we recognised it: an API refusing us is still an API, and
		// a guarded one is worth saying out loud.
		note, rowStatus = fmt.Sprintf("%s · HTTP %d, guarded", a.Source, a.Status), "ok"
	case !a.Confirmed:
		// The base is real but the root does not answer: offer it and do NOT
		// pre-tick it.
		note, rowStatus = fmt.Sprintf("%s · root answers %d, pick an endpoint under it", a.Source, a.Status), "check"
	}
	return map[string]any{
		"id": strings.TrimRight(host, "/") + a.Path, "name": a.Path,
		"meta": note, "status": rowStatus,
		// Only a confirmed endpoint is a sensible default to watch.
		"recommended": a.Confirmed,
	}
}

// discoveredRows renders what discover established; unmeasured facts produce
// no row at all. None of these may guess.
func discoveredRows(f discover.Facts) []map[string]any {
	rows := make([]map[string]any, 0, 3)

	if h := f.Headers; h != nil {
		// HSTS leads because it is the one a reader can act on today; the rest
		// of the sentence is the supporting detail.
		value, headerStatus := "no HSTS", "check"
		if h.HSTS {
			value, headerStatus = "HSTS on", "ok"
		}
		parts := []string{"compression off", "no cache policy"}
		if h.Compression != "" {
			parts[0] = "compression on"
		}
		if h.CacheControl {
			parts[1] = "cache policy set"
		}
		rows = append(rows, map[string]any{
			"label": "headers", "value": value,
			"note": strings.Join(parts, ", "), "status": headerStatus,
		})
	}

	if e := f.ErrorPage; e != nil {
		note, pageStatus := "missing pages answer correctly", "ok"
		if !e.Correct {
			// The whole reason for the row: an uptime checker pointed at any URL
			// of this site reads "fine" straight through an outage.
			note, pageStatus = "should be an error, so checkers will miss outages", "check"
		}
		rows = append(rows, map[string]any{
			"label": "error page", "value": fmt.Sprintf("%d", e.Status),
			"note": note, "status": pageStatus,
		})
	}

	if hl := f.Health; hl != nil {
		// An empty path is measured, so it says "none" and never the unmeasured
		// marker. nodata is the right dot for it: nothing to report, not a fault.
		value, note, healthStatus := hl.Path, "we can watch this directly", "ok"
		if hl.Path == "" {
			value, note, healthStatus = "none", "we will watch the homepage instead", "nodata"
		}
		rows = append(rows, map[string]any{
			"label": "health url", "value": value, "note": note, "status": healthStatus,
		})
	}
	return rows
}

func pluralAddrs(n uint32) string {
	if n == 1 {
		return "1 address"
	}
	return fmt.Sprintf("%d addresses", n)
}

func redirectNote(n uint32) string {
	if n == 0 {
		return "answers directly"
	}
	return "followed to the final URL"
}

// checkAllow applies a per-replica cooldown per source-IP. Host is not part
// of it: repeat visitors get the cached answer instead.
func (h *writeAPI) checkAllow(ip, _ string) bool {
	return h.allowOnce("check", ip, 10*time.Second)
}

// watchAllow throttles the anonymous watch in a bucket of its OWN: a check
// and a watch cost different things; one bucket could not price both.
func (h *writeAPI) watchAllow(ip string) bool {
	return h.allowOnce("watch", ip, 3*time.Second)
}

// trackAllow throttles /public/track: one batch per second per IP. The
// client coalesces, so a compliant visitor sends far less.
func (h *writeAPI) trackAllow(ip string) bool {
	return h.allowOnce("track", ip, time.Second)
}

// publicTrack is the analytics collector: always 204 (429 on limit), and
// the ONLY door that mints the uc_vid cookie; others resolve, never create.
func (h *writeAPI) publicTrack(w http.ResponseWriter, r *http.Request) {
	if !h.trackAllow(analytics.ClientIP(r)) {
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	token, ok := analytics.VisitorToken(r)
	if !ok {
		token = analytics.MintVisitorToken()
		// Secure in prod: TLS ends at the edge so r.TLS is always nil here;
		// dev HTTP would silently drop a Secure cookie.
		analytics.SetVisitorCookie(w, token, !h.devMode)
	}
	events, dropped := analytics.ParseBody(r.Body)
	h.rec.CountInvalid(dropped)
	// A live session stamps person/tenant onto the event rows; a read failure
	// simply means anonymous.
	var personID, tenantID int64
	if s, err := h.sess.FromRequest(r.Context(), r); err == nil {
		personID, tenantID = s.PersonID, s.TenantID
	}
	s := analytics.ScopeFromRequest(r)
	s.Token = token
	ctx := analytics.WithScope(r.Context(), s)
	h.rec.Track(ctx, events, personID, tenantID)
	w.WriteHeader(http.StatusNoContent)
}

// allowOnce is the shared per-replica cooldown: one timestamp per (bucket, ip);
// two replicas admit 2×. Cross-replica windows live in Postgres.
func (h *writeAPI) allowOnce(bucket, ip string, cooldown time.Duration) bool {
	h.checkMu.Lock()
	defer h.checkMu.Unlock()
	now := time.Now()
	key := bucket + ":" + ip
	if t, ok := h.checkSeenAt[key]; ok && now.Sub(t) < cooldown {
		return false
	}
	h.checkSeenAt[key] = now
	if len(h.checkSeenAt) > 512 { // opportunistic GC keeps the map bounded
		for k, t := range h.checkSeenAt {
			if now.Sub(t) > 5*time.Minute {
				delete(h.checkSeenAt, k)
			}
		}
	}
	return true
}

// cachedCheck returns a previous answer for this host, if it is still fresh.
func (h *writeAPI) cachedCheck(host string) (map[string]any, bool) {
	h.checkMu.Lock()
	defer h.checkMu.Unlock()
	entry, ok := h.checkCache[host]
	if !ok || time.Since(entry.at) > checkCacheTTL {
		return nil, false
	}
	return entry.body, true
}

func (h *writeAPI) cacheCheck(host string, body map[string]any) {
	h.checkMu.Lock()
	defer h.checkMu.Unlock()
	now := time.Now()
	h.checkCache[host] = cachedCheck{body: body, at: now}
	if len(h.checkCache) > 256 {
		for k, e := range h.checkCache {
			if now.Sub(e.at) > checkCacheTTL {
				delete(h.checkCache, k)
			}
		}
	}
}

func (h *writeAPI) publicWatch(w http.ResponseWriter, r *http.Request) {
	// A host typed on the landing becomes a real account: with an address the
	// door provisions exactly what the magic link would, so the two doors
	// agree; without one it mints an unclaimed tenant the visitor can claim
	// later, because the result comes first and the account second.
	ctx := analytics.WithScope(r.Context(), analytics.ScopeFromRequest(r))
	var req struct {
		Host    string   `json:"host"`
		Email   string   `json:"email"`
		Targets []string `json:"targets"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Host == "" {
		writeAPIErr(w, http.StatusBadRequest, "missing_host_or_email")
		return
	}
	// A typed-but-malformed address is still an error.
	if req.Email != "" && !strings.Contains(req.Email, "@") {
		writeAPIErr(w, http.StatusBadRequest, "missing_host_or_email")
		return
	}
	// A self-host has no use-before-signup story: an anonymous tenant there
	// is an invisible orphan.
	if req.Email == "" && h.selfHosted {
		writeAPIErr(w, http.StatusBadRequest, "missing_email")
		return
	}
	if !h.watchAllow(analytics.ClientIP(r)) {
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}

	target := req.Host
	if !strings.Contains(target, "://") {
		target = "https://" + target
	}
	// The target is probed by the same guarded executor as any check, so an
	// internal address cannot be turned into a monitor by typing it here.
	if res := h.exec.Execute(ctx, executor.CheckSpec{
		URL: target, Method: "GET", TimeoutMs: 8000, MaxRedirects: 3,
	}); res.ErrorClass == "blocked_target" {
		writeAPIErr(w, http.StatusBadRequest, "blocked_target")
		return
	}

	// Decision 7: the e-mail-less arm reads the session. A signed-in visitor
	// whose plan has room for another project gets it in their own tenant
	// below; signed out, or no room, keeps the anonymous demo mint. An error
	// is simply no session — the door is public, a missing cookie must never
	// fail it.
	s, serr := h.sess.FromRequest(ctx, r)

	// The project is named after the site asked about; an existing account
	// keeps the name it has.
	host := bareHost(req.Host)
	var tenantID, projectID int64
	if req.Email != "" {
		_, tid, err := auth.Provision(ctx, h.pool, req.Email, host, h.rec, h.selfHosted)
		if err != nil || tid == 0 {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		tenantID = tid
		h.rec.LinkEmail(ctx, req.Email)
		_ = h.pool.Raw().QueryRow(ctx,
			`SELECT id FROM project WHERE tenant_id = $1 ORDER BY id LIMIT 1`, tenantID).Scan(&projectID)
	} else {
		// One page per host at this anonymous door. The ORDER BY prefers a
		// CLAIMED tenant over an unclaimed one, so the claimed answer wins when
		// both somehow exist — and a claimed host is never re-minted or added
		// to: reusing it would let anyone who types the domain add monitors to
		// somebody's account.
		var claimed bool
		err := h.pool.Raw().QueryRow(ctx,
			`SELECT t.id, p.id, (t.claim_token_hash IS NULL)
			   FROM tenant t
			   JOIN project p ON p.tenant_id = t.id
			  WHERE p.domain = $1
			  ORDER BY (t.claim_token_hash IS NULL) DESC, p.id LIMIT 1`, host).Scan(&tenantID, &projectID, &claimed)
		if err == nil && claimed {
			var slug string
			if serr := h.pool.Raw().QueryRow(ctx,
				`SELECT slug FROM status_page WHERE project_id = $1 ORDER BY id LIMIT 1`, projectID).Scan(&slug); serr == nil {
				// Nothing was created — no monitors, no channel, no event — so
				// nothing is counted: only the existing page's address.
				writeAPIJSON(w, http.StatusOK, map[string]any{
					"statusUrl": "/status/" + slug,
					"slug":      slug,
					"watching":  0,
					"login":     map[string]any{},
				})
				return
			}
			// A claimed tenant with no status page: nothing to point at, mint
			// as if the host were new.
		}
		if err != nil || claimed {
			// The mint arm is where the session matters (Decision 7): a host
			// nobody holds, typed by a signed-in visitor with room for one more
			// project, becomes a project in their OWN tenant — no demo page to
			// claim later, and the monitors and status page below land on the
			// caller's account from the first check. Everything else keeps the
			// demo mint exactly as today: signed out, no room (the wall is on
			// the claim button, never on the check), and a claimed domain
			// without a page — somebody already owns that one.
			own := false
			if err != nil && serr == nil && s.TenantID != 0 {
				if msg, _ := h.projectsRefusal(ctx, s.TenantID); msg == "" {
					pid, perr := createTenantProject(ctx, h.pool, s.TenantID, host)
					if perr == nil {
						tenantID, projectID = s.TenantID, pid
						own = true
						// Decision 18: the caller just created this project;
						// the app must open on it. Same pick as createProject,
						// and like it the pick never gates the response.
						if s.ID != 0 {
							_ = h.pool.Queries().SetSessionProject(ctx, sqlc.SetSessionProjectParams{
								ID: s.ID, ProjectID: &pid,
							})
						}
					}
				}
			}
			if !own {
				t, terr := newUnclaimedTenant(ctx, h.pool, host, host)
				if terr != nil {
					writeAPIErr(w, http.StatusInternalServerError, "internal")
					return
				}
				tenantID, projectID = t.TenantID, t.ProjectID
			}
		}
	}
	// Both paths count: this is the funnel's step between check_run and
	// signed_in. The event goes to Postgres with the host only; the e-mail,
	// when there is one, went to the visitor row above and nowhere else.
	h.rec.ServerEvent(ctx, "watch_signup", 0, 0, map[string]string{"host": host})

	// The client's target list is not trusted: each must belong to the asked
	// host, or this becomes a probe-enrolment service for strangers.
	wanted := append([]string{target}, sameHostTargets(target, req.Targets)...)
	limit := int(h.freeWatchLimit(ctx))
	if len(wanted) > limit {
		wanted = wanted[:limit]
	}

	watching := 0
	for _, t := range wanted {
		var monitorID int64
		_ = h.pool.Raw().QueryRow(ctx,
			`SELECT id FROM monitor WHERE tenant_id = $1 AND target = $2`, tenantID, t).Scan(&monitorID)
		if monitorID != 0 {
			watching++
			continue
		}
		if err := h.pool.Raw().QueryRow(ctx,
			`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec)
			 VALUES (gen_random_uuid(), $1, $2, 'website', $3, $4, 300) RETURNING id`,
			tenantID, projectID, monitorName(host, t), t).Scan(&monitorID); err == nil {
			_ = h.pool.Queries().EnsureMonitorSchedule(ctx, sqlc.EnsureMonitorScheduleParams{
				MonitorID: monitorID, Region: scheduleRegion(),
			})
			watching++
		}
	}

	// The e-mail channel is the point of leaving an address: without it the
	// account would watch the host and tell nobody.
	if req.Email != "" {
		_, _ = h.pool.Raw().Exec(ctx,
			`INSERT INTO alert_channel (public_id, tenant_id, kind, target)
			 SELECT gen_random_uuid(), $1, 'email', $2
			  WHERE NOT EXISTS (SELECT 1 FROM alert_channel WHERE tenant_id = $1 AND kind = 'email' AND target = $2)`,
			tenantID, req.Email)
	}

	// The page's public address is the site's name, not our internal id.
	slug := h.claimSlug(ctx, host, projectID)
	if slug == "" {
		slug = "prj-" + strconv.FormatInt(projectID, 10)
	}
	// Components ARE the monitors created above. ON CONFLICT DO NOTHING: a
	// handed-out slug must not change under a returning visitor.
	_, _ = h.pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title)
		 VALUES ($1, $2, $3, $4) ON CONFLICT (slug) DO NOTHING`,
		tenantID, projectID, slug, host)
	// A project that already had a page keeps ITS slug, whatever we just picked.
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT slug FROM status_page WHERE project_id = $1 ORDER BY id LIMIT 1`, projectID).Scan(&slug)

	// A way in: the sign-in door's own code, e-mail only in prod (handing it
	// back anonymously is account takeover). Dev echoes it. Without an address
	// there is nobody to tell and nobody to let in: login stays empty.
	login := map[string]any{}
	if req.Email != "" {
		if code, cerr := auth.IssueLoginCode(ctx, h.pool, req.Email, analytics.ClientIP(r)); cerr == nil {
			// Best-effort in both modes: dev never depends on an inbox, and in
			// prod a stored-but-undelivered code just waits on the retry.
			if h.mailer != nil {
				if serr := h.mailer.SendCode(ctx, req.Email, code); serr != nil {
					slog.Warn("watch: login code stored but not delivered", "email", req.Email, "err", serr)
				}
			}
			if h.devMode {
				login["dev_token"] = code
			}
		}
	}

	writeAPIJSON(w, http.StatusOK, map[string]any{
		"statusUrl": "/status/" + slug,
		"slug":      slug,
		"watching":  watching,
		"login":     login,
	})
}

// createTenantProject provisions a project for a tenant that already exists —
// the signed-in watch door's mint: newUnclaimedTenant's triple (project,
// project_seq, api_key) minus the tenant and the claim token, in one
// transaction like POST /v1/projects, so a failure mid-provision leaves no
// half-made project eating a plan slot. The gate is the caller's: this door
// checks it once, before choosing between this and the demo mint.
func createTenantProject(ctx context.Context, pool *pg.Pool, tenantID int64, domain string) (int64, error) {
	tx, err := pool.Raw().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var projectID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES ($1, $2, $3) RETURNING id`,
		newUUID(), tenantID, domain).Scan(&projectID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO project_seq (project_id, next) VALUES ($1, 1) ON CONFLICT DO NOTHING`, projectID); err != nil {
		return 0, err
	}
	// The key insert is tx-bound (createProject's pattern): a pool-bound
	// issueKey would autocommit outside the transaction.
	secret := randomHex()
	hash := sha256.Sum256([]byte("uc_live_" + secret))
	if _, err := pool.Queries().WithTx(tx).CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
		TenantID:   tenantID,
		ProjectID:  projectID,
		Prefix:     secret[:12],
		SecretHash: hash[:],
	}); err != nil {
		return 0, err
	}
	return projectID, tx.Commit(ctx)
}

// sameHostTargets keeps only targets belonging to the asked host: without it
// a posted URL enrols a stranger's endpoint into the probe schedule forever.
func sameHostTargets(base string, targets []string) []string {
	root, err := url.Parse(base)
	if err != nil {
		return nil
	}
	seen := map[string]bool{base: true}
	out := make([]string, 0, len(targets))
	for _, raw := range targets {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		// The host or one of its subdomains, suffix-matched with the dot, so
		// "harpa.ai.evil.com" cannot pass as a subdomain of "harpa.ai".
		if u.Host != root.Host && !strings.HasSuffix(u.Host, "."+root.Host) {
			continue
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			continue
		}
		u.Fragment, u.RawQuery = "", ""
		clean := u.String()
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}

// slugFromHost turns a host into the page's public slug (harpa.ai becomes
// harpa-ai). Formatting only: uniqueness is the caller's job.
func slugFromHost(host string) string {
	var b strings.Builder
	prevDash := true // leading dashes are never written
	for _, r := range strings.ToLower(bareHost(host)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	// A host of nothing but punctuation, or an IDN we cannot spell in ASCII,
	// leaves an empty string; the caller falls back to the project id.
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	return slug
}

// claimSlug returns a free slug for host, or "" to fall back to the project
// id. The suffix is random: a counter would leak who watches what.
func (h *writeAPI) claimSlug(ctx context.Context, host string, projectID int64) string {
	return claimSlugFor(ctx, h.pool, host, projectID)
}

// claimSlugFor is the pool-level claim both doors share: the watch door at
// provisioning, the sign-in door when its project is named.
func claimSlugFor(ctx context.Context, pool *pg.Pool, host string, projectID int64) string {
	base := slugFromHost(host)
	if base == "" {
		return ""
	}
	for attempt := 0; attempt < 5; attempt++ {
		candidate := base
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%03d", base, rand.IntN(1000))
		}
		var owner int64
		err := pool.Raw().QueryRow(ctx,
			`SELECT project_id FROM status_page WHERE slug = $1`, candidate).Scan(&owner)
		if err != nil { // no row: free
			return candidate
		}
		if owner == projectID { // already ours, keep it
			return candidate
		}
	}
	return ""
}

// bareHost reduces what the visitor typed to a bare domain: it names their
// project and titles their status page, so no scheme, path or port may ride.
func bareHost(raw string) string {
	h := strings.TrimSpace(raw)
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	if i := strings.IndexAny(h, "/:?#"); i >= 0 {
		h = h[:i]
	}
	return h
}

// The name comes from the TARGET, not the typed address, so api.harpa.ai and
// app.harpa.ai are not both labelled "harpa.ai". `host` is the fallback.
func monitorName(host, target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return host
	}
	if u.Path != "" && u.Path != "/" {
		return u.Host + u.Path
	}
	return u.Host
}

func (h *writeAPI) publicStatus(w http.ResponseWriter, r *http.Request) {
	// The public page shows the same measured components as the config screen,
	// minus the ones the owner unpublished. Nothing here is typed in by hand.
	ctx := r.Context()
	var tenantID int64
	var claimed bool
	// A request without a slug arrives on somebody's custom domain: the Host
	// header (lowercased, port stripped) is the address, and only a domain we
	// have verified answers — an unverified row must render nothing.
	if r.URL.Path == "/public/status" {
		host := strings.ToLower(r.Host)
		if bare, _, err := net.SplitHostPort(host); err == nil {
			host = bare
		}
		if err := h.pool.Raw().QueryRow(ctx,
			`SELECT sp.tenant_id, (t.claim_token_hash IS NULL)
			   FROM status_page sp JOIN tenant t ON t.id = sp.tenant_id
			  WHERE sp.domain = $1 AND sp.domain_verified_at IS NOT NULL`, host).Scan(&tenantID, &claimed); err != nil {
			writeAPIErr(w, http.StatusNotFound, "no_such_page")
			return
		}
		h.renderPublicStatus(w, r, tenantID, claimed)
		return
	}
	slug := pathLast(r.URL.Path)
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT sp.tenant_id, (t.claim_token_hash IS NULL)
		   FROM status_page sp JOIN tenant t ON t.id = sp.tenant_id
		  WHERE sp.slug = $1`, slug).Scan(&tenantID, &claimed); err != nil {
		// A page not yet configured resolves by its project slug. The parsed id
		// is a PROJECT id, kept apart from the tenant id.
		var projectID int64
		if _, perr := fmt.Sscanf(slug, "prj-%d", &projectID); perr != nil || projectID == 0 {
			writeAPIErr(w, http.StatusNotFound, "no_such_page")
			return
		}
		if qerr := h.pool.Raw().QueryRow(ctx,
			`SELECT p.tenant_id, (t.claim_token_hash IS NULL)
			   FROM project p JOIN tenant t ON t.id = p.tenant_id
			  WHERE p.id = $1`, projectID).Scan(&tenantID, &claimed); qerr != nil || tenantID == 0 {
			writeAPIErr(w, http.StatusNotFound, "no_such_page")
			return
		}
	}
	h.renderPublicStatus(w, r, tenantID, claimed)
}

// renderPublicStatus is the tail both doors share once the tenant is found:
// the config load, the components, the incidents, the response map. A slug in
// the path and a Host header differ only in how the tenant is looked up.
func (h *writeAPI) renderPublicStatus(w http.ResponseWriter, r *http.Request, tenantID int64, claimed bool) {
	ctx := r.Context()
	// The public door has no viewer session to honour: the page's tenant
	// decides, and a tenant-only session lands on the lowest project id —
	// exactly the pre-projects pick. A signed-in viewer's own session must
	// not bend somebody else's page toward their current project.
	cfg, storedDomain, _ := h.statusConfig(ctx, sqlc.Session{TenantID: tenantID}, tenantID)
	resp := map[string]any{
		"title":      cfg.Title,
		"components": h.statusComponents(ctx, tenantID, cfg, true),
		"incidents":  []map[string]any{},
		"network":    []map[string]any{},
		"updatedAt":  time.Now().UTC().Format(time.RFC3339),
		"claimed":    claimed,
		"poweredBy":  h.poweredBy(cfg),
	}
	// The viewer's own page says so: mine is present and true only when a
	// session resolves AND belongs to the page's tenant. Absent = not the
	// viewer's page (signed out, or somebody else's); errors are ignored —
	// the door is public and must answer the same either way.
	if vs, verr := h.sess.FromRequest(ctx, r); verr == nil && vs.TenantID == tenantID {
		resp["mine"] = true
		// Same visibility rule, one purpose: the owner reading their own page on
		// our link is the one viewer the upgrade banner speaks to, and it must
		// not sell an address they already own. A visitor never learns this.
		if storedDomain != "" {
			resp["hasCustomDomain"] = true
		}
	}
	// The owner's switch decides whether the section is published at all; what it
	// then shows is measured, never sample data.
	if cfg.ShowNetwork {
		resp["network"] = h.statusNetwork(ctx, tenantID)
	}
	incidents := []map[string]any{}
	if rows, rerr := h.pool.Raw().Query(ctx,
		// monitor_id IS NOT NULL drops incidents of DELETED checks: the
		// component is gone from the page; the detector filter does the rest.
		`SELECT title, status, detected_at FROM incident
		  WHERE tenant_id = $1 AND detector = 'availability' AND monitor_id IS NOT NULL
		  ORDER BY detected_at DESC LIMIT 10`, tenantID); rerr == nil {
		for rows.Next() {
			var title, status string
			var at time.Time
			if rows.Scan(&title, &status, &at) == nil {
				incidents = append(incidents, map[string]any{
					"title": title, "status": status,
					"since":   at.UTC().Format("Jan 2, 15:04"),
					"ongoing": status == "down" || status == "check",
				})
			}
		}
		rows.Close()
	}
	resp["incidents"] = incidents
	writeAPIJSON(w, http.StatusOK, resp)
}

func pathLast(path string) string {
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	return parts[len(parts)-1]
}

func parseID(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func timeNow() int64 {
	return timeNowImpl().Unix()
}

var timeNowImpl = func() interface{ Unix() int64 } {
	return realTime{}
}

type realTime struct{}

func (realTime) Unix() int64 {
	return time.Now().Unix()
}
