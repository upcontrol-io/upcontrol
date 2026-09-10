// Monitor CRUD handlers for the /v1/monitors surface, tenant-scoped and
// mockData-shaped. The entitlement gate (402 + upgrade.reason) is the wall.

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/detect/availability"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/incident/triage"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
	"go.upcontrol.io/back/internal/targetkey"
)

// monitors serves GET/POST /v1/monitors and GET/PATCH/DELETE /v1/monitors/{id}.
type monitors struct {
	pool   *pg.Pool
	pgs    *pgstore.Store
	sess   *session.Manager
	origin string
}

func NewMonitors(p *pg.Pool, pgs *pgstore.Store, sm *session.Manager, origin string) *monitors {
	return &monitors{pool: p, pgs: pgs, sess: sm, origin: strings.TrimRight(origin, "/")}
}

// ServeHTTP routes by method + path pattern.
func (h *monitors) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, err := h.sess.FromRequest(r.Context(), r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	// Notify members read, login members change: creating/editing/deleting a
	// check is a settings act.
	if r.Method != http.MethodGet && !canManage(r.Context(), h.pool, s) {
		writeAPIErr(w, http.StatusForbidden, "notify_role")
		return
	}
	tenantID := s.TenantID
	id := r.PathValue("id")
	switch {
	case r.Method == http.MethodGet && id == "":
		h.list(w, r, currentProjectID(r.Context(), h.pool, s, tenantID))
	case r.Method == http.MethodPost && id == "":
		h.create(w, r, tenantID)
	case r.Method == http.MethodGet && id != "":
		h.notFound(w) // GET single not needed by the front; list covers it
	case r.Method == http.MethodPatch && id != "":
		h.patch(w, r, tenantID, id)
	case r.Method == http.MethodDelete && id != "":
		h.delete(w, r, tenantID, id)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *monitors) list(w http.ResponseWriter, r *http.Request, projectID int64) {
	rows, err := h.pool.Queries().ListMonitorsByProject(r.Context(), projectID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, monitorRowToAPI(row.Kind, row.Name, row.Target,
			ptrStrSafe(row.Keyword), row.IntervalSec, ptrStrSafe(row.Status),
			row.SslExpiresAt, row.DomainExpiresAt, row.PublicID,
			h.pingURL(row.Kind, row.PingToken), row.Paused, row.PausedBy))
	}
	writeAPIJSON(w, http.StatusOK, out)
}

func (h *monitors) create(w http.ResponseWriter, r *http.Request, tenantID int64) {
	var req struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Target   string `json:"target"`
		Keyword  string `json:"keyword"`
		Interval string `json:"interval"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "bad_body")
		return
	}
	kind := strings.ToLower(req.Type)
	// The API is the gate, not the form: a bad request is refused before any
	// count check, so it must not consume one of the plan's slots.
	if code := validateMonitorCreate(kind, req.Target); code != "" {
		writeAPIErr(w, http.StatusBadRequest, code)
		return
	}
	// How often it may run is a plan number too: the entitlement row is the
	// gate, answering 402 with the reason the upgrade prompt shows.
	plan := h.tenantPlan(r.Context(), tenantID)
	if msg := h.intervalRefusal(r.Context(), plan, req.Interval); msg != "" {
		writeUpgradeRequired(w, msg, "")
		return
	}
	// Entitlement gate: count current monitors vs the plan's http_checks limit.
	count, _ := h.pool.Queries().CountMonitorsByTenant(r.Context(), tenantID)
	limit, _ := h.pool.Queries().GetPlanHTTPChecks(r.Context(), plan)
	if limit > 0 && count >= limit {
		writeUpgradeRequired(w, plan+" allows "+strconv.Itoa(int(limit))+" HTTP checks.", "")
		return
	}
	// The monitor lands in the session's current project; the tenant's first
	// project is the fallback (no session row yet, pick unset or stale).
	s, _ := h.sess.FromRequest(r.Context(), r)
	projectID := currentProjectID(r.Context(), h.pool, s, tenantID)
	pubID := newUUID()
	var keyword *string
	if req.Keyword != "" {
		keyword = &req.Keyword
	}
	// A check is a subscription (plan part 1): the fetch lives in probe_target
	// and is SHARED — one URL is checked once for everybody. A heartbeat owns
	// a private target keyed by the monitor's public id, minted here because
	// the key must exist before the monitor row that references it.
	normTarget := req.Target
	var key string
	if kind != "heartbeat" {
		n, nerr := targetkey.NormalizeURL(req.Target)
		if nerr != nil {
			writeAPIErr(w, http.StatusBadRequest, "bad_target")
			return
		}
		normTarget = n
		key = targetkey.Website(normTarget, req.Keyword)
	}
	// A heartbeat's credential is its ping token; the URL built from it is the
	// only thing the customer's job ever holds.
	var pingToken *string
	if kind == "heartbeat" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		t := hex.EncodeToString(b)
		pingToken = &t
	}
	params := sqlc.CreateMonitorParams{
		PublicID: pubID, TenantID: tenantID, ProjectID: projectID,
		Kind: kind, Name: req.Name, Target: normTarget,
		Keyword: keyword, IntervalSec: parseInterval(req.Interval),
		PingToken: pingToken,
	}
	// Target, monitor, schedule and (when the target is already down) the
	// first incident land in ONE transaction: a subscriber who joins an
	// outage gets its incident with the insert, not one check later.
	var row sqlc.CreateMonitorRow
	// Set when the subscription joined a target already down: its incident
	// opened inside the transaction, the evidence slice follows the commit.
	var openedMonitor int64
	err := h.inTx(r.Context(), func(q *sqlc.Queries, tx pgx.Tx) error {
		tkey, tkind, turl := key, kind, normTarget
		if kind == "heartbeat" {
			// Private target, never shared: the key is the monitor's public id
			// (dashed lowercase, the same text migration 009 builds).
			pub := uuid.UUID(pubID.Bytes).String()
			tkey = targetkey.Heartbeat(pub)
			// url is a tokenless stable label, never the ping URL: the fleet
			// never fetches heartbeat targets (the lease filters the kind) and
			// the ping door joins through monitor.ping_token, so a tokened URL
			// here would be a secret copy nobody reads. 009's backfill stores
			// the same label.
			turl = "heartbeat:" + pub
		}
		targetID, terr := q.GetOrCreateProbeTarget(r.Context(), sqlc.GetOrCreateProbeTargetParams{
			Key: tkey, Kind: tkind, Url: turl, Keyword: keyword,
		})
		if terr != nil {
			return terr
		}
		if terr := q.EnsureTargetSchedule(r.Context(), sqlc.EnsureTargetScheduleParams{
			TargetID: targetID, Region: scheduleRegion(),
		}); terr != nil {
			return terr
		}
		var created bool
		var ierr error
		row, created, ierr = insertMonitorOnTarget(r.Context(), tx, params, targetID)
		if ierr != nil {
			return ierr
		}
		if kind == "heartbeat" {
			// Open the first window at 2x the interval (grace defaults to the
			// interval): a job that starts on its next cron tick is not "missed"
			// the minute it is born.
			return q.SetHeartbeatDue(r.Context(), sqlc.SetHeartbeatDueParams{
				Secs: float64(2 * params.IntervalSec), MonitorID: row.ID,
			})
		}
		// A subscription starts now: the first check runs at the next lease,
		// not one interval later.
		if terr := q.PullTargetDue(r.Context(), targetID); terr != nil {
			return terr
		}
		if !created {
			return nil
		}
		// Subscribing onto a target that is already down opens the incident
		// here, in the same transaction as the insert (plan part 1).
		if facts, ferr := q.GetTargetFacts(r.Context(), targetID); ferr == nil &&
			facts.Status == availability.StatusDown {
			title, eff := downTargetTitle(r.Context(), tx, monitorTitleName(row.Name, row.Target), targetID)
			lc := incident.New(h.pool, h.pgs)
			_, created, _ := lc.OpenOnTx(r.Context(), q, row.ID, title, eff)
			if created {
				// The evidence slice waits for the commit (incident_slice's FK
				// cannot see an uncommitted incident); the caller freezes.
				openedMonitor = row.ID
			}
		}
		return nil
	})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if openedMonitor != 0 {
		incident.New(h.pool, h.pgs).FreezeOpenIncident(r.Context(), openedMonitor)
	}
	// The first website check names the project, only when it is still unnamed:
	// a shared status-page link must not be renamed out from under it.
	h.nameProjectIfUnnamed(r.Context(), projectID, params.Kind, row.Target)

	var kw string
	if row.Keyword != nil {
		kw = *row.Keyword
	}
	writeAPIJSON(w, http.StatusCreated, monitorRowToAPI(
		row.Kind, row.Name, row.Target, kw, row.IntervalSec,
		"nodata", // new monitor has no checks yet
		pgtype.Timestamptz{}, pgtype.Timestamptz{}, row.PublicID,
		h.pingURL(row.Kind, row.PingToken), false, nil))
}

// nameProjectIfUnnamed sets project.domain from a website check's target, only
// while it is unset: the WHERE carries the condition, so no race decides twice.
func (h *monitors) nameProjectIfUnnamed(ctx context.Context, projectID int64, kind, target string) {
	if kind != "website" {
		return
	}
	host := bareHost(target)
	if host == "" {
		return
	}
	ct, err := h.pool.Raw().Exec(ctx,
		`UPDATE project SET domain = $2 WHERE id = $1 AND (domain IS NULL OR domain = '')`,
		projectID, host)
	if err != nil || ct.RowsAffected() == 0 {
		// A named project keeps it; only the winner of the naming race
		// re-claims the slug.
		return
	}
	// Naming the project rewrites only the service slug (`prj-3` to
	// `example-com`): a hand-picked slug may be bookmarked; prj-N still resolves.
	service := "prj-" + strconv.FormatInt(projectID, 10)
	claimed := claimSlugFor(ctx, h.pool, host, projectID)
	if claimed == "" || claimed == service {
		return
	}
	ct, err = h.pool.Raw().Exec(ctx,
		`UPDATE status_page SET slug = $2 WHERE project_id = $1 AND slug = $3`,
		projectID, claimed, service)
	if err != nil || ct.RowsAffected() > 0 {
		return
	}
	// No page row to rename: create it under the site's name, as the watch door
	// does. NOT EXISTS keeps a hand-picked page; DO NOTHING rides a slug race.
	_, _ = h.pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title)
		 SELECT tenant_id, id, $2, $3 FROM project
		  WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM status_page WHERE project_id = $1)
		 ON CONFLICT (slug) DO NOTHING`,
		projectID, claimed, host)
}

func (h *monitors) patch(w http.ResponseWriter, r *http.Request, tenantID int64, id string) {
	var req struct {
		Name     *string `json:"name"`
		Target   *string `json:"target"`
		Keyword  *string `json:"keyword"`
		Interval *string `json:"interval"`
		Paused   *bool   `json:"paused"`
	}
	// Strict: unknown fields must not 200 as a silent no-op.
	if !decodeStrict(w, r, &req) {
		return
	}
	// A different fetch is a different check: target and keyword are immutable
	// (plan part 1). Patching either would show one URL and measure another;
	// recreating a check is free (create is idempotent per (project, target)).
	if req.Target != nil || req.Keyword != nil {
		writeAPIErr(w, http.StatusBadRequest, "target_immutable")
		return
	}
	pubID := parseUUID(id)
	ctx := r.Context()
	// A frozen project's monitors are a snapshot: no edits, no pause toggles —
	// nothing changes what an upgrade will restore (docs/plans/trial-and-freeze.md).
	if h.frozenMonitor(w, ctx, tenantID, pubID) {
		return
	}
	// The BEFORE state decides the pulls below: unpausing or lowering the
	// interval starts a check sooner, and unpausing onto a down target opens
	// the incident in the same transaction as the PATCH.
	var targetID int64
	var wasPaused bool
	var oldInterval int32
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT target_id, paused, interval_sec FROM monitor WHERE public_id = $1 AND tenant_id = $2`,
		pubID, tenantID).Scan(&targetID, &wasPaused, &oldInterval); err != nil {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	params := sqlc.PatchMonitorParams{PublicID: pubID, TenantID: tenantID}
	params.Name = req.Name
	if req.Interval != nil {
		// Same floor as create, or the wall is one PATCH away from not existing.
		if msg := h.intervalRefusal(ctx, h.tenantPlan(ctx, tenantID), *req.Interval); msg != "" {
			writeUpgradeRequired(w, msg, "")
			return
		}
		v := parseInterval(*req.Interval)
		params.IntervalSec = &v
	}
	params.Paused = req.Paused
	var row sqlc.PatchMonitorRow
	// Set when unpausing rejoined a target still down: same commit-ordering
	// as the create path — the slice is frozen after the transaction lands.
	var unpausedOntoDown int64
	err := h.inTx(ctx, func(q *sqlc.Queries, tx pgx.Tx) error {
		var perr error
		row, perr = q.PatchMonitor(ctx, params)
		if perr != nil {
			return perr
		}
		newPaused, newInterval := wasPaused, oldInterval
		if req.Paused != nil {
			newPaused = *req.Paused
		}
		if params.IntervalSec != nil {
			newInterval = *params.IntervalSec
		}
		unpaused := wasPaused && !newPaused
		// Unpausing or tightening the interval lowers the target's effective
		// interval: pull the next check to now so it runs at the next lease.
		if unpaused || newInterval < oldInterval {
			if terr := q.PullTargetDue(ctx, targetID); terr != nil {
				return terr
			}
		}
		if unpaused {
			// Unpausing onto a target that is already down opens the incident
			// here, in the same transaction (a paused subscriber that comes back
			// joins the outage, not the next edge).
			if facts, ferr := q.GetTargetFacts(ctx, targetID); ferr == nil &&
				facts.Status == availability.StatusDown {
				title, eff := downTargetTitle(ctx, tx, monitorTitleName(row.Name, row.Target), targetID)
				lc := incident.New(h.pool, h.pgs)
				_, created, _ := lc.OpenOnTx(ctx, q, row.ID, title, eff)
				if created {
					unpausedOntoDown = row.ID
				}
			}
		}
		return nil
	})
	if err != nil {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	if unpausedOntoDown != 0 {
		incident.New(h.pool, h.pgs).FreezeOpenIncident(ctx, unpausedOntoDown)
	}
	// status/ssl/domain expiry live in target_facts, so PatchMonitor's
	// RETURNING cannot reach them: re-read, the same query `list` uses.
	full, err := h.pool.Queries().GetMonitorByPublicID(r.Context(), sqlc.GetMonitorByPublicIDParams{
		PublicID: pubID, TenantID: tenantID,
	})
	if err != nil {
		full = sqlc.GetMonitorByPublicIDRow{
			Kind: row.Kind, Name: row.Name, Target: row.Target,
			Keyword: row.Keyword, IntervalSec: row.IntervalSec, PublicID: row.PublicID,
		}
	}
	var kw string
	if full.Keyword != nil {
		kw = *full.Keyword
	}
	writeAPIJSON(w, http.StatusOK, monitorRowToAPI(
		full.Kind, full.Name, full.Target, kw, full.IntervalSec,
		ptrStrSafe(full.Status), full.SslExpiresAt, full.DomainExpiresAt, full.PublicID,
		h.pingURL(full.Kind, full.PingToken), full.Paused, full.PausedBy))
}

func (h *monitors) delete(w http.ResponseWriter, r *http.Request, tenantID int64, id string) {
	ctx := r.Context()
	pubID := parseUUID(id)
	// Snapshot rule, same as PATCH: deleting from a frozen project would change
	// what an upgrade restores.
	if h.frozenMonitor(w, ctx, tenantID, pubID) {
		return
	}
	// Close an open incident while the monitor id still resolves: monitor_id is
	// ON DELETE SET NULL, and after the DELETE nothing can find the row.
	if mon, err := h.pool.Queries().GetMonitorByPublicID(ctx, sqlc.GetMonitorByPublicIDParams{
		PublicID: pubID, TenantID: tenantID,
	}); err == nil {
		if cerr := incident.New(h.pool, nil).Close(ctx, mon.ID, incident.ReasonMonitorDelete); cerr != nil {
			// A failed tidy-up fails the delete: nothing afterwards can find the row.
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	err := h.pool.Queries().DeleteMonitor(ctx, sqlc.DeleteMonitorParams{
		PublicID: pubID, TenantID: tenantID,
	})
	if err != nil {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *monitors) notFound(w http.ResponseWriter) {
	writeAPIErr(w, http.StatusNotFound, "not_found")
}

// inTx runs fn on Queries and the raw transaction bound together; commits on
// nil, rolls back otherwise. The tx is fn's to reach for the statements that
// predate sqlc (the idempotent monitor insert, title evidence reads).
func (h *monitors) inTx(ctx context.Context, fn func(*sqlc.Queries, pgx.Tx) error) error {
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

// insertMonitorOnTarget is the idempotent subscription insert: one project,
// one fetch (UNIQUE (project_id, target_id)). A second create of the same
// (project, target) is the same subscription — the existing row comes back
// as the answer, so recreating a check is free (plan part 1).
func insertMonitorOnTarget(ctx context.Context, tx pgx.Tx, params sqlc.CreateMonitorParams, targetID int64) (sqlc.CreateMonitorRow, bool, error) {
	var row sqlc.CreateMonitorRow
	scan := func(r pgx.Row) error {
		return r.Scan(&row.ID, &row.PublicID, &row.Kind, &row.Name, &row.Target,
			&row.Keyword, &row.IntervalSec, &row.PingToken, &row.CreatedAt)
	}
	err := scan(tx.QueryRow(ctx, `
		INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, keyword, interval_sec, ping_token, target_id)
		VALUES (COALESCE($1::uuid, gen_random_uuid()), $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (project_id, target_id) DO NOTHING
		RETURNING id, public_id, kind, name, target, keyword, interval_sec, ping_token, created_at`,
		params.PublicID, params.TenantID, params.ProjectID, params.Kind, params.Name,
		params.Target, params.Keyword, params.IntervalSec, params.PingToken, targetID))
	if err == nil {
		return row, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return row, false, err
	}
	// The conflict arm: answer the existing subscription, not an error.
	err = scan(tx.QueryRow(ctx, `
		SELECT id, public_id, kind, name, target, keyword, interval_sec, ping_token, created_at
		  FROM monitor WHERE project_id = $1 AND target_id = $2`,
		params.ProjectID, targetID))
	return row, false, err
}

// monitorTitleName is the probe path's rule: the name the incident title
// carries, falling back to the target when the row was left unnamed.
func monitorTitleName(name, target string) string {
	if name == "" {
		return target
	}
	return name
}

// downTargetTitle builds the incident title for a subscription joining a
// target that is already down: the same triage the probe path applies, read
// from the target's newest measurement (there is no fresh result in hand —
// the outage was measured before this subscriber arrived). The effective
// interval of that newest row is returned with it (0 when there is none).
func downTargetTitle(ctx context.Context, tx pgx.Tx, monitorName string, targetID int64) (string, int32) {
	var errClass string
	var statusCode *int
	var intervalSec int32
	err := tx.QueryRow(ctx,
		`SELECT error_class, status_code, interval_sec FROM checks
		  WHERE target_id = $1 ORDER BY ts DESC LIMIT 1`, targetID).
		Scan(&errClass, &statusCode, &intervalSec)
	code := 0
	if err != nil || statusCode == nil {
		intervalSec = 0
	} else {
		code = *statusCode
	}
	return triage.Build(monitorName, errClass, code).Title, intervalSec
}

// scheduleRegion reads UC_NODE_REGION, the SAME env var ucprobe leases with, so
// a later region filter on the lease query cannot silently break scheduling.
func scheduleRegion() string {
	if r := os.Getenv("UC_NODE_REGION"); r != "" {
		return r
	}
	return "default"
}

// pingURL builds the heartbeat's ping URL; "" for every other monitor and for
// a heartbeat whose token never landed.
func (h *monitors) pingURL(kind string, token *string) string {
	if kind != "heartbeat" || token == nil || *token == "" {
		return ""
	}
	return h.origin + "/public/ping/" + *token
}

// monitorRowToAPI builds the front-facing Monitor shape: interval as display
// string, expiry dates omitted when no facts exist yet.
// pausedBy: 'plan' marks the budget sweeper's pause (docs/plans/trial-and-
// freeze.md) — the row's card words it as the plan's wall, not the owner's
// choice; NULL is the owner's own pause.
func monitorRowToAPI(kind, name, target, keyword string, intervalSec int32,
	status string, sslExp, domainExp pgtype.Timestamptz,
	pubID pgtype.UUID, pingURL string, paused bool, pausedBy *string) map[string]any {

	m := map[string]any{
		"id":       uuidStr(pubID),
		"type":     monitorTypeLabel(kind),
		"name":     name,
		"target":   target,
		"status":   monitorStatusLabel(status),
		"interval": intervalLabel(intervalSec),
		"paused":   paused,
	}
	if pausedBy != nil {
		m["pausedBy"] = *pausedBy
	}
	if keyword != "" {
		m["keyword"] = keyword
	}
	if pingURL != "" {
		m["pingUrl"] = pingURL
	}
	if kind == "website" && (sslExp.Valid || domainExp.Valid) {
		// Only the half we have a date for: "domain —" says we have not looked,
		// so an absent field does not render at all.
		exp := map[string]string{}
		if sslExp.Valid {
			exp["ssl"] = formatExpiry(sslExp, "SSL")
		}
		if domainExp.Valid {
			exp["domain"] = formatExpiry(domainExp, "domain")
		}
		m["expiry"] = exp
	}
	return m
}

func monitorTypeLabel(kind string) string {
	switch kind {
	case "heartbeat":
		return "Heartbeat"
	default:
		return "Website"
	}
}

func monitorStatusLabel(status string) string {
	if status == "" {
		return "nodata"
	}
	return status
}

func intervalLabel(sec int32) string {
	switch sec {
	case 60:
		return "1m"
	case 300:
		return "5m"
	case 1800:
		return "30m"
	case 3600:
		return "1h"
	default:
		return "5m"
	}
}

// tenantPlan reads the tenant's plan, falling back to Free. The gates and
// GET /v1/plan must read the same row.
func (h *monitors) tenantPlan(ctx context.Context, tenantID int64) string {
	var plan string
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT plan FROM tenant WHERE id = $1`, tenantID).Scan(&plan)
	if plan == "" {
		return "Free"
	}
	return plan
}

// intervalRefusal returns why this plan may not run that often, or "". Empty
// input is "not asked for": the column's default applies.
func (h *monitors) intervalRefusal(ctx context.Context, plan, interval string) string {
	if interval == "" {
		return ""
	}
	ent, err := h.pool.Queries().GetPlanEntitlement(ctx, plan)
	if err != nil || ent.MinIntervalSec <= 0 {
		return ""
	}
	if parseInterval(interval) >= ent.MinIntervalSec {
		return ""
	}
	return plan + " checks a site every " + intervalLabel(ent.MinIntervalSec) +
		". A paid plan checks every minute."
}

func parseInterval(s string) int32 {
	switch s {
	case "1m":
		return 60
	case "5m":
		return 300
	case "30m":
		return 1800
	case "1h":
		return 3600
	default:
		return 300
	}
}

func formatExpiry(t pgtype.Timestamptz, prefix string) string {
	if !t.Valid {
		return prefix + " —"
	}
	return prefix + " " + t.Time.Format("Jan 2")
}

func ptrStrSafe(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// validateMonitorCreate returns the refusal code for a create, or "". A
// heartbeat has no target of its own (we generate its ping URL).
func validateMonitorCreate(kind, target string) string {
	if kind == "heartbeat" {
		return ""
	}
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return "missing_target"
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "bad_target"
	}
	return ""
}

// writeUpgradeRequired is the one shape a paid wall may take: 402 carrying
// `upgrade.reason` — and, when a cheaper plan lifts this wall, `upgrade.plan`,
// which the client routes into its upgrade prompt. No plan field at the top
// of the ladder: the front shows the message instead of the modal.
// frozenMonitor answers whether the named monitor lives in a frozen project
// and, when it does, writes the 402 itself.
func (h *monitors) frozenMonitor(w http.ResponseWriter, ctx context.Context, tenantID int64, pubID pgtype.UUID) bool {
	var frozen bool
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM monitor m JOIN project p ON p.id = m.project_id
		  WHERE m.public_id = $1 AND m.tenant_id = $2 AND p.frozen_at IS NOT NULL)`,
		pubID, tenantID).Scan(&frozen)
	if !frozen {
		return false
	}
	count, _ := h.pool.Queries().CountProjectsByTenant(ctx, tenantID)
	writeUpgradeRequired(w,
		"This project is frozen. Reactivate it by upgrading your plan.",
		upgradePlanForProjects(ctx, h.pool, count))
	return true
}

func writeUpgradeRequired(w http.ResponseWriter, reason, plan string) {
	upgrade := map[string]string{"reason": reason}
	if plan != "" {
		upgrade["plan"] = plan
	}
	writeAPIJSON(w, http.StatusPaymentRequired, map[string]any{
		"error": map[string]any{
			"code":    "plan_limit_exceeded",
			"message": reason,
			"upgrade": upgrade,
		},
	})
}

func writeAPIErr(w http.ResponseWriter, code int, msg string) {
	writeAPIJSON(w, code, map[string]any{
		"error": map[string]string{"code": msg},
	})
}

// writeAPIErrMsg is writeAPIErr with the human message the Error schema marks
// required beside code.
func writeAPIErrMsg(w http.ResponseWriter, code int, errCode, msg string) {
	writeAPIJSON(w, code, map[string]any{
		"error": map[string]string{"code": errCode, "message": msg},
	})
}

func writeAPIJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// parseUUID converts a hex-string public_id to pgtype.UUID: uuidStr writes
// lowercase hex without dashes, and the front sends that back.
func parseUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	if len(s) != 32 || strings.ToLower(s) != s {
		return u
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return u
	}
	copy(u.Bytes[:], b)
	u.Valid = true
	return u
}
