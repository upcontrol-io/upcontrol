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
	"go.upcontrol.io/back/internal/detect/availability"
	"go.upcontrol.io/back/internal/discover"
	"go.upcontrol.io/back/internal/platform/config"
	"go.upcontrol.io/back/internal/probe/executor"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
	"go.upcontrol.io/back/internal/targetkey"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
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
	// Resolves an ingest key for the key-authenticated board doors (replace,
	// append, the board read). Same pool, no extra wiring.
	keys *pg.KeyResolver
	// The permanent-status-page knobs (config.StatusPageKnobs): the mint
	// ceilings and the index kill switch this door consults. Read from the
	// same env vars ucworker reads; zero values fall back to the defaults
	// where they are used, so a struct built by hand in a test never refuses
	// every mint.
	statusKnobs config.StatusPageKnobs
}

// checkCacheTTL is short enough that a reader who just fixed their site sees the
// fix, long enough that a link doing the rounds does not re-probe per visitor.
const checkCacheTTL = 10 * time.Minute

type cachedCheck struct {
	body map[string]any
	at   time.Time
}

func NewWriteAPI(p *pg.Pool, pgs *pgstore.Store, sm *session.Manager, devMode bool, mail auth.Mailer, rec *analytics.Recorder, selfHosted bool) *writeAPI {
	return &writeAPI{pool: p, pgs: pgs, sess: sm, exec: executor.New(), checkSeenAt: map[string]time.Time{}, checkCache: map[string]cachedCheck{}, devMode: devMode, mailer: mail, rec: rec, selfHosted: selfHosted, keys: pg.NewKeyResolver(p, nil), statusKnobs: config.LoadStatusPageKnobs(nil)}
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

	// The agent's three key-authenticated doors: replace-or-propose and the
	// board read on /v1/dashboard, append on /v1/dashboard/widgets. Taken only
	// when a key is actually presented, so a browser session, which sends no
	// such header, never reaches any of them. The catalog is deliberately not
	// among them: it reports what actually arrived, and the agent builds from
	// what it declared.
	if presentedKey(r) != "" {
		switch {
		case r.URL.Path == "/v1/dashboard" && r.Method == http.MethodPut:
			h.putAgentDashboard(w, r)
			return
		case r.URL.Path == "/v1/dashboard" && r.Method == http.MethodGet:
			h.getAgentDashboard(w, r)
			return
		case r.URL.Path == "/v1/dashboard/widgets" && r.Method == http.MethodPost:
			h.appendDashboardWidgets(w, r)
			return
		}
	}

	// Everything else needs a session.
	s, err := h.sess.FromRequest(r.Context(), r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	// Notify members read (GETs below); every mutation needs login. POST /v1/series
	// is a read that travels as a POST for its body, so it stays open to them.
	// POST /v1/projects is the one write a guest may make: it creates in their
	// OWN workspace, where they are the owner.
	newProject := r.URL.Path == "/v1/projects" && r.Method == http.MethodPost
	if r.Method != http.MethodGet && r.URL.Path != "/v1/series" && !newProject &&
		!canManage(r.Context(), h.pool, s) {
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
		h.exportAll(w, r, tenantID, s)

	case r.URL.Path == "/v1/project" && r.Method == http.MethodDelete:
		h.deleteProject(w, r, tenantID)

	case r.URL.Path == "/v1/projects" && r.Method == http.MethodGet:
		h.listProjects(w, r, s)
	case r.URL.Path == "/v1/projects" && r.Method == http.MethodPost:
		h.createProject(w, r, s)
	case r.URL.Path == "/v1/project/switch" && r.Method == http.MethodPost:
		h.switchProject(w, r, s)

	case strings.HasPrefix(r.URL.Path, "/v1/sources/") && r.Method == http.MethodPatch:
		h.patchSource(w, r, tenantID)

	case r.URL.Path == "/v1/logs" && r.Method == http.MethodGet:
		h.getLogs(w, r, tenantID)

	// The board: what this project sends, then every widget's points in one
	// round trip.
	case r.URL.Path == "/v1/dashboard/catalog" && r.Method == http.MethodGet:
		h.getDashboardCatalog(w, r, tenantID)
	case r.URL.Path == "/v1/series" && r.Method == http.MethodPost:
		h.postSeries(w, r, tenantID)
	// The board itself: one stored layout per project, read by any member
	// and replaced whole by a login member (the gate above). The proposal
	// doors: a notify member may READ what the key offered; dropping it is a
	// write and rides the gate.
	case r.URL.Path == "/v1/dashboard" && r.Method == http.MethodGet:
		h.getDashboard(w, r, tenantID)
	case r.URL.Path == "/v1/dashboard" && r.Method == http.MethodPut:
		h.putDashboard(w, r, tenantID)
	case r.URL.Path == "/v1/dashboard/proposal" && r.Method == http.MethodGet:
		h.getDashboardProposal(w, r, tenantID)
	case r.URL.Path == "/v1/dashboard/proposal" && r.Method == http.MethodDelete:
		h.clearDashboardProposal(w, r, tenantID)

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
	ctx := r.Context()
	s, _ := h.sess.FromRequest(ctx, r)
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	if projectID == 0 {
		writeAPIErr(w, http.StatusBadRequest, "no_project")
		return
	}
	// An e-mail channel may only address the workspace's owner or an active
	// member of THIS project: anything else makes us a free mailer to
	// strangers.
	if req.Kind == "email" {
		var known bool
		_ = h.pool.Raw().QueryRow(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM person p
			    WHERE lower(p.email) = lower($2)
			      AND (EXISTS (SELECT 1 FROM tenant t WHERE t.id = $1 AND t.owner_person_id = p.id)
			           OR EXISTS (SELECT 1 FROM project_member m
			                       WHERE m.project_id = $3 AND m.person_id = p.id AND m.status = 'active')))`,
			tenantID, req.Target, projectID).Scan(&known)
		if !known {
			writeAPIErr(w, http.StatusBadRequest, "unknown_recipient")
			return
		}
	}
	// Returns the SAME id shape GET /v1/channels hands out (public_id hex):
	// two id shapes for one entity is how callers parsed a uuid as an integer.
	var pubID pgtype.UUID
	if err := h.pool.Raw().QueryRow(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, project_id, kind, target) VALUES (gen_random_uuid()::text::uuid, $1, $2, $3, $4) RETURNING public_id`,
		tenantID, projectID, req.Kind, req.Target).Scan(&pubID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusCreated, map[string]any{
		"id":     uuidStr(pubID),
		"kind":   req.Kind,
		"target": req.Target,
	})
}

// channelRowID resolves a public_id or numeric id to the row's own id.
// Returns 0 when nothing matches: "not yours", never "deleted". Both keys
// guard — a sibling project's channel is as foreign as a stranger's.
func (h *writeAPI) channelRowID(ctx context.Context, tenantID, projectID int64, raw string) int64 {
	if n := parseID(raw); n > 0 {
		var id int64
		_ = h.pool.Raw().QueryRow(ctx,
			`SELECT id FROM alert_channel WHERE id = $1 AND tenant_id = $2 AND project_id = $3`,
			n, tenantID, projectID).Scan(&id)
		return id
	}
	var id int64
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT id FROM alert_channel WHERE public_id = $1 AND tenant_id = $2 AND project_id = $3`,
		parseUUID(raw), tenantID, projectID).Scan(&id)
	return id
}

// channelRowIDFor is channelRowID with the request's project resolved.
func (h *writeAPI) channelRowIDFor(ctx context.Context, r *http.Request, tenantID int64, raw string) int64 {
	s, _ := h.sess.FromRequest(ctx, r)
	return h.channelRowID(ctx, tenantID, currentProjectID(ctx, h.pool, s, tenantID), raw)
}

func (h *writeAPI) deleteChannel(w http.ResponseWriter, r *http.Request, tenantID int64) {
	// Deleting the last e-mail channel is allowed: nobody has to be reachable
	// by mail.
	id := h.channelRowIDFor(r.Context(), r, tenantID, pathLast(r.URL.Path))
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
	id := h.channelRowIDFor(r.Context(), r, tenantID, pathLast(r.URL.Path))
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
	chID := h.channelRowIDFor(r.Context(), r, tenantID, parts[3])
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
	s, _ := h.sess.FromRequest(ctx, r)
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	if projectID == 0 {
		writeAPIErr(w, http.StatusBadRequest, "no_project")
		return
	}
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
	// The owner has no membership row and never needs one: inviting the
	// address that already owns the workspace answers with the row it has.
	var ownsIt bool
	_ = tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tenant WHERE id = $1 AND owner_person_id = $2)`,
		tenantID, personID).Scan(&ownsIt)
	if ownsIt {
		var pubID pgtype.UUID
		_ = tx.QueryRow(ctx, `SELECT public_id FROM person WHERE id = $1`, personID).Scan(&pubID)
		writeAPIJSON(w, http.StatusOK, map[string]any{
			"id": uuidStr(pubID), "email": req.Email,
			"role": "login", "status": "active", "owner": true,
		})
		return
	}
	// A person already inside this workspace has been let in once: an active
	// membership is written by the magic-link redeem and by the unbound
	// Telegram redeem, and by nothing else — a BOUND redeem deliberately never
	// writes status. Adding them to a SECOND project is an access decision, not
	// an identity one, so the row lands active with no code minted and no mail
	// sent. Left pending they are refused by createChannel's e-mail gate and
	// never offered by the picker, which reads as "this address cannot be added
	// here". This never mints the first proof, it only carries an existing one
	// across the workspace's own projects. The conflict arm heals a row an
	// earlier invite left pending, and only upward: a stranger's stays pending,
	// and a re-invite is still not a way to re-role anybody.
	_, _ = tx.Exec(ctx,
		`INSERT INTO project_member (project_id, person_id, tenant_id, role, status)
		 SELECT $1, $2, $3, $4,
		        CASE WHEN EXISTS (SELECT 1 FROM project_member m JOIN project pr ON pr.id = m.project_id
		                           WHERE m.person_id = $2 AND pr.tenant_id = $3 AND m.status = 'active')
		             THEN 'active' ELSE 'pending' END
		 ON CONFLICT (project_id, person_id) DO UPDATE SET status = 'active'
		  WHERE project_member.status = 'pending' AND EXCLUDED.status = 'active'`,
		projectID, personID, tenantID, req.Role)
	// Read the status inside the same transaction: an already-active invitee
	// needs no mail and no code.
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM project_member WHERE project_id = $1 AND person_id = $2`,
		projectID, personID).Scan(&status); err != nil {
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
	// The mail names the project the invitation is for; an unnamed project
	// keeps the workspace name, the old LEFT JOIN answer.
	_ = tx.QueryRow(ctx,
		`SELECT COALESCE(NULLIF(p.domain, ''), t.name)
		   FROM project p JOIN tenant t ON t.id = p.tenant_id WHERE p.id = $1`,
		projectID).Scan(&project)
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
	s, _ := h.sess.FromRequest(ctx, r)
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	// The id is one segment up from /resend; pathLast alone would read
	// "resend". Resolution is the patch/delete arms' own: public or row id.
	personID := h.personRowID(ctx, projectID, pathLast(strings.TrimSuffix(r.URL.Path, "/resend")))
	if personID == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_person")
		return
	}
	// Only a pending membership is a resend target; anything else answers the
	// 404 the patch and delete arms use.
	var email, status string
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT p.email, m.status FROM person p JOIN project_member m ON m.person_id = p.id
		  WHERE p.id = $1 AND m.project_id = $2`, personID, projectID).Scan(&email, &status); err != nil || status != "pending" {
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
	// Same project naming as the invite: the project the membership is in,
	// its workspace's name when the project has no domain.
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT COALESCE(NULLIF(p.domain, ''), t.name)
		   FROM project p JOIN tenant t ON t.id = p.tenant_id WHERE p.id = $1`,
		projectID).Scan(&project)
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
// GET /v1/recipients hands out as well as the numeric row id. Keyed by the
// PROJECT's team — the owner has no row here, so patch and delete of the
// owner answer the same 404 a stranger's id does.
func (h *writeAPI) personRowID(ctx context.Context, projectID int64, raw string) int64 {
	var id int64
	if n := parseID(raw); n > 0 {
		_ = h.pool.Raw().QueryRow(ctx,
			`SELECT m.person_id FROM project_member m WHERE m.person_id = $1 AND m.project_id = $2`,
			n, projectID).Scan(&id)
		return id
	}
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT p.id FROM person p JOIN project_member m ON m.person_id = p.id
		  WHERE p.public_id = $1 AND m.project_id = $2`,
		parseUUID(raw), projectID).Scan(&id)
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
	ctx := r.Context()
	s, _ := h.sess.FromRequest(ctx, r)
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	personID := h.personRowID(ctx, projectID, idStr)
	if personID == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_person")
		return
	}
	_, _ = h.pool.Raw().Exec(ctx,
		`UPDATE project_member SET role = $1 WHERE person_id = $2 AND project_id = $3`,
		req.Role, personID, projectID)
	writeAPIJSON(w, http.StatusOK, map[string]any{"id": idStr, "role": req.Role})
}

func (h *writeAPI) deleteRecipient(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	s, _ := h.sess.FromRequest(ctx, r)
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	personID := h.personRowID(ctx, projectID, pathLast(r.URL.Path))
	if personID == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_person")
		return
	}
	// Full revocation from THIS project, one transaction: membership,
	// destinations, unused invites and sessions die together. The person row
	// stays, and so does their standing in the workspace's other projects.
	tx, err := h.pool.Raw().Begin(ctx)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{
		`DELETE FROM project_member WHERE person_id = $1 AND project_id = $2`,
		`DELETE FROM alert_channel WHERE project_id = $2 AND kind = 'telegram' AND recipient_person_id = $1`,
		`DELETE FROM alert_channel WHERE project_id = $2 AND kind = 'email' AND lower(target) = (SELECT lower(email) FROM person WHERE id = $1)`,
		// Both invite directions: either redeemed after removal would attach a
		// chat to a person no longer here.
		`UPDATE telegram_invite SET expires_at = now()
		  WHERE project_id = $2 AND (invited_by = $1 OR person_id = $1) AND redeemed_at IS NULL`,
		`DELETE FROM session WHERE person_id = $1 AND project_id = $2`,
	} {
		if _, err := tx.Exec(ctx, q, personID, projectID); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /v1/export: everything this account owns, one JSON document. Secrets
// are not included: an export copies the record, not the credentials. The
// takeout is the workspace's, so only its owner may take it — an Admin
// invited into one project is not the account.
func (h *writeAPI) exportAll(w http.ResponseWriter, r *http.Request, tenantID int64, s sqlc.Session) {
	ctx := r.Context()
	if !isOwner(ctx, h.pool, s) {
		writeAPIErr(w, http.StatusForbidden, "owner_only")
		return
	}
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
	// Owner only: an Admin invited into the project may run it, not end it.
	if !isOwner(ctx, h.pool, s) {
		writeAPIErr(w, http.StatusForbidden, "owner_only")
		return
	}
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
	for _, table := range [...]string{"monitor", "status_page", "incident", "source_connection", "dashboard"} {
		if _, err := tx.Exec(ctx,
			`UPDATE `+table+` SET tenant_id = $1 WHERE project_id = $2`, orphanTenant, projectID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE monitor SET paused = true WHERE project_id = $1`, projectID); err != nil {
		return err
	}
	// The owner's own rows, now that a channel, an invite, a team and the
	// scanner's memory all belong to ONE project: they stay with the account
	// that is leaving, not with the ownerless page.
	for _, table := range [...]string{
		"api_key", "install_token",
		"alert_channel", "telegram_invite", "project_member", "error_alert_state",
	} {
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
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	cfg, domain, verified, page := h.statusConfig(ctx, projectID)
	writeAPIJSON(w, http.StatusOK, h.statusPageResponse(ctx, tenantID, projectID, cfg, domain, verified, page))
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
		IndexOptIn    *bool           `json:"indexOptIn"`
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
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	cfg, current, verified, page := h.statusConfig(ctx, projectID)
	if req.Shown != nil {
		cfg.Shown = req.Shown
	}
	if req.ShowNetwork != nil {
		cfg.ShowNetwork = *req.ShowNetwork
	}
	if req.ShowPoweredBy != nil {
		cfg.ShowPoweredBy = *req.ShowPoweredBy
	}
	// "List in search engines" (plan part 4): stored with the config blob; it
	// is one half of a CLAIMED page's index qualification, the other being the
	// DNS TXT proof the worker verifies.
	if req.IndexOptIn != nil {
		cfg.IndexOptIn = *req.IndexOptIn
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
		verified = h.verifyStatusDomain(ctx, projectID, domain)
	}
	_, _, _, page = h.statusConfig(ctx, projectID)
	writeAPIJSON(w, http.StatusOK, h.statusPageResponse(ctx, tenantID, projectID, cfg, domain, verified, page))
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
	// "List in search engines" (plan part 4): one half of the index
	// qualification for a claimed page, the TXT verification is the other.
	IndexOptIn bool `json:"indexOptIn"`
}

// statusPageRow carries the page's own columns beyond the config blob: the
// part-2 state the owner API reports and the public door renders from. The
// zero value means "no page row" (prj-N pages have none yet).
type statusPageRow struct {
	ID                int64
	RootTargetID      *int64
	IsHostPage        bool
	RemovedAt         *time.Time
	IndexedAt         *time.Time
	HostVerifiedAt    *time.Time
	LastSeenAt        *time.Time
	VerificationToken *string
	RemovalToken      *string
	Slug              string
}

// poweredBy answers whether the credit line is published. On the cloud it is
// always yes, whatever is stored or submitted, so a hand-written API call
// cannot buy what no plan sells.
func (h *writeAPI) poweredBy(cfg statusPageConfig) bool {
	return !h.selfHosted || cfg.ShowPoweredBy
}

// statusConfig loads the saved settings for ONE project, defaulting a page
// that has never been configured to "publish everything" — the page exists to
// be public. The caller resolves the project first (currentProjectID for a
// session, the page's own row for the public door). The domain, its proof and
// the part-2 page columns come from the columns, never the config blob.
func (h *writeAPI) statusConfig(ctx context.Context, projectID int64) (statusPageConfig, string, bool, statusPageRow) {
	cfg := statusPageConfig{ShowNetwork: true, ShowPoweredBy: true, Shown: map[string]bool{}}
	var domain, title, slug *string
	var verifiedAt *time.Time
	var raw []byte
	var page statusPageRow
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT p.id, s.slug, s.title, s.domain, s.domain_verified_at, s.config,
		        s.id, s.root_target_id, s.is_host_page, s.removed_at, s.indexed_at,
		        s.host_verified_at, s.last_seen_at, s.verification_token, s.removal_token
		   FROM project p LEFT JOIN status_page s ON s.project_id = p.id
		  WHERE p.id = $1`, projectID).Scan(
		&projectID, &slug, &title, &domain, &verifiedAt, &raw,
		&page.ID, &page.RootTargetID, &page.IsHostPage, &page.RemovedAt, &page.IndexedAt,
		&page.HostVerifiedAt, &page.LastSeenAt, &page.VerificationToken, &page.RemovalToken)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	// The stored slug is the address already handed out: it wins over the body
	// and the id-shaped default ("prj-N", which publicStatus also accepts).
	cfg.Slug = "prj-" + strconv.FormatInt(projectID, 10)
	if slug != nil && *slug != "" {
		cfg.Slug = *slug
		page.Slug = *slug
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
	return cfg, stored, verifiedAt != nil, page
}

// statusPageResponse is the one shape both /v1/status-page handlers answer
// with: the stored decisions plus the measured components and network.
func (h *writeAPI) statusPageResponse(ctx context.Context, tenantID, projectID int64, cfg statusPageConfig, domain string, verified bool, page statusPageRow) map[string]any {
	resp := map[string]any{
		"slug":           cfg.Slug,
		"title":          cfg.Title,
		"domain":         domain,
		"domainVerified": verified,
		"components":     h.statusComponents(ctx, tenantID, projectID, cfg, false, page),
		"network":        h.statusNetwork(ctx, tenantID, projectID),
		"showNetwork":    cfg.ShowNetwork,
		"showPoweredBy":  h.poweredBy(cfg),
		// The index door's owner-facing facts (plan part 4): the switch, the
		// DNS proof of control, the tokens, and the live page's address (the
		// zero-monitors note links it).
		"indexOptIn":  cfg.IndexOptIn,
		"hostPage":    page.IsHostPage,
		"rootPageUrl": "/status/" + cfg.Slug,
	}
	if page.HostVerifiedAt != nil {
		resp["hostVerifiedAt"] = page.HostVerifiedAt.UTC().Format(time.RFC3339)
	}
	// The verification token is issued on read while it can still be used:
	// generated once, stored, returned every time until the TXT record lands
	// and the worker stamps host_verified_at (then the token is cleared and
	// this door stops offering one).
	if page.HostVerifiedAt == nil {
		if page.VerificationToken != nil {
			resp["verificationToken"] = *page.VerificationToken
		} else if page.ID != 0 {
			token := randomHex()
			if _, err := h.pool.Raw().Exec(ctx,
				`UPDATE status_page SET verification_token = $2 WHERE id = $1 AND verification_token IS NULL`,
				page.ID, token); err == nil {
				resp["verificationToken"] = token
			}
		}
	}
	// The removal token is only ECHOED here: the door that issues it belongs
	// to the page itself (Group 3's surface), never to the owner's settings.
	if page.RemovalToken != nil {
		resp["removalToken"] = *page.RemovalToken
	}
	return resp
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
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	_, domain, _, _ := h.statusConfig(ctx, projectID)
	if domain == "" {
		writeAPIErr(w, http.StatusBadRequest, "no_domain")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"domain":   domain,
		"verified": h.verifyStatusDomain(ctx, projectID, domain),
	})
}

// verifyStatusDomain resolves the stored domain and our own host and stamps
// domain_verified_at when the two answers intersect. LookupHost, deliberately
// not LookupCNAME: several DNS providers flatten the CNAME away and would
// fail this check while serving the traffic correctly.
func (h *writeAPI) verifyStatusDomain(ctx context.Context, projectID int64, domain string) bool {
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
		  WHERE project_id = $2 AND domain = $3`,
		verified, projectID, domain)
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
// bars. `publicOnly` drops the owner's unpublished ones. Since migration 009
// the checks table is keyed by target: the project's monitors are resolved to
// their targets ONCE and the rows are read by target_id. A host page PREPENDS
// its root target as the first component (named by the host), unless one of
// the project's monitors already subscribes to it - no double draw (plan
// part 2). Uptime and bucket math EXCLUDE unmeasured rows (the shared
// pgstore.MeasurableSQL predicate); such rows draw as nodata bars and never
// count against uptime.
func (h *writeAPI) statusComponents(ctx context.Context, tenantID, projectID int64, cfg statusPageConfig, publicOnly bool, page statusPageRow) []map[string]any {
	type compRow struct {
		id          int64 // monitor id; 0 for the root pseudo component
		key         string
		name        string
		intervalSec int32
		targetID    int64
	}
	var mons []compRow
	rows, err := h.pool.Raw().Query(ctx,
		`SELECT id, public_id, name, interval_sec, target_id FROM monitor WHERE project_id = $1 ORDER BY id`, projectID)
	if err == nil {
		for rows.Next() {
			var m compRow
			var pub [16]byte
			if rows.Scan(&m.id, &pub, &m.name, &m.intervalSec, &m.targetID) == nil {
				m.key = fmt.Sprintf("%x", pub[:])
				mons = append(mons, m)
			}
		}
		rows.Close()
	}
	// The host page's root component: first, named by the host, rendered
	// whether or not the project still holds a monitor on it (decision 11 -
	// the owner may delete their check; the page keeps measuring).
	if page.IsHostPage && page.RootTargetID != nil {
		rootID := *page.RootTargetID
		subscribed := false
		for _, m := range mons {
			if m.targetID == rootID {
				subscribed = true
				break
			}
		}
		if !subscribed {
			host := projectDomainOf(ctx, h.pool, projectID)
			name := host
			if name == "" {
				name = cfg.Title
			}
			mons = append([]compRow{{
				key:      fmt.Sprintf("root-%d", rootID),
				name:     name,
				targetID: rootID,
				// The host-page ladder's cadence (300/900/3600); the exact
				// value is corrected from the newest row below.
				intervalSec: 300,
			}}, mons...)
		}
	}
	if len(mons) == 0 {
		return []map[string]any{}
	}

	now := time.Now().UTC()

	// The first check held per target: it decides which rung of the ladder the
	// strip is on. Bounded by the retention window, which is all the table has.
	oldest := map[int64]time.Time{}
	// The cadence the target is actually checked at: the root component has no
	// monitor row of its own to carry an interval, so it reads its newest row.
	cadence := map[int64]int32{}
	targets := make([]int64, 0, len(mons))
	for _, m := range mons {
		targets = append(targets, m.targetID)
	}
	if h.pgs != nil {
		chRows, cerr := h.pgs.Raw().Query(ctx, `
			SELECT target_id, min(ts) FROM checks
			 WHERE target_id = ANY($1) AND ts >= now() - INTERVAL '7 days'
			 GROUP BY target_id`, targets)
		if cerr == nil {
			for chRows.Next() {
				var tid int64
				var first time.Time
				if chRows.Scan(&tid, &first) == nil {
					oldest[tid] = first.UTC()
				}
			}
			chRows.Close()
		}
		cRows, cerr := h.pgs.Raw().Query(ctx, `
			SELECT DISTINCT ON (target_id) target_id, interval_sec FROM checks
			 WHERE target_id = ANY($1) ORDER BY target_id, ts DESC`, targets)
		if cerr == nil {
			for cRows.Next() {
				var tid int64
				var interval int32
				if cRows.Scan(&tid, &interval) == nil && interval > 0 {
					cadence[tid] = interval
				}
			}
			cRows.Close()
		}
	}

	// Every strip's plan, and how far back the widest of them reaches - that
	// bounds the one raw query below.
	type barPlan struct {
		bucket time.Duration
		count  int
	}
	plans := map[int64]barPlan{}
	var rawFrom time.Time
	for _, m := range mons {
		interval := m.intervalSec
		if m.id == 0 && cadence[m.targetID] > 0 {
			interval = cadence[m.targetID]
		}
		window, bucket, count := barPlanFor(oldest[m.targetID], interval, now)
		plans[m.targetID] = barPlan{bucket: bucket, count: count}
		if from := now.Add(-window); rawFrom.IsZero() || from.Before(rawFrom) {
			rawFrom = from
		}
	}

	// Buckets counted BACKWARDS from now: a probe is not clock-aligned, and
	// wall-clock buckets would draw false gaps. Unmeasured rows ride along
	// flagged: they make a bucket nodata when nothing measured did, and never
	// enter the ok/total math.
	type day struct {
		ok, total  uint64
		unmeasured uint64
	}
	recent := map[int64]map[int]day{}
	if h.pgs != nil && !rawFrom.IsZero() {
		chRows, cerr := h.pgs.Raw().Query(ctx, fmt.Sprintf(`
			SELECT target_id, ts, ok, (%s) FROM checks
			 WHERE target_id = ANY($1) AND ts >= $2`, pgstore.MeasurableSQL), targets, rawFrom)
		if cerr == nil {
			for chRows.Next() {
				var tid int64
				var ts time.Time
				var okFlag, measurable bool
				if chRows.Scan(&tid, &ts, &okFlag, &measurable) != nil {
					continue
				}
				p := plans[tid]
				if p.bucket <= 0 {
					continue
				}
				bucket := int(now.Sub(ts.UTC()) / p.bucket)
				if bucket < 0 || bucket >= p.count {
					continue
				}
				m := recent[tid]
				if m == nil {
					m = map[int]day{}
					recent[tid] = m
				}
				entry := m[bucket]
				if measurable {
					entry.total++
					if okFlag {
						entry.ok++
					}
				} else {
					entry.unmeasured++
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
		p := plans[m.targetID]
		bars := make([]string, p.count)
		var okTotal, total uint64
		for i := range p.count {
			d := recent[m.targetID][p.count-1-i] // bucket 0 is the newest, so it lands last
			switch {
			case d.total == 0:
				// Nothing measured here: unmeasured-only and empty buckets
				// alike draw nodata - an unreadable host is not a down one.
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
func (h *writeAPI) statusNetwork(ctx context.Context, tenantID, projectID int64) []map[string]any {
	if h.pgs == nil {
		return []map[string]any{}
	}
	var dns, connect, total float64
	var samples uint64
	// Since migration 009 the checks table is keyed by target: the project's
	// monitors resolve to their targets in the subquery and the rows are read
	// by target_id.
	err := h.pgs.Raw().QueryRow(ctx, `
		SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY dns_ms),
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY connect_ms),
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY total_ms),
		       count(*)
		  FROM checks
		 WHERE ts >= now() - INTERVAL '24 hours' AND ok
		   AND region <> 'heartbeat'
		   AND target_id IN (SELECT target_id FROM monitor WHERE project_id = $1)`,
		projectID).Scan(&dns, &connect, &total, &samples)
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

// logQueryBuilder scopes every log read to the session's current project.
func (h *writeAPI) logQueryBuilder(ctx context.Context, r *http.Request, tenantID int64) *query.QueryBuilder {
	s, _ := h.sess.FromRequest(ctx, r)
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	return query.New(tenantID, projectID)
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
	// The current project's incident, not any incident of the workspace: an id
	// from a sibling project reads as not found here.
	vs, _ := h.sess.FromRequest(r.Context(), r)
	projectID := currentProjectID(r.Context(), h.pool, vs, tenantID)
	if err := h.pool.Raw().QueryRow(r.Context(),
		`SELECT title, status, affected_count FROM incident
		  WHERE public_id = $1 AND tenant_id = $2 AND project_id = $3`,
		parseUUID(idStr), tenantID, projectID).Scan(&title, &status, &affected); err != nil {
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

// watchRequest is the anonymous watch body: a host, an optional address, and
// the landing's ticked rows.
type watchRequest struct {
	Host    string   `json:"host"`
	Email   string   `json:"email"`
	Targets []string `json:"targets"`
}

func (h *writeAPI) publicWatch(w http.ResponseWriter, r *http.Request) {
	// A host typed on the landing becomes a real account: with an address the
	// door provisions exactly what the magic link would, so the two doors
	// agree; without one it mints an unclaimed tenant the visitor can claim
	// later, because the result comes first and the account second.
	ctx := analytics.WithScope(r.Context(), analytics.ScopeFromRequest(r))
	var req watchRequest
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

	// Host canonicalization before any lookup or mint (plan part 2): the
	// project's domain, the slug and the root target all key on the result,
	// so www.example.com and example.com are one host, one probe, one page.
	host, cerr := canonicalHost(req.Host)
	if cerr != nil {
		writeAPIErr(w, http.StatusBadRequest, "missing_host_or_email")
		return
	}
	target := "https://" + host

	// The mint door is the only door blocked_host guards: a host that asked
	// for removal is never minted again, and an unregistrable host (an IP
	// literal, a bare public suffix) has no page to hand out. The check door
	// and a signed-in owner's /v1/monitors are never gated here.
	if refused, code := blockedHostRefused(ctx, h.pool, host); refused {
		if code == "blocked_host" {
			writeAPIErr(w, http.StatusForbidden, code)
		} else {
			writeAPIErr(w, http.StatusBadRequest, code)
		}
		return
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
	// is simply no session - the door is public, a missing cookie must never
	// fail it.
	s, serr := h.sess.FromRequest(ctx, r)

	// Mint audit and ceilings (plan part 2): counted from status_page rows
	// before anything is created - a refusal creates nothing - and counted
	// again inside the mint transaction where the count is authoritative.
	audit := mintAuditFromRequest(r)
	if code := h.mintCeilingRefused(ctx, h.pool.Raw(), audit); code != "" {
		writeAPIErr(w, http.StatusTooManyRequests, code)
		return
	}

	// The client's target list is not trusted: each must belong to the asked
	// host, or this becomes a probe-enrolment service for strangers. The cap
	// is the Free plan's watch limit, the same number the check answer sends.
	wanted := h.wantedTargets(target, req.Targets)

	// The project is named after the site asked about; an existing account
	// keeps the name it has.
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
		// The e-mail arm's page follows the first-page rule like every mint:
		// a host that already has a live page gets a suffixed page on the same
		// root target, a fresh host gets the host page.
		watching, slug, err := h.watchMintPage(watchMint{
			host: host, wanted: wanted, audit: audit,
			tenantID: tenantID, projectID: projectID, existing: hostLivePage(ctx, h.pool, host),
		})
		if err != nil {
			watchRefuse(w, err)
			return
		}
		h.watchTail(ctx, w, r, req, tenantID, projectID, watching, slug)
		return
	}

	// One page per host at this anonymous door. The claimed page wins when
	// both kinds exist (hostLivePage's ORDER BY): a claimed host is never
	// re-minted or added to - reusing it would let anyone who types the
	// domain add monitors to somebody's account - so the visitor gets their
	// OWN suffixed page on the same root target (decision 9, never a second
	// probe). An unclaimed page is reused exactly as before.
	if existing := hostLivePage(ctx, h.pool, host); existing != nil {
		if existing.Claimed {
			t, p, watching, slug, err := h.mintOwnPage(ctx, host, existing, wanted, audit)
			if err != nil {
				watchRefuse(w, err)
				return
			}
			tenantID, projectID = t, p
			h.rec.ServerEvent(ctx, "watch_signup", 0, 0, map[string]string{"host": host})
			h.rec.ServerEvent(ctx, "page_minted", 0, 0, map[string]string{"source": "watch"})
			h.watchTail(ctx, w, r, req, tenantID, projectID, watching, slug)
			return
		}
		// Unclaimed: answer its slug, create no new page, but DO subscribe the
		// ticked rows - a watch that asks to watch more than the page holds is
		// additive, as it always was. Nothing minted: no event, no audit row.
		tenantID, projectID = existing.TenantID, existing.ProjectID
		watching := h.subscribeWanted(ctx, tenantID, projectID, host, wanted)
		h.rec.ServerEvent(ctx, "watch_signup", 0, 0, map[string]string{"host": host})
		h.watchTail(ctx, w, r, req, tenantID, projectID, watching, existing.Slug)
		return
	}

	// No live page for the host: the mint arm is where the session matters
	// (Decision 7) - a host nobody holds, typed by a signed-in visitor with
	// room for one more project, becomes a project in their OWN tenant - no
	// demo page to claim later, and the monitors and status page below land
	// on the caller's account from the first check. Everything else keeps
	// the demo mint exactly as today: signed out, no room (the wall is on
	// the claim button, never on the check).
	own := false
	if serr == nil && s.TenantID != 0 {
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
		t, p, watching, slug, err := h.mintOwnPage(ctx, host, nil, wanted, audit)
		if err != nil {
			watchRefuse(w, err)
			return
		}
		tenantID, projectID = t, p
		h.rec.ServerEvent(ctx, "watch_signup", 0, 0, map[string]string{"host": host})
		h.rec.ServerEvent(ctx, "page_minted", 0, 0, map[string]string{"source": "watch"})
		h.watchTail(ctx, w, r, req, tenantID, projectID, watching, slug)
		return
	}
	// The signed-in arm: same page rules as every mint (no live page exists
	// for this host - that is how this arm was reached), on the caller's own
	// project. page_minted fires with the watch source scope fields.
	watching, slug, err := h.watchMintPage(watchMint{
		host: host, wanted: wanted, audit: audit,
		tenantID: tenantID, projectID: projectID, existing: nil,
	})
	if err != nil {
		watchRefuse(w, err)
		return
	}
	h.rec.ServerEvent(ctx, "watch_signup", 0, 0, map[string]string{"host": host})
	h.rec.ServerEvent(ctx, "page_minted", 0, 0, map[string]string{"source": "watch"})
	h.watchTail(ctx, w, r, req, tenantID, projectID, watching, slug)
}

// canonicalHost reduces what the visitor typed to the host every page, slug
// and target keys on (plan part 2): lowercase, one leading www. stripped,
// IDN to punycode. It is the WATCH door's normalization only - /v1/monitors
// normalizes the URL through targetkey, a different, finer one. Weird input
// never errors here beyond an empty host: the lowercase spelling stands.
func canonicalHost(raw string) (string, error) {
	host := strings.ToLower(bareHost(raw))
	if host == "" {
		return "", errNoHost
	}
	host = strings.TrimPrefix(host, "www.")
	if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		host = strings.ToLower(ascii)
	}
	return host, nil
}

var errNoHost = errors.New("no host in input")

// blockedHostRefused is the anonymous mint door's host gate: the eTLD+1 is
// looked up in blocked_host (self-serve removals land there), and a host
// publicsuffix cannot reduce - an IP literal, a bare public suffix - is
// unmintable: there is no registrable domain to name a page after. Only
// this door consults the table: publicCheck never does, and a signed-in
// owner creating a check via /v1/monitors never passes through here.
func blockedHostRefused(ctx context.Context, pool *pg.Pool, host string) (refused bool, code string) {
	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return true, "unmintable_host"
	}
	var blocked bool
	_ = pool.Raw().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM blocked_host WHERE domain = $1)`, domain).Scan(&blocked)
	if blocked {
		return true, "blocked_host"
	}
	return false, ""
}

// mintAudit is what every watch-minted page records (plan part 2): who
// asked, with what agent. Nothing here identifies on its own; the hashes
// exist so the ceilings can count mints per source without storing either.
type mintAudit struct {
	ipHash      string
	visitorHash *string
	ua          string
}

// mintAuditFromRequest hashes the request's scope: the client IP always, the
// analytics visitor cookie when one exists, the User-Agent truncated.
func mintAuditFromRequest(r *http.Request) mintAudit {
	ip := sha256.Sum256([]byte(analytics.ClientIP(r)))
	audit := mintAudit{ipHash: hex.EncodeToString(ip[:]), ua: r.UserAgent()}
	if len(audit.ua) > 256 {
		audit.ua = audit.ua[:256]
	}
	if token, ok := analytics.VisitorToken(r); ok {
		v := sha256.Sum256([]byte(token))
		s := hex.EncodeToString(v[:])
		audit.visitorHash = &s
	}
	return audit
}

// knobsOrDefaults guards the ceilings against a zero-valued knob struct (a
// handler built by hand in a test): the defaults are the config package's.
func (h *writeAPI) knobsOrDefaults() (perIP, perDay, hostMax int) {
	k := h.statusKnobs.WithDefaults()
	return k.MintPerIPPerDay, k.MintPerDay, k.HostPagesMax
}

// rowQuerier is the one capability the ceiling counts and slug claiming
// need; both the pool and an open transaction provide it, so the same code
// runs outside the mint (early refusal) and inside it (authoritative).
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// mintCeilingRefused counts the day's mints from the audit columns: per IP
// and per instance (the instance count covers every page, whatever minted
// it - the seed door counts against this one too). Empty answer = allowed.
func (h *writeAPI) mintCeilingRefused(ctx context.Context, db rowQuerier, audit mintAudit) string {
	perIP, perDay, _ := h.knobsOrDefaults()
	var n int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM status_page
		  WHERE minted_ip_hash = $1 AND created_at > now() - interval '1 day'`,
		audit.ipHash).Scan(&n); err == nil && n >= perIP {
		return "mint_ip_ceiling"
	}
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM status_page
		  WHERE created_at > now() - interval '1 day'`).Scan(&n); err == nil && n >= perDay {
		return "mint_ceiling"
	}
	return ""
}

// hostPagesCeilingRefused caps the eternal population (review decision 15):
// live host pages held by unclaimed tenants are forever, so their number has
// a hard instance limit past which the mint answers 429.
func (h *writeAPI) hostPagesCeilingRefused(ctx context.Context, db rowQuerier) string {
	_, _, hostMax := h.knobsOrDefaults()
	var n int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM status_page sp JOIN tenant t ON t.id = sp.tenant_id
		  WHERE sp.is_host_page AND sp.removed_at IS NULL AND t.claim_token_hash IS NOT NULL`).Scan(&n); err == nil && n >= hostMax {
		return "host_pages_ceiling"
	}
	return ""
}

// wantedTargets is the landing's pick list: the root URL first, then the
// ticked rows that belong to the asked host, capped at the Free watch limit
// (the same number the check answer sends as watchLimit).
func (h *writeAPI) wantedTargets(target string, ticked []string) []string {
	wanted := append([]string{target}, sameHostTargets(target, ticked)...)
	limit := int(h.freeWatchLimit(context.Background()))
	if len(wanted) > limit {
		wanted = wanted[:limit]
	}
	return wanted
}

// hostPageRow is what the watch door needs to know about a host's live page:
// who holds it, whether it is claimed, and which target is its root.
type hostPageRow struct {
	TenantID     int64
	ProjectID    int64
	Slug         string
	RootTargetID *int64
	IsHostPage   bool
	Claimed      bool
}

// hostLivePage finds the host's LIVE page (a removed page does not exist for
// this door): the claimed one wins when both kinds exist, the host page wins
// within a holder. No row means the host is fresh for minting.
func hostLivePage(ctx context.Context, pool *pg.Pool, host string) *hostPageRow {
	var row hostPageRow
	err := pool.Raw().QueryRow(ctx,
		`SELECT sp.tenant_id, sp.project_id, sp.slug, sp.root_target_id, sp.is_host_page,
		       (t.claim_token_hash IS NULL) AS claimed
		  FROM status_page sp
		  JOIN project p ON p.id = sp.project_id
		  JOIN tenant t ON t.id = sp.tenant_id
		 WHERE p.domain = $1 AND sp.removed_at IS NULL
		 ORDER BY (t.claim_token_hash IS NULL) DESC, sp.is_host_page DESC, sp.id
		 LIMIT 1`, host).Scan(
		&row.TenantID, &row.ProjectID, &row.Slug, &row.RootTargetID, &row.IsHostPage, &row.Claimed)
	if err != nil {
		return nil
	}
	return &row
}

// subscribeWanted turns the landing's ticked rows into subscriptions on the
// SHARED targets (plan part 1: one URL, one probe - a reused page's project
// subscribes to the same target a fresh mint would). Idempotent per
// (project, target); returns how many rows the project now watches.
func (h *writeAPI) subscribeWanted(ctx context.Context, tenantID, projectID int64, host string, wanted []string) int {
	watching := 0
	_ = h.watchTx(ctx, func(q *sqlc.Queries, tx pgx.Tx) error {
		var err error
		watching, err = subscribeWantedTx(ctx, q, tx, tenantID, projectID, host, wanted)
		return err
	})
	return watching
}

// subscribeWantedTx is subscribeWanted inside a caller's transaction: every
// row resolves through GetOrCreateProbeTarget and pulls the target due, so a
// subscription's first check runs at the next lease.
func subscribeWantedTx(ctx context.Context, q *sqlc.Queries, tx pgx.Tx, tenantID, projectID int64, host string, wanted []string) (int, error) {
	watching := 0
	for _, t := range wanted {
		targetID, terr := q.GetOrCreateProbeTarget(ctx, sqlc.GetOrCreateProbeTargetParams{
			Key: targetkey.Website(t, ""), Kind: "website", Url: t,
		})
		if terr != nil {
			return watching, terr
		}
		if terr := q.EnsureTargetSchedule(ctx, sqlc.EnsureTargetScheduleParams{
			TargetID: targetID, Region: scheduleRegion(),
		}); terr != nil {
			return watching, terr
		}
		row, created, ierr := insertMonitorOnTarget(ctx, tx, sqlc.CreateMonitorParams{
			TenantID: tenantID, ProjectID: projectID, Kind: "website",
			Name: monitorName(host, t), Target: t, IntervalSec: 300,
		}, targetID)
		if ierr != nil {
			return watching, ierr
		}
		if created {
			if terr := q.PullTargetDue(ctx, targetID); terr != nil {
				return watching, terr
			}
		}
		_ = row
		watching++
	}
	return watching, nil
}

// watchMint is one page-minting run on an EXISTING tenant and project (the
// e-mail arm and the signed-in arm): subscribe the wanted rows, upsert the
// page row with its audit columns and the first-page rule from `existing`
// (nil = no live page for the host, so this mint IS the host page).
type watchMint struct {
	host      string
	wanted    []string
	audit     mintAudit
	tenantID  int64
	projectID int64
	existing  *hostPageRow
}

// watchMintPage runs a watchMint in one transaction. A refusal code
// travels as refusalError; any other error is a real failure. On either the
// transaction rolled back - nothing was created, which is the whole point
// of counting inside it.
func (h *writeAPI) watchMintPage(m watchMint) (watching int, slug string, err error) {
	err = h.watchTx(context.Background(), func(q *sqlc.Queries, tx pgx.Tx) error {
		if code := h.mintCeilingRefused(context.Background(), tx, m.audit); code != "" {
			return refusalError(code)
		}
		var serr error
		watching, serr = subscribeWantedTx(context.Background(), q, tx, m.tenantID, m.projectID, m.host, m.wanted)
		if serr != nil {
			return serr
		}
		slug, serr = upsertWatchPage(context.Background(), tx, m)
		return serr
	})
	return watching, slug, err
}

// mintUnclaimedTriple is the unclaimed tenant + project + project_seq the
// anonymous and seed mint doors share: newUnclaimedTenant's triple minus the
// api key, on the caller's open transaction so a refusal leaves nothing
// behind. The doors that hand out a key mint it themselves, tx-bound.
func mintUnclaimedTriple(ctx context.Context, tx pgx.Tx, host string) (tenantID, projectID int64, err error) {
	claimHash := sha256.Sum256([]byte(randomHex()))
	if err := tx.QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, claim_token_hash)
		 VALUES (gen_random_uuid(), $1, $2) RETURNING id`, host, claimHash[:]).Scan(&tenantID); err != nil {
		return 0, 0, err
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES ($1, $2, $3) RETURNING id`,
		newUUID(), tenantID, host).Scan(&projectID); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO project_seq (project_id, next) VALUES ($1, 1) ON CONFLICT DO NOTHING`, projectID); err != nil {
		return 0, 0, err
	}
	return tenantID, projectID, nil
}

// mintOwnPage is the anonymous door's own-project mint: a fresh unclaimed
// tenant and project (the demo triple), the wanted subscriptions, and a page
// that is either the host's first (existing == nil) or a SUFFIXED second
// page sharing the claimed one's root target (decision 9). One transaction;
// on refusal nothing exists afterwards.
func (h *writeAPI) mintOwnPage(ctx context.Context, host string, existing *hostPageRow, wanted []string, audit mintAudit) (tenantID, projectID int64, watching int, slug string, err error) {
	err = h.watchTx(ctx, func(q *sqlc.Queries, tx pgx.Tx) error {
		if code := h.mintCeilingRefused(ctx, tx, audit); code != "" {
			return refusalError(code)
		}
		// The host-pages cap guards only ETERNAL pages: a second (suffixed)
		// page is not one - the reaper collects it under the old rule.
		if existing == nil {
			if code := h.hostPagesCeilingRefused(ctx, tx); code != "" {
				return refusalError(code)
			}
		}
		var terr error
		tenantID, projectID, terr = mintUnclaimedTriple(ctx, tx, host)
		if terr != nil {
			return terr
		}
		// The api key, tx-bound like createTenantProject's: a pool-bound mint
		// would autocommit outside this transaction.
		secret := randomHex()
		keyHash := sha256.Sum256([]byte(keyScheme(keyKindSecret) + secret))
		if _, err := q.CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
			TenantID: tenantID, ProjectID: projectID, Prefix: secret[:12],
			SecretHash: keyHash[:], Kind: keyKindSecret, Origins: []string{},
		}); err != nil {
			return err
		}
		var serr error
		watching, serr = subscribeWantedTx(ctx, q, tx, tenantID, projectID, host, wanted)
		if serr != nil {
			return serr
		}
		slug, serr = upsertWatchPage(ctx, tx, watchMint{
			host: host, audit: audit, tenantID: tenantID, projectID: projectID,
			existing: existing,
		})
		return serr
	})
	if err != nil {
		return 0, 0, 0, "", err
	}
	return tenantID, projectID, watching, slug, nil
}

// upsertWatchPage writes the page row a watch mint leaves behind: the audit
// columns, minted_source='watch', and the first-page rule - the mint that
// finds no live page for the host IS the host page (is_host_page, bare
// slug); a second page shares the root target and takes a suffixed slug. ON
// CONFLICT DO NOTHING: a handed-out slug must not change under a returning
// visitor, and a project that already had a page keeps ITS slug (re-read).
func upsertWatchPage(ctx context.Context, tx pgx.Tx, m watchMint) (string, error) {
	rootURL := "https://" + m.host
	isHostPage := m.existing == nil
	rootID := int64(0)
	if m.existing != nil {
		if m.existing.RootTargetID != nil {
			rootID = *m.existing.RootTargetID
		}
	}
	if rootID == 0 {
		// The root target of https://{host}: shared like every other, so the
		// page's reference keeps it measured even with no subscriber left.
		var err error
		rootID, err = h0GetOrCreateProbeTarget(ctx, tx, rootURL)
		if err != nil {
			return "", err
		}
	}
	slug := claimSlugOn(ctx, tx, m.host, m.projectID)
	if slug == "" {
		slug = "prj-" + strconv.FormatInt(m.projectID, 10)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title, root_target_id, is_host_page,
		                         minted_source, minted_ip_hash, minted_visitor_hash, minted_ua)
		 VALUES ($1, $2, $3, $4, $5, $6, 'watch', $7, $8, $9) ON CONFLICT (slug) DO NOTHING`,
		m.tenantID, m.projectID, slug, m.host, rootID, isHostPage,
		m.audit.ipHash, m.audit.visitorHash, m.audit.ua); err != nil {
		return "", err
	}
	_ = tx.QueryRow(ctx,
		`SELECT slug FROM status_page WHERE project_id = $1 ORDER BY id LIMIT 1`, m.projectID).Scan(&slug)
	return slug, nil
}

// h0GetOrCreateProbeTarget resolves (or mints) the target of a raw URL on a
// transaction the sqlc Queries object does not wrap (upsertWatchPage works
// on the tx directly).
func h0GetOrCreateProbeTarget(ctx context.Context, tx pgx.Tx, rawURL string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO probe_target (key, kind, url) VALUES ($1, 'website', $2)
		 ON CONFLICT (key) DO UPDATE SET url = EXCLUDED.url RETURNING id`,
		targetkey.Website(rawURL, ""), rawURL).Scan(&id)
	return id, err
}

// refusalError marks a refusal code travelling out of a transaction closure;
// watchRefuse unpacks it (429 with the code) and treats anything else as a
// real failure on the internal path - never as a silent empty answer.
type refusalError string

func (e refusalError) Error() string { return string(e) }

func watchRefuse(w http.ResponseWriter, err error) {
	if code, ok := err.(refusalError); ok {
		writeAPIErr(w, http.StatusTooManyRequests, string(code))
		return
	}
	slog.Warn("watch: mint failed", "err", err)
	writeAPIErr(w, http.StatusInternalServerError, "internal")
}

// watchTx is the watch vertical's transaction runner: nil commits, anything
// else rolls back. A refusal code wrapped in refusalError travels as itself.
func (h *writeAPI) watchTx(ctx context.Context, fn func(*sqlc.Queries, pgx.Tx) error) error {
	tx, err := h.pool.Raw().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(h.pool.Queries().WithTx(tx), tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// watchTail is the e-mail arm's response side: the e-mail channel, the
// login code (dev echoes it), and the JSON answer.
func (h *writeAPI) watchTail(ctx context.Context, w http.ResponseWriter, r *http.Request, req watchRequest, tenantID, projectID int64, watching int, slug string) {
	// The e-mail channel is the point of leaving an address: without it the
	// account would watch the host and tell nobody.
	if req.Email != "" {
		_, _ = h.pool.Raw().Exec(ctx,
			`INSERT INTO alert_channel (public_id, tenant_id, project_id, kind, target)
			 SELECT gen_random_uuid(), $1, $2, 'email', $3
			  WHERE NOT EXISTS (SELECT 1 FROM alert_channel WHERE project_id = $2 AND kind = 'email' AND target = $3)`,
			tenantID, projectID, req.Email)
	}
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
	// issueKeyOfKind would autocommit outside the transaction. This is the THIRD
	// copy of that mint in this package — the comment on issueNamedKey claiming
	// "nothing else here mints" was wrong three ways, and every copy had to gain
	// Kind and Origins by hand when the columns arrived, because a missing
	// `text[] NOT NULL` field encodes as NULL and fails the insert.
	secret := randomHex()
	hash := sha256.Sum256([]byte(keyScheme(keyKindSecret) + secret))
	if _, err := pool.Queries().WithTx(tx).CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
		TenantID:   tenantID,
		ProjectID:  projectID,
		Prefix:     secret[:12],
		SecretHash: hash[:],
		Kind:       keyKindSecret,
		Origins:    []string{},
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
	// leaves an empty string; the caller falls back to the project id. A slug
	// past 40 chars is cut AND salted with a short hash of the host (plan part
	// 2): the same host always yields the same slug, where a plain cut could
	// collide two long hosts sharing a prefix.
	if len(slug) > 40 {
		sum := sha256.Sum256([]byte(bareHost(host)))
		slug = strings.Trim(slug[:40], "-") + "-" + hex.EncodeToString(sum[:])[:6]
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
	return claimSlugOn(ctx, pool.Raw(), host, projectID)
}

// claimSlugOn is claimSlugFor on whatever can run the lookup — the pool or
// the watch mint's open transaction.
func claimSlugOn(ctx context.Context, db rowQuerier, host string, projectID int64) string {
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
		err := db.QueryRow(ctx,
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

// canonicalAliasSlug folds a www-shaped slug to the host page's canonical
// slug (www.example.com and example.com are one host, one page): "" when
// the slug is no page's alias. Every slug-miss door runs it - the JSON
// door's fold and the HTML door's - so the two can never drift.
func (h *writeAPI) canonicalAliasSlug(ctx context.Context, slug string) string {
	var canon string
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT sp.slug
		   FROM status_page sp
		   JOIN project p ON p.id = sp.project_id
		  WHERE p.domain <> '' AND sp.is_host_page AND sp.removed_at IS NULL
		    AND trim(both '-' FROM regexp_replace(lower('www.' || p.domain), '[^a-z0-9]+', '-', 'g')) = $1
		  LIMIT 1`, slug).Scan(&canon); err != nil {
		return ""
	}
	return canon
}

func (h *writeAPI) publicStatus(w http.ResponseWriter, r *http.Request) {
	// The public page shows the same measured components as the config screen,
	// minus the ones the owner unpublished. Nothing here is typed in by hand.
	ctx := r.Context()
	var tenantID, projectID int64
	var claimed bool
	// A request without a slug arrives on somebody's custom domain: the Host
	// header (lowercased, port stripped) is the address, and only a domain we
	// have verified answers — an unverified row must render nothing.
	if r.URL.Path == "/public/status" {
		host := strings.ToLower(r.Host)
		if bare, _, err := net.SplitHostPort(host); err == nil {
			host = bare
		}
		var removedAt *time.Time
		if err := h.pool.Raw().QueryRow(ctx,
			`SELECT sp.tenant_id, sp.project_id, (t.claim_token_hash IS NULL), sp.removed_at
			   FROM status_page sp JOIN tenant t ON t.id = sp.tenant_id
			  WHERE sp.domain = $1 AND sp.domain_verified_at IS NOT NULL`, host).Scan(&tenantID, &projectID, &claimed, &removedAt); err != nil {
			writeAPIErr(w, http.StatusNotFound, "no_such_page")
			return
		}
		if removedAt != nil {
			writeAPIErr(w, http.StatusGone, "page_removed")
			return
		}
		h.renderPublicStatus(w, r, tenantID, projectID, claimed)
		return
	}
	slug := pathLast(r.URL.Path)
	var removedAt *time.Time
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT sp.tenant_id, sp.project_id, (t.claim_token_hash IS NULL), sp.removed_at
		   FROM status_page sp JOIN tenant t ON t.id = sp.tenant_id
		  WHERE sp.slug = $1`, slug).Scan(&tenantID, &projectID, &claimed, &removedAt); err == nil {
		// A removed page answers 410 on every door (plan part 2): the link a
		// visitor may still hold says "gone", not "never existed".
		if removedAt != nil {
			writeAPIErr(w, http.StatusGone, "page_removed")
			return
		}
		h.renderPublicStatus(w, r, tenantID, projectID, claimed)
		return
	}
	// Slug miss: the alias fold — a request for the www-shaped slug of a host
	// page redirects to the page's canonical slug (www.example.com and
	// example.com are one host, one page). One query, only on a miss.
	if canon := h.canonicalAliasSlug(ctx, slug); canon != "" && canon != slug {
		w.Header().Set("Location", "/status/"+canon)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}
	// A page not yet configured resolves by its project slug: the parsed
	// id already IS the project, so only its workspace is looked up.
	if _, perr := fmt.Sscanf(slug, "prj-%d", &projectID); perr != nil || projectID == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_page")
		return
	}
	if qerr := h.pool.Raw().QueryRow(ctx,
		`SELECT p.tenant_id, (t.claim_token_hash IS NULL), (SELECT sp.removed_at FROM status_page sp WHERE sp.project_id = p.id LIMIT 1)
		   FROM project p JOIN tenant t ON t.id = p.tenant_id
		  WHERE p.id = $1`, projectID).Scan(&tenantID, &claimed, &removedAt); qerr != nil || tenantID == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_page")
		return
	}
	if removedAt != nil {
		writeAPIErr(w, http.StatusGone, "page_removed")
		return
	}
	h.renderPublicStatus(w, r, tenantID, projectID, claimed)
}

// renderPublicStatus is the tail both doors share once the page's project is
// found: the shared assembly, then the one request-shaped fact no other
// surface can compute. A slug in the path and a Host header differ only in
// how the project is looked up.
func (h *writeAPI) renderPublicStatus(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, claimed bool) {
	ctx := r.Context()
	resp, meta := h.publicStatusData(ctx, tenantID, projectID, claimed)
	// The viewer's own page says so: mine is present and true only when a
	// session resolves AND that person reaches THIS project - a member of a
	// sibling project is a visitor here. Absent = not the viewer's page
	// (signed out, or somebody else's); errors are ignored - the door is
	// public and must answer the same either way. This block reads the
	// request's session, so it lives in the request-bearing door, never in
	// the shared assembly.
	if vs, verr := h.sess.FromRequest(ctx, r); verr == nil && h.reachesProject(ctx, vs.PersonID, projectID) {
		resp["mine"] = true
		// Same visibility rule, one purpose: the owner reading their own page on
		// our link is the one viewer the upgrade banner speaks to, and it must
		// not sell an address they already own. A visitor never learns this.
		if meta.storedDomain != "" {
			resp["hasCustomDomain"] = true
		}
	}
	writeAPIJSON(w, http.StatusOK, resp)
}

// statusPageMeta carries what a rendering surface needs beyond the JSON map
// (plan part 4): the slug (already the canonical one - an alias slug is
// folded to it with a 301 before any assembly runs, and the claim door's
// URL shape is /status/{slug}#claim, derivable from it), the host the page
// words its title and sentences from, the host-page marker that decides the
// robots meta and the canonical link, and the index gate's stamp.
type statusPageMeta struct {
	slug         string
	host         string
	isHostPage   bool
	storedDomain string
	rootTargetID int64 // 0 when the page carries no root reference
	removedAt    *time.Time
	indexedAt    *time.Time
}

// publicStatusData is the ONE assembly every public surface of a page
// renders from (the JSON door, the crawler's HTML door, the OG image):
// config, components, incidents, the state sentence, the index facts and
// the liveness stamp. Part 4 lifted it out of renderPublicStatus unchanged;
// the JSON door's bytes stay what they were.
func (h *writeAPI) publicStatusData(ctx context.Context, tenantID, projectID int64, claimed bool) (map[string]any, statusPageMeta) {
	// The page's OWN project decides what is rendered: a signed-in viewer's
	// session must not bend somebody else's page toward their current project.
	cfg, storedDomain, _, page := h.statusConfig(ctx, projectID)
	meta := statusPageMeta{
		slug:         cfg.Slug,
		host:         projectDomainOf(ctx, h.pool, projectID),
		isHostPage:   page.IsHostPage,
		storedDomain: storedDomain,
		removedAt:    page.RemovedAt,
		indexedAt:    page.IndexedAt,
	}
	if page.RootTargetID != nil {
		meta.rootTargetID = *page.RootTargetID
	}
	resp := map[string]any{
		"title":      cfg.Title,
		"components": h.statusComponents(ctx, tenantID, projectID, cfg, true, page),
		"incidents":  []map[string]any{},
		"network":    []map[string]any{},
		"updatedAt":  time.Now().UTC().Format(time.RFC3339),
		"claimed":    claimed,
		"poweredBy":  h.poweredBy(cfg),
		// The gate's public face (plan part 4): a page is indexable only with a
		// stamp AND the kill switch off. Suffixed and unhosted pages carry no
		// hostPage; the front words its banners from these three facts.
		"hostPage": page.IsHostPage,
	}
	if page.HostVerifiedAt == nil {
		resp["unverifiedClaim"] = claimed
	} else {
		resp["unverifiedClaim"] = false
	}
	indexable := page.IndexedAt != nil && !h.statusKnobs.IndexDisabled
	resp["indexable"] = indexable
	// The host page's measured state sentence (plan part 3): worded from the
	// probe's point of view, exactly the four forms the plan fixes.
	if page.IsHostPage && page.RootTargetID != nil {
		if state, ok := h.hostPageState(ctx, projectDomainOf(ctx, h.pool, projectID), *page.RootTargetID); ok {
			resp["state"] = state
		}
		// Best-effort liveness stamp, throttled to one an hour: it feeds the
		// index gate's origin rule and the unclaimed-page slowdown, and must
		// never delay or fail the response.
		go func(id int64) {
			bctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = h.pool.Raw().Exec(bctx,
				`UPDATE status_page SET last_seen_at = now()
			  WHERE id = $1 AND (last_seen_at IS NULL OR last_seen_at < now() - interval '1 hour')`, id)
		}(page.ID)
	}
	// The owner's switch decides whether the section is published at all; what it
	// then shows is measured, never sample data.
	if cfg.ShowNetwork {
		resp["network"] = h.statusNetwork(ctx, tenantID, projectID)
	}
	incidents := []map[string]any{}
	if rows, rerr := h.pool.Raw().Query(ctx,
		// monitor_id IS NOT NULL drops incidents of DELETED checks: the
		// component is gone from the page; the detector filter does the rest.
		`SELECT title, status, detected_at FROM incident
		  WHERE project_id = $1 AND detector = 'availability' AND monitor_id IS NOT NULL
		  ORDER BY detected_at DESC LIMIT 10`, projectID); rerr == nil {
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
	return resp, meta
}

// reachesProject answers whether this person owns the project's workspace or
// is an active member of it: the public page's `mine`.
func (h *writeAPI) reachesProject(ctx context.Context, personID, projectID int64) bool {
	if personID == 0 || projectID == 0 {
		return false
	}
	_, err := h.pool.Queries().ProjectScope(ctx, sqlc.ProjectScopeParams{
		PersonID: &personID, ProjectID: projectID,
	})
	return err == nil
}

// projectDomainOf reads the project's domain ("" when unset): the host page's
// component name and state sentence are worded from it.
func projectDomainOf(ctx context.Context, pool *pg.Pool, projectID int64) string {
	var domain string
	_ = pool.Raw().QueryRow(ctx, `SELECT domain FROM project WHERE id = $1`, projectID).Scan(&domain)
	return domain
}

// hostPageState builds the host page's `state` object (plan part 3): the
// measured verdict of the page's root target, worded from the probe's point
// of view. Kinds: ok | down | could_not_measure | nodata (the detector's
// "check" - something wrong, not yet an outage - maps to ok's shape with its
// own sentence through the same newest row). False when there is nothing to
// say (no facts row yet).
func (h *writeAPI) hostPageState(ctx context.Context, host string, targetID int64) (map[string]any, bool) {
	if host == "" {
		return nil, false
	}
	var status *string
	var newestOK *bool
	var newestCode *int
	var newestClass *string
	var newestMs *int
	var newestTS *time.Time
	var lastOK *time.Time
	// Scalar subqueries, one row always: a page minted a minute ago has no
	// facts row and no checks, and its honest state is nodata, not an error.
	err := h.pool.Raw().QueryRow(ctx,
		`SELECT (SELECT status FROM target_facts WHERE target_id = $1),
		        (SELECT ok FROM checks c WHERE c.target_id = $1 ORDER BY ts DESC LIMIT 1),
		        (SELECT status_code FROM checks c WHERE c.target_id = $1 ORDER BY ts DESC LIMIT 1),
		        (SELECT error_class FROM checks c WHERE c.target_id = $1 ORDER BY ts DESC LIMIT 1),
		        (SELECT total_ms FROM checks c WHERE c.target_id = $1 ORDER BY ts DESC LIMIT 1),
		        (SELECT ts FROM checks c WHERE c.target_id = $1 ORDER BY ts DESC LIMIT 1),
		        (SELECT max(ts) FROM checks c WHERE c.target_id = $1 AND ok)`, targetID).
		Scan(&status, &newestOK, &newestCode, &newestClass, &newestMs, &newestTS, &lastOK)
	if err != nil {
		return nil, false
	}
	kind := "nodata"
	if status != nil {
		switch *status {
		case availability.StatusOK, availability.StatusCheck:
			kind = "ok"
		case availability.StatusDown:
			kind = "down"
		case availability.StatusCouldNotMeasure:
			kind = "could_not_measure"
		case availability.StatusNoData, "":
			kind = "nodata"
		}
	}
	state := map[string]any{"kind": kind}
	clock := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format("15:04") + " UTC"
	}
	if newestTS != nil {
		state["asOf"] = clock(newestTS)
	}
	switch kind {
	case "ok":
		code, ms := 0, 0
		if newestCode != nil {
			code = *newestCode
		}
		if newestMs != nil {
			ms = *newestMs
		}
		state["sentence"] = fmt.Sprintf("As of %s, %s answered HTTP %d in %d ms from our check.",
			clock(newestTS), host, code, ms)
	case "down":
		// The outage began at the last answer we did get; without one, at the
		// newest attempt. The parenthetical names why, in the plan's words.
		since := newestTS
		if lastOK != nil {
			since = lastOK
		}
		state["sentence"] = fmt.Sprintf("Our check has had no answer from %s since %s (%s).",
			host, clock(since), errorPhrase(newestClass, newestCode))
	case "could_not_measure":
		state["sentence"] = fmt.Sprintf(
			"%s refuses automated checks from our location; we cannot measure it.", host)
	default:
		state["sentence"] = "No data yet: the first check runs at the next probe cycle, usually within a few minutes."
	}
	return state, true
}

// errorPhrase words the down sentence's parenthetical (plan part 3): the
// class in the reader's words, an unknown class falling back to the plain
// connection failure.
func errorPhrase(class *string, code *int) string {
	name := ""
	if class != nil {
		name = *class
	}
	switch name {
	case "dns":
		return "DNS lookup failed"
	case "connect":
		return "connection failed"
	case "timeout":
		return "connection timed out"
	case "tls":
		return "TLS error"
	case "keyword_missing":
		return "expected keyword missing"
	case "status":
		if code != nil {
			return fmt.Sprintf("HTTP %d", *code)
		}
		return "connection failed"
	default:
		return "connection failed"
	}
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
