// Monitor CRUD handlers for the /v1/monitors surface, tenant-scoped and
// mockData-shaped. The entitlement gate (402 + upgrade.reason) is the wall.

package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/storage/pg"
)

// monitors serves GET/POST /v1/monitors and GET/PATCH/DELETE /v1/monitors/{id}.
type monitors struct {
	pool *pg.Pool
	sess *session.Manager
}

func NewMonitors(p *pg.Pool, sm *session.Manager) *monitors {
	return &monitors{pool: p, sess: sm}
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
	if r.Method != http.MethodGet && !roleAtLeastLogin(r.Context(), h.pool, s.PersonID, s.TenantID) {
		writeAPIErr(w, http.StatusForbidden, "notify_role")
		return
	}
	tenantID := s.TenantID
	id := r.PathValue("id")
	switch {
	case r.Method == http.MethodGet && id == "":
		h.list(w, r, tenantID)
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

func (h *monitors) list(w http.ResponseWriter, r *http.Request, tenantID int64) {
	rows, err := h.pool.Queries().ListMonitorsByTenant(r.Context(), tenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, monitorRowToAPI(row.Kind, row.Name, row.Target,
			ptrStrSafe(row.Keyword), row.IntervalSec, ptrStrSafe(row.Status),
			row.SslExpiresAt, row.DomainExpiresAt, row.PublicID))
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
	params := sqlc.CreateMonitorParams{
		PublicID: pubID, TenantID: tenantID, ProjectID: projectID,
		Kind: kind, Name: req.Name, Target: req.Target,
		Keyword: keyword, IntervalSec: parseInterval(req.Interval),
	}
	// Create the monitor AND seed its schedule row in one transaction: without
	// EnsureMonitorSchedule the probe fleet never leases the monitor.
	var row sqlc.CreateMonitorRow
	err := h.inTx(r.Context(), func(q *sqlc.Queries) error {
		var cerr error
		row, cerr = q.CreateMonitor(r.Context(), params)
		if cerr != nil {
			return cerr
		}
		return q.EnsureMonitorSchedule(r.Context(), sqlc.EnsureMonitorScheduleParams{
			MonitorID: row.ID,
			Region:    scheduleRegion(),
		})
	})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
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
		pgtype.Timestamptz{}, pgtype.Timestamptz{}, row.PublicID))
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
	pubID := parseUUID(id)
	params := sqlc.PatchMonitorParams{PublicID: pubID, TenantID: tenantID}
	params.Name = req.Name
	params.Target = req.Target
	params.Keyword = req.Keyword
	if req.Interval != nil {
		// Same floor as create, or the wall is one PATCH away from not existing.
		if msg := h.intervalRefusal(r.Context(), h.tenantPlan(r.Context(), tenantID), *req.Interval); msg != "" {
			writeUpgradeRequired(w, msg, "")
			return
		}
		v := parseInterval(*req.Interval)
		params.IntervalSec = &v
	}
	params.Paused = req.Paused
	row, err := h.pool.Queries().PatchMonitor(r.Context(), params)
	if err != nil {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	// status/ssl/domain expiry live in monitor_facts, so PatchMonitor's
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
		ptrStrSafe(full.Status), full.SslExpiresAt, full.DomainExpiresAt, full.PublicID))
}

func (h *monitors) delete(w http.ResponseWriter, r *http.Request, tenantID int64, id string) {
	ctx := r.Context()
	pubID := parseUUID(id)
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

// inTx runs fn on Queries bound to a fresh transaction; commits on nil, rolls
// back otherwise. Used so monitor create + schedule row land atomically.
func (h *monitors) inTx(ctx context.Context, fn func(*sqlc.Queries) error) error {
	tx, err := h.pool.Raw().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(h.pool.Queries().WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// scheduleRegion reads UC_NODE_REGION, the SAME env var ucprobe leases with, so
// a later region filter on the lease query cannot silently break scheduling.
func scheduleRegion() string {
	if r := os.Getenv("UC_NODE_REGION"); r != "" {
		return r
	}
	return "default"
}

// monitorRowToAPI builds the front-facing Monitor shape: interval as display
// string, expiry dates omitted when no facts exist yet.
func monitorRowToAPI(kind, name, target, keyword string, intervalSec int32,
	status string, sslExp, domainExp pgtype.Timestamptz,
	pubID pgtype.UUID) map[string]any {

	m := map[string]any{
		"id":       uuidStr(pubID),
		"type":     monitorTypeLabel(kind),
		"name":     name,
		"target":   target,
		"status":   monitorStatusLabel(status),
		"interval": intervalLabel(intervalSec),
	}
	if keyword != "" {
		m["keyword"] = keyword
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
// required beside code (the explain 400/429).
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
