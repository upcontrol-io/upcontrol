// Monitor CRUD handlers for the /v1/monitors surface. Each handler reads the
// session cookie → tenant_id, scopes every query to that tenant (invariant 3),
// and returns the Monitor shape the front's mockData expects.
//
// The entitlement gate (POST → 402 with upgrade.reason) is the ONLY paid wall
// in this surface: the plan's http_checks limit (3 on Free). A 4th monitor on
// Free is rejected with the exact upgrade shape the client reads.

package api

import (
	"context"
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
	"go.upcontrol.io/back/internal/storage/pg"
)

// Monitors serves GET/POST /v1/monitors and GET/PATCH/DELETE /v1/monitors/{id}.
type Monitors struct {
	pool *pg.Pool
	sess *session.Manager
}

func NewMonitors(p *pg.Pool, sm *session.Manager) *Monitors {
	return &Monitors{pool: p, sess: sm}
}

// ServeHTTP routes by method + path pattern.
func (h *Monitors) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, err := h.sess.FromRequest(r.Context(), r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	// §7.4: notify members read, login members change. Checks are the product's
	// own surface, so creating/editing/deleting one is a settings act.
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

func (h *Monitors) list(w http.ResponseWriter, r *http.Request, tenantID int64) {
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

func (h *Monitors) create(w http.ResponseWriter, r *http.Request, tenantID int64) {
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
	// The API is the gate, not the form: the client's own length check is a
	// courtesy, and without this an empty target produced a real monitor row
	// that consumed one of the plan's three HTTP-check slots and handed the
	// probe fleet an empty string to schedule. Refused before any rate or
	// count check: a bad request must not consume a gate.
	if code := validateMonitorCreate(kind, req.Target); code != "" {
		writeAPIErr(w, http.StatusBadRequest, code)
		return
	}
	// How often it may run is a plan number too (Aug 14, 2026): Free runs every
	// 5 minutes. The client's own picker is a courtesy — the entitlement row is
	// the gate, and it answers 402 with the reason the upgrade prompt shows.
	plan := h.tenantPlan(r.Context(), tenantID)
	if msg := h.intervalRefusal(r.Context(), plan, req.Interval); msg != "" {
		writeUpgradeRequired(w, msg)
		return
	}
	// Entitlement gate: count current monitors vs the plan's http_checks limit.
	count, _ := h.pool.Queries().CountMonitorsByTenant(r.Context(), tenantID)
	limit, _ := h.pool.Queries().GetPlanHTTPChecks(r.Context(), plan)
	if limit > 0 && count >= limit {
		writeUpgradeRequired(w, plan+" allows "+strconv.Itoa(int(limit))+" HTTP checks.")
		return
	}
	// Get the first project for this tenant (the MVP's one-project model).
	var projectID int64
	_ = h.pool.Raw().QueryRow(r.Context(),
		`SELECT id FROM project WHERE tenant_id = $1 ORDER BY id LIMIT 1`, tenantID).Scan(&projectID)
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
	// Create the monitor AND seed its schedule row in one transaction, so a
	// crash between the two can never leave a monitor the probe fleet can't
	// see (block 2: without EnsureMonitorSchedule the monitor never leases).
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
	// The first website check names the project, if nothing has named it yet. The
	// sign-in door creates the account with no domain (it only ever saw an email
	// address), so this is where a workspace stops being anonymous — and the host
	// somebody chose to watch first is the best answer available.
	//
	// Deliberately only when empty: a project that already carries a name is one
	// somebody may have shared a status-page link to, and adding a second check
	// must never rename it out from under that link. Heartbeats are skipped —
	// their target is a ping URL we generated, not a site.
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

// nameProjectIfUnnamed sets project.domain from a website check's target, but
// only while the project has no domain at all. The UPDATE carries that condition
// in its WHERE rather than in a read-then-write, so two checks created at once
// cannot both decide they are the first one.
func (h *Monitors) nameProjectIfUnnamed(ctx context.Context, projectID int64, kind, target string) {
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
		// A project that already carries a name keeps it — and only the winner of
		// the naming race re-claims the slug, which is what serializes two checks
		// created at once.
		return
	}
	// The moment the project is named is the moment the page it hands out stops
	// being service debris (audit §13 / D10): `prj-3` becomes `example-com`.
	// Only the service slug is rewritten — a hand-picked one may already be in
	// somebody's bookmark, and the old `prj-N` link keeps working either way,
	// because publicStatus resolves it through the project-id fallback.
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
	// No page row to rename: the sign-in door creates none, so the account that
	// signed in first and added a check second was left handing out prj-N while
	// the claimed slug went nowhere. Create the row under the site's name, the
	// same INSERT the watch door does at provisioning. The NOT EXISTS guard
	// keeps a hand-picked page intact, and DO NOTHING on a slug race just means
	// the prj-N fallback keeps working.
	_, _ = h.pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title)
		 SELECT tenant_id, id, $2, $3 FROM project
		  WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM status_page WHERE project_id = $1)
		 ON CONFLICT (slug) DO NOTHING`,
		projectID, claimed, host)
}

func (h *Monitors) patch(w http.ResponseWriter, r *http.Request, tenantID int64, id string) {
	var req struct {
		Name     *string `json:"name"`
		Target   *string `json:"target"`
		Keyword  *string `json:"keyword"`
		Interval *string `json:"interval"`
		Paused   *bool   `json:"paused"`
	}
	// Strict (audit §14): `{"url": …}` used to 200 as a silent no-op.
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
			writeUpgradeRequired(w, msg)
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
	// The row as it now is. status/ssl/domain expiry live in monitor_facts, not
	// monitor, so PatchMonitor's RETURNING cannot reach them — a hardcoded
	// "nodata" here greyed a healthy check out on every rename until something
	// else re-read the list. The re-read is the same query `list` uses.
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

func (h *Monitors) delete(w http.ResponseWriter, r *http.Request, tenantID int64, id string) {
	pubID := parseUUID(id)
	err := h.pool.Queries().DeleteMonitor(r.Context(), sqlc.DeleteMonitorParams{
		PublicID: pubID, TenantID: tenantID,
	})
	if err != nil {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Monitors) notFound(w http.ResponseWriter) {
	writeAPIErr(w, http.StatusNotFound, "not_found")
}

// inTx runs fn against sqlc Queries bound to a fresh transaction; the tx commits
// when fn returns nil and rolls back otherwise. Used so monitor create + its
// schedule row land atomically.
func (h *Monitors) inTx(ctx context.Context, fn func(*sqlc.Queries) error) error {
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

// scheduleRegion is the region monitors are leased in. It reads UC_NODE_REGION —
// the SAME env var ucprobe leases with (default "default") — so the value the
// API schedules in always matches the region the probe fleet leases from. The
// lease query does not yet filter by region, but keeping these in step means
// adding that filter later cannot silently break scheduling.
func scheduleRegion() string {
	if r := os.Getenv("UC_NODE_REGION"); r != "" {
		return r
	}
	return "default"
}

// --- helpers ---

// monitorRowToAPI builds the front-facing Monitor shape. The interval is
// converted from seconds to the display string ("5m"). Expiry dates are
// formatted to the mockData convention (omitted for heartbeat monitors or when
// no facts exist yet).
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
		// Only the half we have a date for: "domain —" is not a fact about the
		// domain, it is us saying we have not looked yet, and the row printed it
		// under every check whose certificate we had already read. Zero is
		// silence — an absent field does not render.
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
// GET /v1/plan must read the same row: the gates used to hardcode "Free", so
// the picker promised a paid tenant the minute its POST then refused.
func (h *Monitors) tenantPlan(ctx context.Context, tenantID int64) string {
	var plan string
	_ = h.pool.Raw().QueryRow(ctx,
		`SELECT plan FROM tenant WHERE id = $1`, tenantID).Scan(&plan)
	if plan == "" {
		return "Free"
	}
	return plan
}

// intervalRefusal returns the reason this plan may not run a check that often,
// or "" when it may. Empty input means "not asked for", which is not a refusal:
// the column's default applies.
func (h *Monitors) intervalRefusal(ctx context.Context, plan, interval string) string {
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

// validateMonitorCreate returns the error code for a create that must be
// refused, or "" when the request is fine.
//
// A heartbeat has no target of its own — we generate its ping URL — so it is
// judged only on the kind.
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

// writeAPIErr / writeAPIJSON are shared by all API handlers.
// writeUpgradeRequired is the one shape a paid wall may take: 402 carrying
// `upgrade.reason`, which the client routes straight into its upgrade prompt.
// There is no other paid-wall path — no `disabled`, no muted text.
func writeUpgradeRequired(w http.ResponseWriter, reason string) {
	writeAPIJSON(w, http.StatusPaymentRequired, map[string]any{
		"error": map[string]any{
			"code":    "plan_limit_exceeded",
			"message": reason,
			"upgrade": map[string]string{"reason": reason},
		},
	})
}

func writeAPIErr(w http.ResponseWriter, code int, msg string) {
	writeAPIJSON(w, code, map[string]any{
		"error": map[string]string{"code": msg},
	})
}

// writeAPIErrMsg is writeAPIErr with the human message the Error schema marks
// required beside code — for the responses whose message the front shows as
// the failure copy (the explain 400/429). writeAPIErr stays for the code-only
// replies.
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

// parseUUID converts a hex-string public_id to pgtype.UUID (the front sends
// "0123456789abcdef..." — our uuidStr uses %x on [16]byte so it's lowercase hex
// without dashes).
func parseUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	if len(s) != 32 {
		return u
	}
	b := make([]byte, 16)
	for i := 0; i < 16; i++ {
		var byteVal byte
		for j := 0; j < 2; j++ {
			c := s[i*2+j]
			var d byte
			switch {
			case c >= '0' && c <= '9':
				d = c - '0'
			case c >= 'a' && c <= 'f':
				d = c - 'a' + 10
			case c >= 'A' && c <= 'F':
				d = c - 'A' + 10
			default:
				return u
			}
			byteVal = byteVal<<4 | d
		}
		b[i] = byteVal
	}
	copy(u.Bytes[:], b)
	u.Valid = true
	return u
}
