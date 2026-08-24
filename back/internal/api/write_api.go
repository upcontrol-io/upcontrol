// Write API: channels, recipients, sources CRUD + public endpoints. All
// session-scoped (tenant_id from the cookie), all returning shapes that match
// mockData.ts so the front needs no component changes.

package api

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"

	"go.upcontrol.io/back/internal/account/auth"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/ai"
	"go.upcontrol.io/back/internal/analytics"
	notifysettings "go.upcontrol.io/back/internal/channel/notify"
	"go.upcontrol.io/back/internal/probe/discover"
	"go.upcontrol.io/back/internal/probe/executor"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// WriteAPI handles all the POST/PATCH/DELETE + public endpoints.
type WriteAPI struct {
	pool *pg.Pool
	ch   *ch.Conn
	sess *session.Manager
	exec *executor.Executor
	acct *ai.Accountant
	// selfHosted (UC_SELF_HOSTED=1): the anonymous watch door provisions
	// 'Self-hosted' tenants, same as the sign-in door (Decision 7).
	selfHosted bool
	// One mutex for the three in-memory per-replica maps below. Per-replica is
	// the honest MVP bound — two replicas give 2× each limit, documented here
	// once so it's not a surprise.
	//   - checkSeenAt: anonymous /public/check + watch cooldowns (plan §6.1:
	//     the public prober is unauthenticated and must be rate-limited per IP
	//     and per host), keyed "bucket:ip"
	//   - checkCache: those checks' answers, keyed by host
	//   - explainSeenAt: the authenticated explain throttle, keyed by tenant
	checkMu     sync.Mutex
	checkSeenAt map[string]time.Time
	// Answers already computed, keyed by host. A check costs up to
	// discover.MaxRequests outbound requests, so serving a fresh one twice is
	// spending somebody else's bandwidth to say the same thing.
	checkCache map[string]cachedCheck
	// Per-tenant throttle for POST /v1/logs/explain: the endpoint spends
	// provider money, so one tenant may not hammer it. Same per-replica
	// pattern as checkSeenAt above — two replicas admit 2× the limit.
	explainSeenAt map[int64][]time.Time
	// Dev relaxation, and the only one: the anonymous watch echoes the login
	// code it issued. Never true in prod — see publicWatch.
	devMode bool
	// Async analytics recorder. nil (tests, unwired deployments) is a no-op.
	rec *analytics.Recorder
	// The watch door hands a first-time visitor their login code. In prod it
	// goes by e-mail (the same mailer the sign-in door uses); nil means no
	// mailer configured, in which case prod stores the code unsent — the
	// visitor still owns the account and can sign in by requesting a fresh link.
	mailer auth.Mailer
}

// checkCacheTTL is short enough that a reader who just fixed their site sees the
// fix, long enough that a link doing the rounds does not re-probe per visitor.
const checkCacheTTL = 10 * time.Minute

// The explain throttle: a sliding one-minute window admitting at most six
// requests per tenant. A human reading logs clicks Explain a handful of times;
// a script does not. It is the BURST gate only — the monthly bound is
// plan_entitlement.ai_explains, finite on every plan since migration 020
// (5/50/200/400, the owner's decision; the row that names them is
// Pricing.tsx). It used to be the only cap on three of the four plans, which
// is what made 6/min worth this paragraph: 8 640 explains per tenant per day
// per replica, each up to the 32 KiB input cap, with an idempotency hash over
// the raw lines that a caller varying one byte per request misses every time.
// Per-replica and in-memory by design, no config (Decision 13).
const (
	explainBurst  = 6
	explainWindow = time.Minute
)

type cachedCheck struct {
	body map[string]any
	at   time.Time
}

func NewWriteAPI(p *pg.Pool, chConn *ch.Conn, sm *session.Manager, acct *ai.Accountant, devMode bool, mail auth.Mailer, rec *analytics.Recorder, selfHosted bool) *WriteAPI {
	return &WriteAPI{pool: p, ch: chConn, sess: sm, exec: executor.New(), acct: acct, checkSeenAt: map[string]time.Time{}, checkCache: map[string]cachedCheck{}, explainSeenAt: map[int64][]time.Time{}, devMode: devMode, mailer: mail, rec: rec, selfHosted: selfHosted}
}

func (h *WriteAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Public endpoints don't need a session.
	if strings.HasPrefix(r.URL.Path, "/public/") {
		h.public(w, r)
		return
	}

	// Everything else needs a session.
	s, err := h.sess.FromRequest(r.Context(), r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	// §7.4 / design D6: notify members read (deliveries, status-page, logs,
	// incidents, export are GETs below); every mutation is a settings act and
	// needs login. The Mini App and the web answer to the same gate.
	if r.Method != http.MethodGet && !roleAtLeastLogin(r.Context(), h.pool, s.PersonID, s.TenantID) {
		writeAPIErr(w, http.StatusForbidden, "notify_role")
		return
	}
	tenantID := s.TenantID

	switch {
	// Channels
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

	// Recipients
	case r.URL.Path == "/v1/recipients" && r.Method == http.MethodPost:
		h.createRecipient(w, r, tenantID)
	case strings.HasPrefix(r.URL.Path, "/v1/recipients/") && r.Method == http.MethodPatch:
		h.patchRecipient(w, r, tenantID)
	case strings.HasPrefix(r.URL.Path, "/v1/recipients/") && r.Method == http.MethodDelete:
		h.deleteRecipient(w, r, tenantID)

	// Sources
	case strings.Contains(r.URL.Path, "/connect") && r.Method == http.MethodPost:
		h.connectSource(w, r, tenantID)
	case strings.HasPrefix(r.URL.Path, "/v1/sources/") && r.Method == http.MethodDelete:
		h.deleteSource(w, r, tenantID)

	// Status page
	case r.URL.Path == "/v1/status-page":
		switch r.Method {
		case http.MethodGet:
			h.getStatusPage(w, r, tenantID)
		case http.MethodPut:
			h.putStatusPage(w, r, tenantID)
		default:
			writeAPIErr(w, http.StatusMethodNotAllowed, "method_not_allowed")
		}

	// Logs
	case r.URL.Path == "/v1/export" && r.Method == http.MethodGet:
		h.exportAll(w, r, tenantID)

	case r.URL.Path == "/v1/project" && r.Method == http.MethodDelete:
		h.deleteProject(w, r, tenantID)

	case strings.HasPrefix(r.URL.Path, "/v1/sources/") && r.Method == http.MethodPatch:
		h.patchSource(w, r, tenantID)

	case r.URL.Path == "/v1/logs" && r.Method == http.MethodGet:
		h.getLogs(w, r, tenantID)
	case r.URL.Path == "/v1/logs/explain" && r.Method == http.MethodPost:
		h.explainLogs(w, r, tenantID)
	case r.URL.Path == "/v1/logs/explain/preview" && r.Method == http.MethodPost:
		h.previewExplain(w, r, tenantID)

	// Incident explain: the server assembles the evidence, the caller sends
	// only the id. The suffix check keeps this arm disjoint from the GET
	// below (different method, and the GET's pathLast would read "explain").
	case strings.HasPrefix(r.URL.Path, "/v1/incidents/") && strings.HasSuffix(r.URL.Path, "/explain") && r.Method == http.MethodPost:
		h.explainIncident(w, r, tenantID)

	// Incident detail
	case strings.HasPrefix(r.URL.Path, "/v1/incidents/") && r.Method == http.MethodGet:
		h.getIncident(w, r, tenantID)

	default:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	}
}

// --- Channels ---

func (h *WriteAPI) createChannel(w http.ResponseWriter, r *http.Request, tenantID int64) {
	var req struct {
		Kind   string `json:"kind"`
		Target string `json:"target"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	// An e-mail channel may only address someone already on this workspace's
	// People list. Any address at all would make us a free mailer: type a
	// stranger's address, press Send test, and we deliver to somebody who never
	// asked us for anything. The screen offers a dropdown of exactly these
	// people; this is the same rule where it cannot be bypassed by posting.
	if req.Kind == "email" {
		var known bool
		_ = h.pool.Raw().QueryRow(r.Context(),
			`SELECT EXISTS (
			   SELECT 1 FROM tenant_member tm JOIN person p ON p.id = tm.person_id
			    WHERE tm.tenant_id = $1 AND lower(p.email) = lower($2))`,
			tenantID, req.Target).Scan(&known)
		if !known {
			writeAPIErr(w, http.StatusBadRequest, "unknown_recipient")
			return
		}
	}
	// Returns the SAME id shape GET /v1/channels hands out (public_id hex), not
	// the row number: two id shapes for one entity is how delete and test ended
	// up parsing a uuid as an integer and silently doing nothing.
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

// channelRowID resolves whichever id the caller is holding to the row's own.
//
// GET /v1/channels hands out `public_id` as 32 hex chars, POST /v1/channels used
// to answer with the numeric one, and both delete and test parsed the id as an
// integer — so every channel that came from the LIST parsed to 0 and the DELETE
// matched nothing while still answering 204. On screen that was "I cannot remove
// my only e-mail address": the row went away optimistically and came back on the
// next read. Returns 0 when nothing matches, which callers must treat as "not
// yours", never as "deleted".
func (h *WriteAPI) channelRowID(ctx context.Context, tenantID int64, raw string) int64 {
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

func (h *WriteAPI) deleteChannel(w http.ResponseWriter, r *http.Request, tenantID int64) {
	// Deleting the last e-mail channel is allowed. Nobody has to be reachable by
	// mail — Telegram, Discord and a webhook are all destinations, and a product
	// that refuses to stop e-mailing you is a product you cannot leave.
	id := h.channelRowID(r.Context(), tenantID, pathLast(r.URL.Path))
	if id == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_channel")
		return
	}
	_, _ = h.pool.Raw().Exec(r.Context(),
		`DELETE FROM alert_channel WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	w.WriteHeader(http.StatusNoContent)
}

// patchChannel changes what the channel is notified about
// (docs/plans/channel-notify-settings.md). The only PATCHable thing is
// `notify`: the destination itself stays immutable — delete and re-add is the
// honest way to change where a message lands.
func (h *WriteAPI) patchChannel(w http.ResponseWriter, r *http.Request, tenantID int64) {
	id := h.channelRowID(r.Context(), tenantID, pathLast(r.URL.Path))
	if id == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_channel")
		return
	}
	var req struct {
		Notify notifysettings.Patch `json:"notify"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	// PAID ONLY: the 15-minute resolve follow-up is on every paid plan. Turning
	// it on from Free answers 402 in the exact upgrade shape the client reads — the
	// screen's own gate (the toggle opens the modal client-side) is a courtesy,
	// this is the gate.
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
	// apart from "sent as false", so toggling one checkbox never resets the
	// other four. The resolved (never sparse) object is what gets stored and
	// returned.
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

func (h *WriteAPI) testChannel(w http.ResponseWriter, r *http.Request, tenantID int64) {
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
	// The delivery's id travels back so the caller can poll the OUTCOME
	// (audit §3): "queued" is the only honest synchronous answer — whether the
	// message left is a fact the delivery pipeline owns, reported by
	// GET /v1/deliveries/{id} as sent/dead. A UI that answered `sent` here was
	// reporting a delivery that had not happened yet (and on a stack with no
	// mailer, never would).
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

// getDelivery answers GET /v1/deliveries/{id} — one queue row's outcome,
// tenant-scoped. This is where "Send test" gets its truth from: the state is
// the queue's own vocabulary (pending/sent/dead), never a guess, and a dead
// row carries the reason it died so the owner learns WHY their alert did not
// arrive instead of learning nothing.
func (h *WriteAPI) getDelivery(w http.ResponseWriter, r *http.Request, tenantID int64) {
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

// --- Recipients ---

func (h *WriteAPI) createRecipient(w http.ResponseWriter, r *http.Request, tenantID int64) {
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Role == "" {
		req.Role = "notify"
	}
	// Find or create person by email.
	var personID int64
	err := h.pool.Raw().QueryRow(r.Context(),
		`SELECT id FROM person WHERE email = $1`, req.Email).Scan(&personID)
	if err != nil {
		_ = h.pool.Raw().QueryRow(r.Context(),
			`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, '') RETURNING id`,
			req.Email).Scan(&personID)
	}
	_, _ = h.pool.Raw().Exec(r.Context(),
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, $3, 'pending')
		 ON CONFLICT DO NOTHING`, tenantID, personID, req.Role)
	writeAPIJSON(w, http.StatusCreated, map[string]any{
		"id":     strconv.FormatInt(personID, 10),
		"email":  req.Email,
		"role":   req.Role,
		"status": "pending",
	})
}

// personRowID is channelRowID for People: GET /v1/recipients hands out the
// person's public_id, and these two handlers parsed it as an integer — so a role
// change and a removal both matched nothing and answered success. Same defect,
// same fix; the optimistic row simply came back on the next read.
func (h *WriteAPI) personRowID(ctx context.Context, tenantID int64, raw string) int64 {
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

func (h *WriteAPI) patchRecipient(w http.ResponseWriter, r *http.Request, tenantID int64) {
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

func (h *WriteAPI) deleteRecipient(w http.ResponseWriter, r *http.Request, tenantID int64) {
	personID := h.personRowID(r.Context(), tenantID, pathLast(r.URL.Path))
	if personID == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_person")
		return
	}
	// Full revocation, one transaction (design D7): the member goes, their
	// telegram destinations go (the next alert must not reach that chat),
	// their unused invites burn, and their sessions die — so a removed member
	// loses alerts AND the Mini App at once, with nothing left to race. The
	// person row itself stays: the Telegram account exists even unlinked.
	tx, err := h.pool.Raw().Begin(r.Context())
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	for _, q := range []string{
		`DELETE FROM tenant_member WHERE person_id = $1 AND tenant_id = $2`,
		`DELETE FROM alert_channel WHERE tenant_id = $2 AND kind = 'telegram' AND recipient_person_id = $1`,
		`UPDATE telegram_invite SET expires_at = now() WHERE tenant_id = $2 AND invited_by = $1 AND redeemed_at IS NULL`,
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

// --- Account data ---

// GET /v1/export — everything this account owns, as one JSON document. Taking
// your data out is not a feature to earn: it is the answer to "what happens if
// we leave", and a product that cannot answer it is a trap. Secrets are not
// included — an export is a copy of the record, not of the credentials.
func (h *WriteAPI) exportAll(w http.ResponseWriter, r *http.Request, tenantID int64) {
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
	if rows, err := h.pool.Queries().ListIncidentsByTenant(ctx,
		sqlc.ListIncidentsByTenantParams{TenantID: tenantID, Limit: 1000}); err == nil {
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

// DELETE /v1/project — delete the tenant and everything under it. Every table
// hangs off tenant/project with ON DELETE CASCADE, so one delete is the whole
// account; the session goes with it, because the person who pressed this must
// not land back on a dashboard reading someone's leftovers.
func (h *WriteAPI) deleteProject(w http.ResponseWriter, r *http.Request, tenantID int64) {
	if _, err := h.pool.Raw().Exec(r.Context(), `DELETE FROM tenant WHERE id = $1`, tenantID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	session.ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// --- Sources ---

func (h *WriteAPI) connectSource(w http.ResponseWriter, r *http.Request, tenantID int64) {
	// Persist the connection. A per-kind OAuth/token exchange is out of scope
	// for this pass; this records the source_connection row (status ok, not
	// paused) and returns its id so the front's source list reflects it (was a
	// static stub that created a row but returned a fake id).
	ctx := r.Context()
	parts := strings.Split(strings.TrimRight(r.URL.Path, "/"), "/")
	kind := ""
	if len(parts) >= 2 {
		kind = parts[len(parts)-2]
	}
	// `activate` is the copy button speaking (looking creates nothing): the
	// panel fetches the URL with a plain connect (draft stays hidden), and the
	// moment the reader copies it the same endpoint promotes the draft into
	// the visible card — still "waiting…" until the first event proves it.
	var body struct {
		Activate bool `json:"activate"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	var projectID int64
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT id FROM project WHERE tenant_id = $1 ORDER BY id LIMIT 1`, tenantID).Scan(&projectID)
	var id int64
	var paused bool
	var lastSignal pgtype.Timestamptz
	var hookToken string
	// 'draft', not 'waiting' (looking creates nothing — user decision, Aug 14,
	// 2026): opening the hook panel calls this endpoint to get the URL, and
	// browsing must not leave a connection card behind. A draft row is the
	// token's storage and nothing else — the source list hides it until the
	// first event arrives on the hook, which is when the feed has proven it is
	// a feed (the token route promotes it to 'ok' and sets last_signal_at).
	//
	// Connecting is idempotent: one connection per kind, so a second press has
	// nothing new to record — it used to insert a second row and the screen
	// grew a duplicate "Deploy hooks" card per click. ON CONFLICT returns the
	// row that already exists and changes nothing about it (a DO UPDATE that
	// reset `paused` would silently resume a feed the owner had paused, one
	// that re-rolled hook_token would break every URL already pasted somewhere,
	// and one that reset `status` would demote a proven connection to a draft).
	//
	// The hook token is the connection's own inbound URL (universal hooks,
	// docs/plans/universal-hooks.md): POST /hooks/{token} attributes events to
	// this tenant and project, whoever the poster is.
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

// newHookToken mints the per-connection inbound-hook credential: 16 bytes of
// crypto/rand as hex. The URL carrying it is the whole auth for a write-only
// event sink, so it must be unguessable; it is revoked by disconnecting.
func newHookToken() string {
	b := make([]byte, 16)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}

func (h *WriteAPI) deleteSource(w http.ResponseWriter, r *http.Request, tenantID int64) {
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
func (h *WriteAPI) patchSource(w http.ResponseWriter, r *http.Request, tenantID int64) {
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

// --- Status page ---

// GET /v1/status-page — what this tenant publishes. The components ARE the
// tenant's checks: a status page whose components were a separate hand-kept list
// is a second source of truth that drifts from the monitors it describes, and
// the uptime on each row is measured, not typed in.
func (h *WriteAPI) getStatusPage(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	cfg := h.statusConfig(ctx, tenantID)
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"slug":          cfg.Slug,
		"title":         cfg.Title,
		"domain":        cfg.Domain,
		"components":    h.statusComponents(ctx, tenantID, cfg, false),
		"network":       h.statusNetwork(ctx, tenantID),
		"showNetwork":   cfg.ShowNetwork,
		"showIncidents": cfg.ShowIncidents,
		"showPoweredBy": cfg.ShowPoweredBy,
	})
}

// PUT /v1/status-page — persist the settings. The components themselves are not
// stored: they are the monitors, and only the decision to publish each one is
// the owner's to keep.
func (h *WriteAPI) putStatusPage(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	var req struct {
		Title         string          `json:"title"`
		Domain        string          `json:"domain"`
		Shown         map[string]bool `json:"shown"`
		ShowNetwork   *bool           `json:"showNetwork"`
		ShowIncidents *bool           `json:"showIncidents"`
		ShowPoweredBy *bool           `json:"showPoweredBy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "bad_body")
		return
	}
	cfg := h.statusConfig(ctx, tenantID)
	if req.Shown != nil {
		cfg.Shown = req.Shown
	}
	if req.ShowNetwork != nil {
		cfg.ShowNetwork = *req.ShowNetwork
	}
	if req.ShowIncidents != nil {
		cfg.ShowIncidents = *req.ShowIncidents
	}
	if req.ShowPoweredBy != nil {
		cfg.ShowPoweredBy = *req.ShowPoweredBy
	}
	cfg.Title = req.Title
	cfg.Domain = req.Domain

	raw, _ := json.Marshal(cfg)
	var projectID int64
	var projectDomain string
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT id, domain FROM project WHERE tenant_id = $1 ORDER BY id LIMIT 1`, tenantID).Scan(&projectID, &projectDomain)
	// First save of a page that has never existed: name it after the project's
	// own domain rather than its id. Safe precisely because there is no page yet,
	// so no link to it has been handed out; an existing slug is never rewritten.
	if cfg.Slug == "prj-"+strconv.FormatInt(projectID, 10) {
		if claimed := h.claimSlug(ctx, projectDomain, projectID); claimed != "" {
			cfg.Slug = claimed
		}
	}
	if _, err := h.pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title, config)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (slug) DO UPDATE SET title = EXCLUDED.title, config = EXCLUDED.config`,
		tenantID, projectID, cfg.Slug, cfg.Title, raw); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"slug":          cfg.Slug,
		"title":         cfg.Title,
		"domain":        cfg.Domain,
		"components":    h.statusComponents(ctx, tenantID, cfg, false),
		"network":       h.statusNetwork(ctx, tenantID),
		"showNetwork":   cfg.ShowNetwork,
		"showIncidents": cfg.ShowIncidents,
		"showPoweredBy": cfg.ShowPoweredBy,
	})
}

// statusPageConfig is the owner's decisions about the page. Everything else on
// it is measured.
type statusPageConfig struct {
	Slug          string          `json:"slug"`
	Title         string          `json:"title"`
	Domain        string          `json:"domain"`
	Shown         map[string]bool `json:"shown"`
	ShowNetwork   bool            `json:"showNetwork"`
	ShowIncidents bool            `json:"showIncidents"`
	ShowPoweredBy bool            `json:"showPoweredBy"`
}

// statusConfig loads the saved settings, defaulting a page that has never been
// configured to "publish everything" — the page exists to be public.
func (h *WriteAPI) statusConfig(ctx context.Context, tenantID int64) statusPageConfig {
	cfg := statusPageConfig{ShowNetwork: true, ShowIncidents: true, ShowPoweredBy: true, Shown: map[string]bool{}}
	var projectID int64
	var domain, title, slug *string
	var raw []byte
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT p.id, s.slug, s.title, s.domain, s.config
		   FROM project p LEFT JOIN status_page s ON s.tenant_id = p.tenant_id
		  WHERE p.tenant_id = $1 ORDER BY p.id LIMIT 1`, tenantID).Scan(&projectID, &slug, &title, &domain, &raw)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	// The stored slug is the address the owner has already handed out, so it
	// wins over anything in the body and over the id-shaped default. A project
	// with no page yet still resolves by "prj-N" (publicStatus accepts both).
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
	if domain != nil && cfg.Domain == "" {
		cfg.Domain = *domain
	}
	return cfg
}

// statusBars is how many daily bars the page draws — one per retained day. The
// checks table keeps 7 days, so drawing 60 would be drawing 53 days we never saw.
const statusBars = 7

// A day is the wrong bucket for an account that started this morning: its check
// has been running for minutes, so six of the seven bars would read `nodata` for
// a week and the page would look broken on exactly the day somebody was shown
// it. Until there is a day of history, one bar is one check — the row fills as
// the fleet works, which is what a new owner is actually watching for.
const (
	statusIntervalBars = 12             // one per check, so the strip is the last 12 runs
	statusDayThreshold = 24 * time.Hour // older history than this and days win
)

// barSpanFor decides the bucket for one monitor: a day, or its own check
// interval. `oldest` is the first check we still hold (zero when there are none
// at all — a monitor created a minute ago, which is the case this exists for).
func barSpanFor(oldest time.Time, intervalSec int32, now time.Time) time.Duration {
	if !oldest.IsZero() && now.Sub(oldest) >= statusDayThreshold {
		return 24 * time.Hour
	}
	span := time.Duration(intervalSec) * time.Second
	if span <= 0 {
		span = 5 * time.Minute
	}
	return span
}

// statusComponents renders one component per monitor, with uptime and the daily
// bars measured from the checks table. `publicOnly` drops the ones the owner has
// unpublished.
func (h *WriteAPI) statusComponents(ctx context.Context, tenantID int64, cfg statusPageConfig, publicOnly bool) []map[string]any {
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

	// One query for every monitor's daily ok/total over the retained window, plus
	// the first check we still hold — which is what decides whether a day is a
	// bucket this account can fill at all.
	type day struct{ ok, total uint64 }
	daily := map[int64]map[int64]day{}
	oldest := map[int64]time.Time{}
	if h.ch != nil {
		chRows, cerr := h.ch.Raw().Query(ctx, `
			SELECT monitor_id, toStartOfDay(ts) AS d, countIf(ok = 1), count(), min(ts)
			  FROM checks
			 WHERE tenant_id = ? AND ts >= now() - INTERVAL 7 DAY
			 GROUP BY monitor_id, d`, uint64(tenantID))
		if cerr == nil {
			for chRows.Next() {
				var monID uint64
				var d, first time.Time
				var ok, total uint64
				if chRows.Scan(&monID, &d, &ok, &total, &first) == nil {
					id := int64(monID)
					m := daily[id]
					if m == nil {
						m = map[int64]day{}
						daily[id] = m
					}
					m[d.UTC().Unix()] = day{ok: ok, total: total}
					if prev, seen := oldest[id]; !seen || first.Before(prev) {
						oldest[id] = first.UTC()
					}
				}
			}
			_ = chRows.Close()
		}
	}

	// Monitors young enough to be drawn per check, and how far back their strip
	// reaches — the widest of them bounds the one raw query below.
	spans := map[int64]time.Duration{}
	var rawFrom time.Time
	for _, m := range mons {
		span := barSpanFor(oldest[m.id], m.intervalSec, now)
		spans[m.id] = span
		if span >= 24*time.Hour {
			continue
		}
		if from := now.Add(-span * statusIntervalBars); rawFrom.IsZero() || from.Before(rawFrom) {
			rawFrom = from
		}
	}

	// Per-check buckets, counted BACKWARDS from now rather than aligned to the
	// clock: a probe running every 5 minutes is not aligned to :00/:05, so a
	// wall-clock bucket would sometimes hold two checks and its neighbour none —
	// drawing a gap into a row where nothing was ever missed.
	recent := map[int64]map[int]day{}
	if h.ch != nil && !rawFrom.IsZero() {
		chRows, cerr := h.ch.Raw().Query(ctx, `
			SELECT monitor_id, ts, ok FROM checks
			 WHERE tenant_id = ? AND ts >= ?`, uint64(tenantID), rawFrom)
		if cerr == nil {
			for chRows.Next() {
				var monID uint64
				var ts time.Time
				var okFlag uint8
				if chRows.Scan(&monID, &ts, &okFlag) != nil {
					continue
				}
				id := int64(monID)
				span := spans[id]
				if span <= 0 || span >= 24*time.Hour {
					continue
				}
				bucket := int(now.Sub(ts.UTC()) / span)
				if bucket < 0 || bucket >= statusIntervalBars {
					continue
				}
				m := recent[id]
				if m == nil {
					m = map[int]day{}
					recent[id] = m
				}
				entry := m[bucket]
				entry.total++
				if okFlag == 1 {
					entry.ok++
				}
				m[bucket] = entry
			}
			_ = chRows.Close()
		}
	}

	today := now.Truncate(24 * time.Hour)
	out := make([]map[string]any, 0, len(mons))
	for _, m := range mons {
		shown, ok := cfg.Shown[m.key]
		if !ok {
			shown = true // a new check is published unless the owner says otherwise
		}
		if publicOnly && !shown {
			continue
		}
		span := spans[m.id]
		count := statusBars
		if span < 24*time.Hour {
			count = statusIntervalBars
		}
		bars := make([]string, count)
		var okTotal, total uint64
		for i := range count {
			var d day
			if span >= 24*time.Hour {
				d = daily[m.id][today.AddDate(0, 0, -(count-1-i)).Unix()]
			} else {
				d = recent[m.id][count-1-i] // bucket 0 is the newest, so it lands last
			}
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
			// What one bar covers. The page prints its own axis from this, so a
			// strip of five-minute bars can never be labelled "7 days ago".
			"barSpanSec": int(span / time.Second),
		})
	}
	return out
}

// pctLabelAPI is read_api's pctLabel — same rendering, same "—" for no data.
func pctLabelAPI(ok, total uint64) string { return pctLabel(ok, total) }

// statusNetwork is the "Network" section of a status page: the phases of the
// request the fleet already times on every probe (dns_ms, connect_ms, tls_ms,
// total_ms in the checks table), as medians over the retained window.
//
// It used to be `networkChecks` from mockData, rendered only when the backend
// was unreachable — so the owner's own switch ("Show the Network section") could
// not do anything on a live page, and turning it on showed nothing at all. The
// same rule as the landing's probe strip: this is measured or it is absent, and
// a phase that never ran (no TLS on a plaintext host) produces no tile rather
// than a zero.
func (h *WriteAPI) statusNetwork(ctx context.Context, tenantID int64) []map[string]any {
	if h.ch == nil {
		return []map[string]any{}
	}
	var dns, connect, tls, total float64
	var samples uint64
	err := h.ch.Raw().QueryRow(ctx, `
		SELECT median(dns_ms), median(connect_ms), median(tls_ms), median(total_ms), count()
		  FROM checks
		 WHERE tenant_id = ? AND ts >= now() - INTERVAL 24 HOUR AND ok = 1`,
		uint64(tenantID)).Scan(&dns, &connect, &tls, &total, &samples)
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
		{"tls", tls, "handshake"},
		{"response", total, "start to finish"},
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		// Zero is silence: a plaintext host records no handshake, and a reused
		// connection no lookup. Only `response` is always real.
		if r.ms == 0 && r.label != "response" {
			continue
		}
		out = append(out, map[string]any{
			"label": r.label,
			"value": msLabel(r.ms),
			"note":  r.note,
			// Green is a claim about health, and a timing is not one. These tiles
			// report what the request cost; whether the site is up is the
			// components section's job, measured and displayed there.
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

// --- Logs ---

// logQueryBuilder resolves the tenant's project and ring cutoff into the ONLY
// permitted path to the logs table (invariant 4): the window is whatever
// project_window.cutoff_seq currently is — queries below it are displaced by
// the ring and not served.
func (h *WriteAPI) logQueryBuilder(ctx context.Context, tenantID int64) *query.QueryBuilder {
	var projectID, cutoffSeq int64
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT p.id, COALESCE(pw.cutoff_seq, 0)
		   FROM project p LEFT JOIN project_window pw ON pw.project_id = p.id
		  WHERE p.tenant_id = $1 ORDER BY p.id LIMIT 1`, tenantID).Scan(&projectID, &cutoffSeq)
	return query.New(tenantID, projectID, cutoffSeq)
}

func (h *WriteAPI) getLogs(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	qb := h.logQueryBuilder(ctx, tenantID)
	q := r.URL.Query()
	// The spec's window enum, honoured. Absent or unrecognised means the whole
	// ring — the ring is a line count, not a time range (backend-from-new-plan.md
	// §0.3), so a bad value must not invent a narrower window than was asked for.
	window := parseLogWindow(q.Get("window"))
	// The range the reader dragged on the timeline. It bounds the lines and the
	// count beside them, and deliberately NOT the volume: the strip is the map
	// the range was picked on, and narrowing it to the chosen range would leave
	// the reader zoomed in with nothing left to navigate by.
	within := parseLogRange(q)
	// Both filters are repeatable (?service=api&service=web&level=error). A
	// service value may be the empty string — the unlabelled service is a real
	// row and has to be pickable — so presence, not non-emptiness, is what
	// distinguishes "filtered" from "everything".
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
	// Sub-minute resolution for the range the reader is holding, and only for
	// that range. `volume` above stays the whole-ring map on purpose, so this
	// cannot replace it: it says what one of its minutes is made of. Absent
	// unless it adds something — DetailBucketSeconds answers 0 for a range wide
	// enough that every bucket it could offer is coarser than the minute the map
	// already draws, and the same numbers under a second name would be a claim
	// about precision nobody measured.
	if size := query.DetailBucketSeconds(parseBucketSeconds(q.Get("bucketSeconds")), within); size > 0 {
		if rows := h.runBucketRows(ctx, qb.VolumeDetail(size, within, levels, services), "bucket"); rows != nil {
			body["detail"] = map[string]any{"bucketSeconds": size, "buckets": rows}
		}
	}
	// What the window holds, whatever is being read from it: the picker is built
	// from this, so narrowing it by the service already picked would leave the
	// reader holding their own choice. Absent when there is nothing to list
	// ("zero is silence") — one service is still sent, and the panel draws no
	// picker for it.
	if services := h.runServiceRows(ctx, qb.Services(window)); len(services) > 0 {
		body["services"] = services
	}
	writeAPIJSON(w, http.StatusOK, body)
}

// parseLogLevels keeps the spec's enum (error, warn, info), deduplicated. All
// three buckets partition the stream, so picking every one is the same as
// picking none — normalised to nil here so the builder skips the predicate.
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

// How many lines one read of /v1/logs returns. The ring holds far more, and
// `total` beside them says so — this is the cap on one answer, not on the
// window. Raised from 200 when the timeline learned to ask for a range: with
// bounds on the wire a read is a slice of the ring rather than its newest tail,
// and 200 truncated a busy five minutes. Kept where the stream can still be
// rendered in one pass — the list is not virtualised, so this number is also a
// budget of DOM nodes.
const streamLines = 1000

// parseLogRange reads the timeline's `from`/`to` bounds off the query. Either
// may be sent alone. Anything unparseable is dropped rather than defaulted, for
// the same reason parseLogWindow drops an unknown enum: a bad timestamp must
// not invent a narrower window than the reader asked for.
// parseBucketSeconds reads the detail resolution the reader is asking for. A
// value that is not a positive number is not an error worth refusing the whole
// read over — it means no detail, and the strip falls back to its minutes,
// which is what a client that never asked also gets.
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

// countRange picks what `total` is counted over. The `window` enum predates the
// timeline and still answers on its own; an explicit range always wins, because
// the two are the same question asked twice and honouring both would count a
// slice of a slice.
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

// runWindowCount counts the window's lines before the stream limit, so the
// panel can say how much of the window it is showing. Same filters as Stream,
// same cutoff — a different predicate here would make the number a lie.
func (h *WriteAPI) runWindowCount(ctx context.Context, qb *query.QueryBuilder, within query.Range, levels, services []string, search string) int {
	if h.ch == nil {
		return 0
	}
	// The builder owns the predicate so it cannot drift from Stream's.
	lq := qb.WindowCount(within, levels, services, search)
	rows, err := h.ch.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return 0
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		// count() is UInt64 in ClickHouse and the native driver refuses to
		// narrow it into int64 — the same reason logSummary scans uint64.
		var n uint64
		if err := rows.Scan(&n); err != nil {
			continue
		}
		return int(n)
	}
	return 0
}

// runLogRows executes a QueryBuilder stream query against ClickHouse and maps
// the rows to the front's log-line shape. Returns nil on any error so the
// caller can substitute an empty array.
func (h *WriteAPI) runLogRows(ctx context.Context, lq query.LogQuery) []map[string]any {
	if h.ch == nil {
		return nil
	}
	rows, err := h.ch.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
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
func (h *WriteAPI) runServiceRows(ctx context.Context, lq query.LogQuery) []map[string]any {
	if h.ch == nil {
		return nil
	}
	rows, err := h.ch.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var name string
		// count() is UInt64 and the native driver refuses to narrow it, same as
		// runWindowCount.
		var lines uint64
		if err := rows.Scan(&name, &lines); err != nil {
			continue
		}
		out = append(out, map[string]any{"name": name, "lines": lines})
	}
	return out
}

// runBucketRows executes a histogram query — the per-minute map or the
// range-bounded detail — and names the timestamp column what that answer calls
// it. The two differ by one word on the wire because they measure different
// widths, and calling a five-second count `minute` is the kind of name that
// outlives the person who knew better.
func (h *WriteAPI) runBucketRows(ctx context.Context, lq query.LogQuery, key string) []map[string]any {
	if h.ch == nil || lq.SQL == "" {
		return nil
	}
	rows, err := h.ch.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
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

func (h *WriteAPI) explainLogs(w http.ResponseWriter, r *http.Request, tenantID int64) {
	// Real path through internal/ai: idempotent by input hash (scenario key +
	// version + brain + meta line + lines, Decision 7), cached by fingerprint,
	// metered against the plan's monthly quota (402 on exhaustion, not 403).
	// No key anywhere (env or the Settings-set one) = 503 ai_not_configured —
	// Explain is off, never a canned fallback.
	ctx := r.Context()
	var req struct {
		Lines []string `json:"lines"`
	}
	// Cap the body before anything is counted: the caps below promise a bounded
	// input, and an unbounded decode would buffer the body first.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || len(req.Lines) == 0 {
		writeAPIErr(w, http.StatusBadRequest, "missing_lines")
		return
	}
	// The registry owns the caps: over-cap input is rejected, never trimmed,
	// and the message quotes the caps so it cannot drift from them.
	sc := ai.ExplainLogs
	total, over := 0, len(req.Lines) > sc.MaxInputLines
	for _, line := range req.Lines {
		if len(line) > sc.MaxLineBytes {
			over = true
		}
		total += len(line)
	}
	if over || total > sc.MaxInputBytes {
		writeAPIErrMsg(w, http.StatusBadRequest, "input_too_large",
			fmt.Sprintf("Explain reads at most %d lines (%d KiB, %d bytes per line).", sc.MaxInputLines, sc.MaxInputBytes/1024, sc.MaxLineBytes))
		return
	}
	// Per-tenant throttle, standing between validation and the first read.
	// Anything that passes validation spends a batch of reads (plan,
	// entitlement, explain context) whether it ends in a cached answer, a 402
	// or a fresh one, so each burns a slot; a validation failure above spends
	// nothing and must not. Retry-After tells machine callers what the message
	// tells the human — the window releases on its own.
	if ok, retryAfter := h.explainAllow(tenantID); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int64((retryAfter+time.Second-1)/time.Second)))
		writeAPIErrMsg(w, http.StatusTooManyRequests, "explain_rate_limited", "Too many Explain requests. Try again in a minute.")
		return
	}
	// Quota gate, fail closed: a plan or entitlement read that fails is a 500,
	// never a silent 0 = "unlimited". NULL ai_explains (paid plans) is the
	// only thing that means unlimited.
	plan, perr := tenantPlan(ctx, h.pool, tenantID)
	if perr != nil && !errors.Is(perr, pgx.ErrNoRows) {
		slog.Error("ai explain: read tenant plan failed", "err", perr, "tenant_id", tenantID)
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	ent, eerr := h.pool.Queries().GetPlanEntitlement(ctx, plan)
	if errors.Is(eerr, pgx.ErrNoRows) {
		// tenant.plan is free text with no FK to plan_entitlement, and the
		// other entitlement readers tolerate a miss (read_api, monitors). An
		// unknown tier — rename, legacy row, unshipped billing plan — must
		// not 500 this endpoint forever: the most restrictive seeded row is
		// the fail-closed answer for a plan with no row of its own.
		slog.Warn("ai explain: plan has no entitlement row, using Free", "plan", plan, "tenant_id", tenantID)
		ent, eerr = h.pool.Queries().GetPlanEntitlement(ctx, "Free")
	}
	if eerr != nil {
		slog.Error("ai explain: read plan entitlement failed", "err", eerr, "plan", plan, "tenant_id", tenantID)
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// The meta line is the stable half of the explain context: it changes only
	// when the installer re-uploads the spec, so it is the one context entry
	// the cache hash covers (Decision 7) — everything explainContext gathers
	// below is volatile and rides along unhashed.
	metaLine := ""
	if meta, err := h.pool.Queries().GetProjectMeta(ctx, tenantID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			// The meta line is a hashed part, so a transient read failure does
			// not just narrow the prompt — it silently re-keys the tenant's
			// cache (a guaranteed miss, a billable provider call, and a second
			// permanent row under the no-meta key). The operator must see why.
			slog.Warn("ai explain: project meta read failed", "err", err, "tenant_id", tenantID)
		}
	} else if len(meta) > 0 {
		metaLine = projectMetaLine(meta)
	}
	res, err := h.acct.Explain(ctx, tenantID, sc,
		ai.Input{Lines: req.Lines, MetaLine: metaLine, Context: h.explainContext(ctx, tenantID)}, ent.AiExplains)
	if err != nil {
		if errors.Is(err, ai.ErrNotConfigured) {
			writeAPIErrMsg(w, http.StatusServiceUnavailable, "ai_not_configured",
				h.aiNotConfiguredMsg())
			return
		}
		if errors.Is(err, ai.ErrOverQuota) {
			writeUpgradeRequired(w, "Your plan's monthly AI-explain quota is used up.")
			return
		}
		// The underlying cause must be traceable server-side — a revoked
		// provider key used to be a bare 500 with no trace.
		slog.Error("ai explain failed", "err", err, "tenant_id", tenantID)
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"problem":     res.Answer.Problem,
		"cause":       res.Answer.Cause,
		"confidence":  res.Answer.Confidence,
		"fix":         res.Answer.Fix,
		"investigate": res.Answer.Investigate,
		"cached":      res.Cached,
		"used":        res.Used,
		"limit":       res.Limit,
		// Dev observability (the prompt-editing loop): the exact user message
		// the model received. Empty string on a cache hit. The system prompt is
		// static and lives in scenario.go — printing it per response would be
		// noise; docker logs carry it when UC_AI_LOG_PROMPT=1.
		"prompt": res.Prompt,
	})
}

// previewExplain answers the prompt-editing question "what exactly is about
// to be sent?" at the moment of dispatch, not when the model answers: the same
// validation, the same meta line and context composition as explainLogs, and
// NO model call, NO quota, NO throttle slot — it spends nothing, so a dev
// tools loop can inspect the exact bytes for free. The browser console logs
// this alongside the real call (the front fires both concurrently).
func (h *WriteAPI) previewExplain(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	var req struct {
		Lines []string `json:"lines"`
	}
	// Unlike explainLogs, an empty `lines` is a legitimate call here: Settings
	// reads `model` off this endpoint to learn the wired brain's identity
	// without composing a real explanation (client.ts's explainPreview([])).
	// Only a malformed body is rejected.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	sc := ai.ExplainLogs
	total, over := 0, len(req.Lines) > sc.MaxInputLines
	for _, line := range req.Lines {
		if len(line) > sc.MaxLineBytes {
			over = true
		}
		total += len(line)
	}
	if over || total > sc.MaxInputBytes {
		writeAPIErrMsg(w, http.StatusBadRequest, "input_too_large",
			fmt.Sprintf("Explain reads at most %d lines (%d KiB, %d bytes per line).", sc.MaxInputLines, sc.MaxInputBytes/1024, sc.MaxLineBytes))
		return
	}
	metaLine := ""
	if meta, err := h.pool.Queries().GetProjectMeta(ctx, tenantID); err == nil && len(meta) > 0 {
		metaLine = projectMetaLine(meta)
	}
	input := ai.Input{Lines: req.Lines, MetaLine: metaLine, Context: h.explainContext(ctx, tenantID)}
	// The brain's identity and the generation knobs, so the console shows
	// everything the provider request carries, not only the messages. A null
	// model is the front's "Explain is off" fact: no key resolves anywhere
	// (env or the Settings-set instance key), so there is no brain.
	var model any
	if h.acct.Configured(ctx) {
		model = h.acct.BrainID(ctx)
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"system":            sc.SystemPrompt,
		"user":              input.UserMessage(),
		"model":             model,
		"temperature":       sc.Temperature,
		"max_output_tokens": sc.MaxOutputTokens,
	})
}

// explainIncident is the incident's own explain: the server owns the
// evidence (the incident's facts, its timeline, the log slice frozen when it
// fired), so the request carries no body — only the id in the path. Same
// money path as explainLogs (throttle, monthly quota, 402 shape), new
// scenario key (ai.ExplainIncident) whose answer adds severity and area.
func (h *WriteAPI) explainIncident(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	// pathLast cannot work here (the path ends in /explain), so the id comes
	// from the {id} segment the 1.22 mux binds. It is the PUBLIC id — the
	// only one that ever leaves this process, because `incidentToAPI` sends
	// `uuidStr(row.PublicID)` and the row's serial `id` appears in no
	// response. Reading it with parseID answered 404 for every real click:
	// a uuid parses to 0, and no row owns 0. The internal id comes back from
	// this same lookup, because the evidence queries below are keyed by it.
	pubID := parseUUID(r.PathValue("id"))
	var id int64
	var title, status string
	var detectedAt, resolvedAt pgtype.Timestamptz
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT id, title, status, detected_at, resolved_at FROM incident WHERE public_id = $1 AND tenant_id = $2`,
		pubID, tenantID).Scan(&id, &title, &status, &detectedAt, &resolvedAt); err != nil {
		// Only a row this tenant does not own is a 404 — a dead pool or a
		// timeout must fail closed as a 500, never "not found" for an
		// incident that exists.
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("ai explain: read incident failed", "err", err, "tenant_id", tenantID)
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	// Per-tenant throttle, shared with the logs explain: an incident explain
	// spends the same reads and the same provider money, so it burns the same
	// slots. Retry-After tells machine callers what the message tells the
	// human — the window releases on its own.
	if ok, retryAfter := h.explainAllow(tenantID); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int64((retryAfter+time.Second-1)/time.Second)))
		writeAPIErrMsg(w, http.StatusTooManyRequests, "explain_rate_limited", "Too many Explain requests. Try again in a minute.")
		return
	}
	// Quota gate, fail closed: a plan or entitlement read that fails is a 500,
	// never a silent 0 = "unlimited". NULL ai_explains (paid plans) is the
	// only thing that means unlimited.
	plan, perr := tenantPlan(ctx, h.pool, tenantID)
	if perr != nil && !errors.Is(perr, pgx.ErrNoRows) {
		slog.Error("ai explain: read tenant plan failed", "err", perr, "tenant_id", tenantID)
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	ent, eerr := h.pool.Queries().GetPlanEntitlement(ctx, plan)
	if errors.Is(eerr, pgx.ErrNoRows) {
		// tenant.plan is free text with no FK to plan_entitlement, and the
		// other entitlement readers tolerate a miss (read_api, monitors). An
		// unknown tier — rename, legacy row, unshipped billing plan — must
		// not 500 this endpoint forever: the most restrictive seeded row is
		// the fail-closed answer for a plan with no row of its own.
		slog.Warn("ai explain: plan has no entitlement row, using Free", "plan", plan, "tenant_id", tenantID)
		ent, eerr = h.pool.Queries().GetPlanEntitlement(ctx, "Free")
	}
	if eerr != nil {
		slog.Error("ai explain: read plan entitlement failed", "err", eerr, "plan", plan, "tenant_id", tenantID)
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// Evidence: the same queries incidentWithEvidence builds the card from
	// (read_api.go). The slice is the lines; the timeline (lifecycle rows +
	// the tenant's events around the break) rides as context. An empty slice
	// is not an error — the timeline is the evidence then, so the read runs.
	// But the incident's OWN records are load-bearing on this paid path: a
	// failed read is a logged 500 before anything is charged, never a
	// silently narrowed prompt.
	rows, serr := h.pool.Queries().ListIncidentSlice(ctx, id)
	if serr != nil {
		slog.Error("ai explain: read incident slice failed", "err", serr, "incident_id", id, "tenant_id", tenantID)
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	lines := make([]string, 0, len(rows))
	for _, l := range rows {
		// Same "HH:MM:SS  message" shape the live stream and the card's
		// renderer use, so the model reads what the reader would have seen.
		at := ""
		if l.Ts.Valid {
			at = l.Ts.Time.Format("15:04:05") + "  "
		}
		lines = append(lines, at+l.Message)
	}
	end := time.Now()
	if resolvedAt.Valid {
		end = resolvedAt.Time
	}
	updates, uerr := h.pool.Queries().ListIncidentUpdates(ctx, id)
	if uerr != nil {
		slog.Error("ai explain: read incident updates failed", "err", uerr, "incident_id", id, "tenant_id", tenantID)
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	lifecycle := make([]map[string]any, 0, len(updates))
	for _, u := range updates {
		entry := map[string]any{"time": "", "text": u.Text}
		if u.At.Valid {
			entry["time"] = u.At.Time.Format("15:04")
		}
		lifecycle = append(lifecycle, entry)
	}
	// The window the card cares about: 30 minutes before the open through the
	// close (or now), events nearest the break first among equals. h.ch nil in
	// tests — the lifecycle alone is still a timeline. Best-effort, exactly
	// like the card renderer tolerates it (read_api.go): these events are
	// enrichment around the break, not the incident's own record.
	var events []ch.EventRow
	if h.ch != nil && detectedAt.Valid {
		events, _ = h.ch.EventsAround(ctx, tenantID,
			detectedAt.Time.Add(-30*time.Minute), end, detectedAt.Time, 50)
	}
	// Fact lines first — what the incident IS — then the timeline oldest
	// first (mergeTimeline orders both halves), then the room the logs explain
	// already sends (services, monitors, open incidents).
	open := "closed"
	if status == "down" || status == "check" {
		open = "open"
	}
	minutes := 0
	if detectedAt.Valid {
		minutes = int(end.Sub(detectedAt.Time).Minutes())
	}
	inputCtx := []string{
		"Incident: " + title,
		fmt.Sprintf("Status: %s, running %d minutes", open, minutes),
	}
	for _, entry := range mergeTimeline(lifecycle, events) {
		inputCtx = append(inputCtx, fmt.Sprintf("%v %v", entry["time"], entry["text"]))
	}
	inputCtx = append(inputCtx, h.explainContext(ctx, tenantID)...)
	// The meta line is the stable half of the explain context — the one
	// context entry the cache hash covers; everything explainContext gathers
	// above is volatile and rides along unhashed.
	metaLine := ""
	if meta, merr := h.pool.Queries().GetProjectMeta(ctx, tenantID); merr != nil {
		if !errors.Is(merr, pgx.ErrNoRows) {
			slog.Warn("ai explain: project meta read failed", "err", merr, "tenant_id", tenantID)
		}
	} else if len(meta) > 0 {
		metaLine = projectMetaLine(meta)
	}
	res, err := h.acct.Explain(ctx, tenantID, ai.ExplainIncident,
		ai.Input{Lines: lines, MetaLine: metaLine, Context: inputCtx}, ent.AiExplains)
	if err != nil {
		if errors.Is(err, ai.ErrNotConfigured) {
			writeAPIErrMsg(w, http.StatusServiceUnavailable, "ai_not_configured",
				h.aiNotConfiguredMsg())
			return
		}
		if errors.Is(err, ai.ErrOverQuota) {
			writeUpgradeRequired(w, "Your plan's monthly AI-explain quota is used up.")
			return
		}
		slog.Error("ai explain failed", "err", err, "tenant_id", tenantID)
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"problem":    res.Answer.Problem,
		"cause":      res.Answer.Cause,
		"confidence": res.Answer.Confidence,
		// Incident-scenario additions; null when the answer carried none —
		// honestly absent, never derived client- or server-side.
		"severity":    res.Answer.Severity,
		"area":        res.Answer.Area,
		"fix":         res.Answer.Fix,
		"investigate": res.Answer.Investigate,
		"cached":      res.Cached,
		"used":        res.Used,
		"limit":       res.Limit,
		"prompt":      res.Prompt,
	})
}

// aiNotConfiguredMsg answers the 503 with an instruction the caller can act
// on, which is not the same sentence in both deployments.
//
// On a self-host the operator IS the person reading this, and the Settings
// door accepts a key (see InstanceSettings), so naming it is the whole help.
// On a hosted instance that door answers 404 to every tenant on purpose: an
// instance-level knob writable from a tenant session would let one customer
// steer everyone's brain. Sending a tenant there would be a control that
// cannot act — so the hosted sentence states the fact and asks for nothing.
// The key is the operator's to place, and their being without one is not a
// task the caller can be handed.
func (h *WriteAPI) aiNotConfiguredMsg() string {
	if h.selfHosted {
		return "AI is not configured on this instance. Add an OpenAI-compatible API key in Settings."
	}
	return "AI explains are not available on this instance."
}

// explainContext gathers the volatile server-known facts the prompt receives
// beside the lines (Decision 15a): what is in the window, what is watched,
// whether an incident is open. None of it is hashed (Decision 7) — it is
// sent to the model but changes with the room, so hashing it would bust the
// cache on every incident flap; the stable project-meta line travels
// separately as Input.MetaLine. Every source is best-effort — a context
// read that fails narrows the prompt, it never fails the read.
func (h *WriteAPI) explainContext(ctx context.Context, tenantID int64) []string {
	var out []string
	// Services over the whole visible ring — the selection may come from any
	// of it — capped because service names are customer-named and unbounded.
	if rows := h.runServiceRows(ctx, h.logQueryBuilder(ctx, tenantID).Services(0)); len(rows) > 0 {
		names := make([]string, 0, len(rows))
		for _, row := range rows[:min(len(rows), 10)] {
			if name, _ := row["name"].(string); name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			out = append(out, "services in window: "+strings.Join(names, ", "))
		}
	}
	if mons, err := h.pool.Queries().ListMonitorsByTenant(ctx, tenantID); err == nil {
		for _, m := range mons[:min(len(mons), 10)] {
			out = append(out, "monitors: "+m.Name+" "+m.Target)
		}
	}
	if incRows, _ := h.pool.Queries().ListIncidentsByTenant(ctx,
		sqlc.ListIncidentsByTenantParams{TenantID: tenantID, Limit: 1}); len(incRows) > 0 &&
		(incRows[0].Status == "down" || incRows[0].Status == "check") {
		out = append(out, "open incident: "+incRows[0].Title)
	}
	return out
}

// projectMetaLine renders the installer-collected spec (Decision 15b) as one
// context entry, omitting fields the spec did not carry.
func projectMetaLine(meta []byte) string {
	var spec struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Framework   string `json:"framework"`
		Runtime     string `json:"runtime"`
		Language    string `json:"language"`
	}
	if json.Unmarshal(meta, &spec) != nil {
		return ""
	}
	head := spec.Name
	if spec.Description != "" {
		head += " — " + spec.Description
	}
	var parts []string
	if spec.Framework != "" {
		parts = append(parts, "framework "+spec.Framework)
	}
	if spec.Runtime != "" {
		parts = append(parts, spec.Runtime)
	}
	if spec.Language != "" {
		parts = append(parts, spec.Language)
	}
	if head == "" && len(parts) == 0 {
		return ""
	}
	line := "product: " + head
	if len(parts) > 0 {
		line += "; " + strings.Join(parts, "; ")
	}
	return line
}

// tenantPlan reads the tenant's plan name (Free|Indie|Growth|Agency).
func tenantPlan(ctx context.Context, pool *pg.Pool, tenantID int64) (string, error) {
	var plan string
	err := pool.Raw().QueryRow(ctx, `SELECT plan FROM tenant WHERE id = $1`, tenantID).Scan(&plan)
	return plan, err
}

// --- Incident detail ---

func (h *WriteAPI) getIncident(w http.ResponseWriter, r *http.Request, tenantID int64) {
	idStr := pathLast(r.URL.Path)
	// The public id, same as POST /v1/incidents/{id}/explain: `incidentToAPI`
	// only ever sends `uuidStr(row.PublicID)`, so the serial `id` this used to
	// parse never reaches a caller. Reading it with parseID matched nothing
	// and, because the error was discarded, answered 200 with an empty title
	// and status instead of saying so.
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

// --- Public ---

func (h *WriteAPI) public(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/public/check" && r.Method == http.MethodPost:
		h.publicCheck(w, r)
	case r.URL.Path == "/public/watch" && r.Method == http.MethodPost:
		h.publicWatch(w, r)
	case r.URL.Path == "/public/track" && r.Method == http.MethodPost:
		h.publicTrack(w, r)
	case strings.HasPrefix(r.URL.Path, "/public/status/") && r.Method == http.MethodGet:
		h.publicStatus(w, r)
	default:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	}
}

func (h *WriteAPI) publicCheck(w http.ResponseWriter, r *http.Request) {
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
	// A fresh answer for this host is served to everyone, which is what keeps a
	// link doing the rounds from re-probing the site once per visitor. Checked
	// before the IP cooldown on purpose: a cached answer costs the far side
	// nothing, so there is nothing to throttle.
	if cached, ok := h.cachedCheck(host); ok {
		h.rec.ServerEvent(ctx, "public_check_run", 0, 0, map[string]string{
			"host": bareHost(host), "cached": "true",
		})
		writeAPIJSON(w, http.StatusOK, cached)
		return
	}
	// Anonymous endpoint: throttle per source-IP (plan §6.1). Per-replica is the
	// honest MVP bound — two ucapi replicas admit up to 2× the cooldown.
	if !h.checkAllow(analytics.ClientIP(r), host) {
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	// Real probe BEHIND the SSRF guard: the executor refuses internal ranges
	// (169.254/16, loopback, RFC1918…) and reports error_class=blocked_target.
	// That is exactly what makes a metadata-IP probe return blocked_target, not
	// the metadata body.
	// CollectBody so page discovery can read this homepage's links when the host
	// has no sitemap — the fallback costs no request precisely because the body
	// comes from here.
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
	// guard. Bounded, not a crawl: see internal/probe/discover.
	facts := discover.Run(r.Context(), h.exec, net.DefaultResolver, host, res)
	network := append(networkRowsFrom(res, status), discoveredRows(facts)...)

	// Every pickable row's id IS its URL, so the watch request can send the ids
	// back as targets with no lookup table in between. That includes this one:
	// it used to be the literal "live", which no target list could name.
	groups := []map[string]any{
		{"title": "Live probe", "source": "from a real request", "rows": []map[string]any{
			{"id": host, "name": host, "meta": meta, "status": status, "recommended": res.OK},
		}},
	}
	// Reading order, widest first: the address they typed, then its other hosts,
	// then its pages. Hosts sits above Pages because an api. or auth. host is its
	// own failure domain — separate DNS, separate certificate, usually a separate
	// deploy — so it can be down while the marketing site is perfectly healthy,
	// and that is the outage a status page built from one URL never sees.
	if rows := pageRows(facts.Hosts); len(rows) > 0 {
		groups = append(groups, map[string]any{
			"title": "Hosts", "source": facts.Hosts[0].Source, "rows": rows,
		})
	}
	// The API belongs with the things that carry a checkbox, not in the read-only
	// facts strip: a subdomain API (api.example.com) is already pickable under
	// Hosts, and the path-based one is the same thing on a site that routes
	// instead of subdomaining. Leaving it unpickable made the two inconsistent.
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
	// How many of these rows an account can actually watch. Sent by the server
	// because the plan's numbers live in one place; a landing that hardcoded "3"
	// would be a second source for a number Pricing already owns.
	body["watchLimit"] = h.freeWatchLimit(r.Context())

	h.cacheCheck(host, body)
	writeAPIJSON(w, http.StatusOK, body)
}

// freeWatchLimit is what a brand-new account may watch. The anonymous flow can
// only ever create a Free tenant, so that is the plan to ask about.
func (h *WriteAPI) freeWatchLimit(ctx context.Context) int32 {
	limit, err := h.pool.Queries().GetPlanHTTPChecks(ctx, "Free")
	if err != nil || limit <= 0 {
		return 3
	}
	return limit
}

// stagesFrom turns the executor's phase timings into the waterfall. A phase that
// never ran is left out rather than sent as 0: the difference between "TLS took
// no time" and "there was no TLS" is the whole point of the row.
//
// Two of the five are derived, and the reason is a trap worth naming.
// Result.DNSMs/ConnectMs/TLSMs are durations OF their phase, but Result.TTFBMs
// is measured from the start of the request, so it already contains all three.
// Emitting it as a fourth bar would describe this 546 ms request as 958 ms. The
// bar a reader wants there is what is left once the connection exists — the
// server thinking — so `wait` is TTFB minus the phases that preceded it, and
// `html` is the tail after the first byte (httptrace has no "body done" hook).
// With those two derived, the bars sum to the total, which is the only property
// that makes a waterfall readable.
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

	// The derived two are different: they are computed from timings we already
	// hold, so once a response arrived they ARE measured — including when they
	// round down to zero. A 10 KB single-page shell really does arrive in the
	// same millisecond as its first byte, and reporting that as the no-data
	// marker would say "we never looked" about the one phase we can always
	// compute. Emitted at 0 rather than dropped (the spec's rule: absence means
	// unmeasured, never "took zero").
	//
	// The comparisons stay guarded: a reused connection or a coarse clock can put
	// the phases past TTFB, and an underflow on uint32 would render a bar roughly
	// four billion milliseconds wide.
	preTTFB := res.DNSMs + res.ConnectMs + res.TLSMs
	if res.TTFBMs >= preTTFB {
		stages = append(stages, map[string]any{"label": "wait", "ms": res.TTFBMs - preTTFB})
	}
	if res.TotalMs >= res.TTFBMs {
		stages = append(stages, map[string]any{"label": "html", "ms": res.TotalMs - res.TTFBMs})
	}
	return stages
}

// networkRowsFrom reports one row per fact the probe actually established.
// Facts it cannot measure yet (error page, security headers, health URL) are
// absent, and the landing renders absence as unknown — the alternative, a
// plausible-looking constant, is what the strip used to do.
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
	// RESPONSE is the whole request as the visitor experiences it. It replaced
	// three named cities on the landing: there is one probe region, so Frankfurt
	// / Ashburn / Singapore were a promise the fleet could not keep.
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

// pageRows renders the discovered pages as pickable rows. `recommended` is the
// server's opinion of a sensible default, so the landing stops keeping its own
// list of which boxes start ticked — it had one, keyed by mock ids, and it went
// on counting rows that were no longer on screen.
func pageRows(pages []discover.Page) []map[string]any {
	rows := make([]map[string]any, 0, len(pages))
	for _, p := range pages {
		// Every branch below writes meta, including the default — seeding it with
		// the bare source only looked like a fallback.
		var meta string
		rowStatus := "ok"
		switch {
		case p.Status == 0 && p.Error != "":
			// Asked, and nothing came back. For a monitoring product this is the
			// most interesting row on the screen, not a gap: a host the site
			// links but that does not answer is already broken.
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

// apiRow renders the app's API as a pickable row. It returns nil when there is
// nothing to offer — unmeasured, or measured and absent — because a group with
// no target to watch is not a choice, and the strip's no-data marker is for
// facts, not for things you tick.
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
		// The base is real — the app calls through it — but the root itself does
		// not answer, so a check on it would pin a permanent 404. Offer it, say
		// so, and do NOT pre-tick it.
		note, rowStatus = fmt.Sprintf("%s · root answers %d, pick an endpoint under it", a.Source, a.Status), "check"
	}
	return map[string]any{
		"id": strings.TrimRight(host, "/") + a.Path, "name": a.Path,
		"meta": note, "status": rowStatus,
		// Only a confirmed endpoint is a sensible default to watch.
		"recommended": a.Confirmed,
	}
}

// discoveredRows renders what discover established. A fact it could not measure
// produces no row at all, and the landing shows its no-data marker there — the
// one thing none of these may do is guess.
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

// checkAllow applies a per-replica cooldown per source-IP. The host is no longer
// part of it: a second visitor asking about a popular domain gets the cached
// answer (cachedCheck) instead of a refusal, which is both a better answer and
// fewer requests to that domain.
func (h *WriteAPI) checkAllow(ip, _ string) bool {
	return h.allowOnce("check", ip, 10*time.Second)
}

// watchAllow throttles the anonymous watch, in a bucket of its OWN.
//
// It used to share the check's: a visitor checked a site, ticked a row and
// pressed Watch — all of it inside the check's 10 s cooldown — and got a 429.
// The landing treats a failed watch as "no backend" and walks on to the sample
// status page, so the conversion silently created no account at all. The two
// endpoints also cost different things: a check spends up to discover.MaxRequests
// against a stranger's server, a watch spends one guarded probe and some rows
// here. One bucket could not price both.
func (h *WriteAPI) watchAllow(ip string) bool {
	return h.allowOnce("watch", ip, 3*time.Second)
}

// trackAllow throttles /public/track. One batch per second per IP is the
// honest reading of the 60 req/min/IP budget (plan: product-analytics
// §Decision 2): the client coalesces events (front queues 1.5 s / 20 events),
// so a compliant visitor sends far less than that. Same per-replica caveat
// as allowOnce — two ucapi replicas admit up to 2× — accepted for the MVP
// exactly as the check cooldown is.
func (h *WriteAPI) trackAllow(ip string) bool {
	return h.allowOnce("track", ip, time.Second)
}

// publicTrack is the first-party analytics collection endpoint (plan:
// product-analytics §Decision 2). Always 204 (429 on limit): the write is
// asynchronous, invalid events are dropped individually, and a collector that
// answers errors teaches clients to retry. The uc_vid cookie is minted here
// — the ONLY door that mints — and set on the response; server-side event
// doors (check/watch/sign-in) resolve the cookie but never create one.
func (h *WriteAPI) publicTrack(w http.ResponseWriter, r *http.Request) {
	if !h.trackAllow(analytics.ClientIP(r)) {
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	token, ok := analytics.VisitorToken(r)
	if !ok {
		token = analytics.MintVisitorToken()
		// Secure in prod, matching the session cookie's rule (!devMode, see
		// session.SetCookie): TLS ends at the edge, never at ucapi, so r.TLS
		// is always nil here and would never mark the cookie Secure. Dev runs
		// plain HTTP through Caddy on :80, where a Secure cookie is silently
		// dropped — the visitor would get a new id on every request.
		analytics.SetVisitorCookie(w, token, !h.devMode)
	}
	events, dropped := analytics.ParseBody(r.Body)
	h.rec.CountInvalid(dropped)
	// A live session stamps person/tenant onto the event rows (§Decision 4):
	// /app traffic and anonymous traffic become one behavioural stream. Public
	// route, so a session read failure simply means anonymous.
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

// allowOnce is the shared per-replica cooldown: one timestamp per (bucket, ip).
// Per-replica is the honest MVP bound — two ucapi replicas admit up to 2× — and
// the sliding window that holds across replicas lives in Postgres for the codes
// this door issues (magic_link_ip).
func (h *WriteAPI) allowOnce(bucket, ip string, cooldown time.Duration) bool {
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

// explainAllow admits one explain request for a tenant, or refuses when the
// tenant already holds explainBurst slots inside the last minute; on refusal
// the second return value is how long until the oldest slot ages out, for the
// Retry-After header. A slot is recorded only when admitted, so a refused
// burst never extends its own lockout. The map is made lazily because the
// handler tests build WriteAPI by struct literal, which skips NewWriteAPI's
// initialisation.
func (h *WriteAPI) explainAllow(tenantID int64) (bool, time.Duration) {
	h.checkMu.Lock()
	defer h.checkMu.Unlock()
	if h.explainSeenAt == nil {
		h.explainSeenAt = map[int64][]time.Time{}
	}
	now := time.Now()
	slots := h.explainSeenAt[tenantID]
	kept := slots[:0]
	for _, t := range slots {
		if now.Sub(t) < explainWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= explainBurst {
		h.explainSeenAt[tenantID] = kept
		// kept is non-empty here and in time order, so kept[0] is the slot
		// that releases the window first.
		return false, explainWindow - now.Sub(kept[0])
	}
	h.explainSeenAt[tenantID] = append(kept, now)
	if len(h.explainSeenAt) > 512 { // opportunistic GC keeps the map bounded
		for id, ts := range h.explainSeenAt {
			// Slots are appended in time order, so the last one is the newest;
			// every stored slice is non-empty (both exits above store one).
			if now.Sub(ts[len(ts)-1]) > 5*time.Minute {
				delete(h.explainSeenAt, id)
			}
		}
	}
	return true, 0
}

// cachedCheck returns a previous answer for this host, if it is still fresh.
//
// This is the piece that makes discovery affordable: a check now costs up to
// discover.MaxRequests against somebody else's server, and without a cache every
// visitor curious about the same popular domain would spend them again. Two
// replicas mean the ceiling is two crawls per host per TTL, which is bounded and
// harmless — a shared table in Postgres is the right home once it is not (a
// third replica, or a crawl that grows), and plan §6.1 asked for one.
func (h *WriteAPI) cachedCheck(host string) (map[string]any, bool) {
	h.checkMu.Lock()
	defer h.checkMu.Unlock()
	entry, ok := h.checkCache[host]
	if !ok || time.Since(entry.at) > checkCacheTTL {
		return nil, false
	}
	return entry.body, true
}

func (h *WriteAPI) cacheCheck(host string, body map[string]any) {
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

func (h *WriteAPI) publicWatch(w http.ResponseWriter, r *http.Request) {
	// The anonymous watch is the growth loop (plan §6.2): a host typed on the
	// landing page becomes a real account with a real check, provisioned by
	// e-mail. It creates exactly what the magic link would — same tenant, same
	// project, same key — so the two doors cannot produce different accounts.
	ctx := analytics.WithScope(r.Context(), analytics.ScopeFromRequest(r))
	var req struct {
		Host    string   `json:"host"`
		Email   string   `json:"email"`
		Targets []string `json:"targets"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Host == "" || !strings.Contains(req.Email, "@") {
		writeAPIErr(w, http.StatusBadRequest, "missing_host_or_email")
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

	// The project is named after the site the visitor asked about, so their
	// workspace opens on their own domain. An account that already exists keeps
	// the name it has.
	host := bareHost(req.Host)
	_, tenantID, err := auth.Provision(ctx, h.pool, req.Email, host, h.rec, h.selfHosted)
	if err != nil || tenantID == 0 {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// The watch IS the conversion moment (plan: product-analytics §Decision 5):
	// the event goes to ClickHouse with the host only; the e-mail — the
	// visitor's identity — goes to the Postgres visitor row and nowhere else.
	h.rec.ServerEvent(ctx, "watch_signup", 0, 0, map[string]string{"host": host})
	h.rec.LinkEmail(ctx, req.Email)
	var projectID int64
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT id FROM project WHERE tenant_id = $1 ORDER BY id LIMIT 1`, tenantID).Scan(&projectID)

	// The host itself, then whatever pages were ticked. The client's list is not
	// trusted: every target has to resolve to the same host it asked about, or
	// this endpoint becomes a way to make us watch — and repeatedly request —
	// somebody else's server by posting a URL.
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
	_, _ = h.pool.Raw().Exec(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, kind, target)
		 SELECT gen_random_uuid(), $1, 'email', $2
		  WHERE NOT EXISTS (SELECT 1 FROM alert_channel WHERE tenant_id = $1 AND kind = 'email' AND target = $2)`,
		tenantID, req.Email)

	// The page's public address is the site's name, not our internal id.
	slug := h.claimSlug(ctx, host, projectID)
	if slug == "" {
		slug = "prj-" + strconv.FormatInt(projectID, 10)
	}
	// The page opens under the site's own name rather than an empty title. Its
	// components need no seeding: they ARE the monitors created above, published
	// unless the owner says otherwise, so the page shows exactly what was ticked.
	// DO NOTHING, never an UPDATE: a returning visitor may have retitled it, and
	// a slug they have already handed out must not change under them.
	_, _ = h.pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title)
		 VALUES ($1, $2, $3, $4) ON CONFLICT (slug) DO NOTHING`,
		tenantID, projectID, slug, host)
	// A project that already had a page keeps ITS slug, whatever we just picked.
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT slug FROM status_page WHERE project_id = $1 ORDER BY id LIMIT 1`, projectID).Scan(&slug)

	// A way in. The code is the sign-in door's own — one-time, expiring,
	// attempt-capped. In prod it leaves by e-mail only, through the same mailer
	// the sign-in door uses: handing a login token back to an anonymous caller
	// who typed somebody else's address is account takeover, not onboarding.
	// Dev echoes it so the flow is testable without an inbox, exactly as
	// POST /v1/auth/magic-link does.
	login := map[string]any{}
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

	writeAPIJSON(w, http.StatusOK, map[string]any{
		"statusUrl": "/status/" + slug,
		"slug":      slug,
		"watching":  watching,
		"login":     login,
	})
}

// sameHostTargets keeps only the targets that belong to the host the visitor
// asked about. The landing sends back row ids, which ARE urls, so this is the
// boundary where a client-supplied URL stops being trusted: without it, posting
// {"host":"mine.com","targets":["https://victim.example/heavy"]} would enrol a
// stranger's endpoint into our probe schedule every 5 minutes, forever.
//
// The SSRF guard still runs when the check executes; this is the cheaper,
// earlier refusal, and the one that keeps the schedule clean.
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
		// The host itself or one of its subdomains — an api./app. host is a
		// thing its owner needs watched. Suffix-matched with the dot, so
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

// slugFromHost turns a host into the status page's public address: harpa.ai
// becomes /status/harpa-ai. The slug is the one part of this product a customer
// hands to their own users, so it says whose page it is — "prj-20" says nothing
// and leaks how many accounts exist.
//
// Formatting only. Uniqueness is the caller's job (the column is UNIQUE across
// tenants, so the second harpa.ai has to take another one) — keeping the two
// apart is what makes this testable without a database.
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

// claimSlug returns a free status-page slug for host, or "" to fall back to the
// project id. The suffix is random rather than a counter: a counter tells the
// second person to watch a host that somebody else is already watching it.
func (h *WriteAPI) claimSlug(ctx context.Context, host string, projectID int64) string {
	return claimSlugFor(ctx, h.pool, host, projectID)
}

// claimSlugFor is the pool-level claim both doors share: the watch door claims
// at provisioning, the sign-in door re-claims the moment its project is named
// (audit §13 / D10).
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

// bareHost is what the visitor typed reduced to a domain: it names their project
// and titles their status page, so it must carry no scheme, no path and no port.
// The landing sends whatever was in the field ("https://mysite.io/pricing" is a
// perfectly ordinary paste), and a workspace called "https://mysite.io/pricing"
// is wrong on every screen that shows it.
func bareHost(raw string) string {
	h := strings.TrimSpace(raw)
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	if i := strings.IndexAny(h, "/:?#"); i >= 0 {
		h = h[:i]
	}
	return h
}

// The name comes from the TARGET, not from the address that was typed. Built the
// other way, every subdomain the discovery found — api.harpa.ai, app.harpa.ai —
// was labelled "harpa.ai", and a status page listing three components with one
// name is a list nobody can read. `host` remains the fallback for a target we
// cannot parse.
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

func (h *WriteAPI) publicStatus(w http.ResponseWriter, r *http.Request) {
	// The public page shows the same measured components as the config screen,
	// minus the ones the owner unpublished. Nothing here is typed in by hand.
	ctx := r.Context()
	slug := pathLast(r.URL.Path)
	var tenantID int64
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT tenant_id FROM status_page WHERE slug = $1`, slug).Scan(&tenantID); err != nil {
		// A page nobody has configured yet still resolves by its project slug, so
		// a fresh account can share the link before touching the settings. The
		// parsed id is a PROJECT id and is kept apart from the tenant id: sharing
		// one variable meant an unknown project fell through as a tenant id, and
		// the page then published whichever tenant happened to hold that number.
		var projectID int64
		if _, perr := fmt.Sscanf(slug, "prj-%d", &projectID); perr != nil || projectID == 0 {
			writeAPIErr(w, http.StatusNotFound, "no_such_page")
			return
		}
		if qerr := h.pool.Raw().QueryRow(ctx,
			`SELECT tenant_id FROM project WHERE id = $1`, projectID).Scan(&tenantID); qerr != nil || tenantID == 0 {
			writeAPIErr(w, http.StatusNotFound, "no_such_page")
			return
		}
	}
	cfg := h.statusConfig(ctx, tenantID)
	resp := map[string]any{
		"title":      cfg.Title,
		"components": h.statusComponents(ctx, tenantID, cfg, true),
		"incidents":  []map[string]any{},
		"network":    []map[string]any{},
		"updatedAt":  time.Now().UTC().Format(time.RFC3339),
		"poweredBy":  cfg.ShowPoweredBy,
		// Whether the SECTION is published, which is not the same question as
		// whether it is empty. Sending only the list meant a page with incidents
		// switched off still drew the heading with "No incidents recorded" under
		// it — the owner hid a section and got a section saying nothing is wrong.
		"showIncidents": cfg.ShowIncidents,
	}
	// The owner's switch decides whether the section is published at all; what it
	// then shows is measured, never sample data.
	if cfg.ShowNetwork {
		resp["network"] = h.statusNetwork(ctx, tenantID)
	}
	if cfg.ShowIncidents {
		incidents := []map[string]any{}
		if rows, rerr := h.pool.Raw().Query(ctx,
			// monitor_id IS NOT NULL drops the incidents of DELETED checks: the
			// component is gone from the page, and a chronicle about a service
			// the reader cannot see in the list explains nothing (owner
			// decision, 2026-08-24). Detector incidents are project-scoped and
			// already excluded by the detector filter.
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
	}
	writeAPIJSON(w, http.StatusOK, resp)
}

// --- helpers ---

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
	return time.Now().Unix() // real clock (was a 0 stub for compilation)
}

var _ = context.Background
