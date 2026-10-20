// Product alert rules: the CRUD behind /v1/product-alerts. The evaluator that
// fires them lives in internal/productalert; this file only keeps the rules
// and gates how many a workspace may hold.

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/storage/pg"
)

// productAlertRule is what a person sets; the episode (state, timestamps) is
// the worker's and never written through these doors.
type productAlertRule struct {
	Name        string
	Event       string
	Labels      map[string]string
	Measure     string
	Currency    string
	Kind        string
	Direction   string
	Percent     int
	WindowHours int
	WidgetID    string
	Paused      bool
}

// productAlertWrite is the body of both writes: a nil field is one the
// request leaves alone, so a PATCH merges and a create starts from zero.
// A nil Labels map is absent; `{}` clears them.
type productAlertWrite struct {
	Name        *string           `json:"name"`
	Event       *string           `json:"event"`
	Labels      map[string]string `json:"labels"`
	Measure     *string           `json:"measure"`
	Currency    *string           `json:"currency"`
	Kind        *string           `json:"kind"`
	Direction   *string           `json:"direction"`
	Percent     *int              `json:"percent"`
	WindowHours *int              `json:"windowHours"`
	WidgetID    *string           `json:"widgetId"`
	Paused      *bool             `json:"paused"`
}

func (w productAlertWrite) mergeInto(r productAlertRule) productAlertRule {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&r.Name, w.Name)
	set(&r.Event, w.Event)
	set(&r.Measure, w.Measure)
	set(&r.Currency, w.Currency)
	set(&r.Kind, w.Kind)
	set(&r.Direction, w.Direction)
	set(&r.WidgetID, w.WidgetID)
	if w.Labels != nil {
		r.Labels = w.Labels
	}
	if w.Percent != nil {
		r.Percent = *w.Percent
	}
	if w.WindowHours != nil {
		r.WindowHours = *w.WindowHours
	}
	if w.Paused != nil {
		r.Paused = *w.Paused
	}
	return r
}

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

// validateProductAlert returns "" for a rule that may be stored, else the
// message naming the field. It runs on the MERGED rule, so a PATCH cannot
// leave a pair (measure/currency, change/direction) half set.
func validateProductAlert(r productAlertRule) string {
	switch {
	case r.Event == "" || len(r.Event) > 40 || hasControl(r.Event):
		return "event is 1 to 40 bytes with no control characters"
	// The SDK's own events are not the reader's product, except a page view:
	// a traffic drop is worth an alert.
	case strings.HasPrefix(r.Event, "uc.") && r.Event != "uc.pageview":
		return "event may not start with uc. (uc.pageview is the one allowed)"
	case len(r.Name) > 80 || hasControl(r.Name):
		return "name is at most 80 bytes with no control characters"
	case len(r.WidgetID) > 64 || hasControl(r.WidgetID):
		return "widgetId is at most 64 bytes with no control characters"
	}
	if msg := labelsMsg(r.Labels, "labels"); msg != "" {
		return msg
	}
	switch r.Measure {
	case "":
		if r.Currency != "" {
			return "currency needs measure amount"
		}
	case "amount":
		if !currencyCode.MatchString(r.Currency) {
			return "currency is a three-letter code like USD, required with measure amount"
		}
	default:
		return "measure is amount or absent"
	}
	switch r.WindowHours {
	case 1, 3, 6, 12, 24:
	default:
		return "windowHours is one of 1, 3, 6, 12 or 24"
	}
	validDirection := r.Direction == "down" || r.Direction == "up" || r.Direction == "any"
	validPercent := r.Percent >= 10 && r.Percent <= 1000
	switch r.Kind {
	case "change":
		if !validDirection {
			return "direction is down, up or any, required with kind change"
		}
		if !validPercent {
			return "percent is 10 to 1000, required with kind change"
		}
	case "silence":
		// Unused by a silence rule, but a value sent must still be one the
		// rule could switch back to change with.
		if r.Direction != "" && !validDirection {
			return "direction is down, up or any"
		}
		if r.Percent != 0 && !validPercent {
			return "percent is 10 to 1000"
		}
	default:
		return "kind is change or silence"
	}
	return ""
}

// episodeChanged: anything but the name changes what the rule measures, so
// the episode it was in no longer describes it. A pause closes it too: a
// firing rule resumed later must not send a stale "back to normal".
func episodeChanged(a, b productAlertRule) bool {
	return a.Paused != b.Paused || a.Event != b.Event || !maps.Equal(a.Labels, b.Labels) || a.Measure != b.Measure ||
		a.Currency != b.Currency || a.Kind != b.Kind || a.Direction != b.Direction ||
		a.Percent != b.Percent || a.WindowHours != b.WindowHours || a.WidgetID != b.WidgetID
}

// productAlertLimit answers how many rules the workspace may hold, nil
// unlimited; a missing plan reads as Free, like boardPlan.
func productAlertLimit(ctx context.Context, pool *pg.Pool, tenantID int64) (*int, error) {
	plan, _ := pool.Queries().GetTenantPlan(ctx, tenantID)
	if plan == "" {
		plan = "Free"
	}
	ent, err := pool.Queries().GetPlanEntitlement(ctx, plan)
	if err != nil || ent.ProductAlerts == nil {
		return nil, err
	}
	limit := int(*ent.ProductAlerts)
	return &limit, nil
}

// productAlertsReason is the create wall's copy. `lift` is the cheapest plan
// with room for one more, liftMax its count (nil: no limit), "" when the
// ladder holds nothing above the caller.
func productAlertsReason(carries int, lift string, liftMax *int) string {
	noun := " product alerts."
	if carries == 1 {
		noun = " product alert."
	}
	reason := "Your plan carries " + strconv.Itoa(carries) + noun
	switch {
	case lift == "":
		return reason
	case liftMax == nil:
		return reason + " " + lift + " carries them without a limit."
	default:
		return reason + " " + lift + " carries " + strconv.Itoa(*liftMax) + "."
	}
}

func writeProductAlertWall(ctx context.Context, w http.ResponseWriter, pool *pg.Pool, carries, have int) {
	var liftMax *int
	lift := cheapestPlan(ctx, pool, func(e sqlc.PlanEntitlement) bool {
		if e.ProductAlerts != nil && int(*e.ProductAlerts) <= have {
			return false
		}
		if e.ProductAlerts != nil {
			n := int(*e.ProductAlerts)
			liftMax = &n
		}
		return true
	})
	writeUpgradeRequired(w, productAlertsReason(carries, lift, liftMax), strings.ToLower(lift))
}

type storedProductAlert struct {
	productAlertRule
	publicID    pgtype.UUID
	state       string
	lastFired   pgtype.Timestamptz
	lastChecked pgtype.Timestamptz
	created     pgtype.Timestamptz
	rank        int64
}

// productAlertSelect ranks every rule of the WORKSPACE, oldest first, before
// filtering to the project: the freeze is the workspace's count, computed on
// read. $1 tenant, $2 project, $3 a public id or NULL for all.
const productAlertSelect = `
SELECT a.public_id, a.name, a.event, a.labels, a.measure, a.currency, a.kind,
       a.direction, a.percent, a.window_hours, a.widget_id, a.paused, a.state,
       a.last_fired_at, a.last_checked_at, a.created_at, r.rank
  FROM product_alert a
  JOIN (SELECT id, row_number() OVER (ORDER BY created_at, id) AS rank
          FROM product_alert WHERE tenant_id = $1) r ON r.id = a.id
 WHERE a.tenant_id = $1 AND a.project_id = $2 AND ($3::uuid IS NULL OR a.public_id = $3)
 ORDER BY a.created_at, a.id`

func (h *writeAPI) readProductAlerts(ctx context.Context, tenantID, projectID int64, id pgtype.UUID) ([]storedProductAlert, error) {
	rows, err := h.pool.Raw().Query(ctx, productAlertSelect, tenantID, projectID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []storedProductAlert{}
	for rows.Next() {
		var a storedProductAlert
		var labels []byte
		if err := rows.Scan(&a.publicID, &a.Name, &a.Event, &labels, &a.Measure, &a.Currency, &a.Kind,
			&a.Direction, &a.Percent, &a.WindowHours, &a.WidgetID, &a.Paused, &a.state,
			&a.lastFired, &a.lastChecked, &a.created, &a.rank); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(labels, &a.Labels); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func productAlertJSON(a storedProductAlert, limit *int) map[string]any {
	state := a.state
	switch {
	case a.Paused:
		state = "paused"
	case limit != nil && a.rank > int64(*limit):
		state = "frozen"
	}
	out := map[string]any{
		"id":          uuidStr(a.publicID),
		"name":        a.Name,
		"event":       a.Event,
		"kind":        a.Kind,
		"windowHours": a.WindowHours,
		"paused":      a.Paused,
		"state":       state,
		"createdAt":   a.created.Time.UTC().Format(time.RFC3339),
	}
	if a.WidgetID != "" {
		out["widgetId"] = a.WidgetID
	}
	if len(a.Labels) > 0 {
		out["labels"] = a.Labels
	}
	if a.Measure != "" {
		out["measure"] = a.Measure
		out["currency"] = a.Currency
	}
	if a.Kind == "change" {
		out["direction"] = a.Direction
		out["percent"] = a.Percent
	}
	if a.lastFired.Valid {
		out["lastFiredAt"] = a.lastFired.Time.UTC().Format(time.RFC3339)
	}
	if a.lastChecked.Valid {
		out["lastCheckedAt"] = a.lastChecked.Time.UTC().Format(time.RFC3339)
	}
	return out
}

// answerProductAlert reads one rule back and writes it with code; a rule gone
// in between is the 404.
func (h *writeAPI) answerProductAlert(w http.ResponseWriter, ctx context.Context, tenantID, projectID int64, id pgtype.UUID, code int) {
	limit, err := productAlertLimit(ctx, h.pool, tenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	rows, err := h.readProductAlerts(ctx, tenantID, projectID, id)
	switch {
	case err != nil:
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
	case len(rows) == 0:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	default:
		writeAPIJSON(w, code, productAlertJSON(rows[0], limit))
	}
}

func productAlertPathID(path string) pgtype.UUID {
	return parseUUID(strings.TrimPrefix(path, "/v1/product-alerts/"))
}

// GET /v1/product-alerts — the current project's rules, oldest first.
func (h *writeAPI) listProductAlerts(w http.ResponseWriter, r *http.Request, s sqlc.Session) {
	ctx := r.Context()
	projectID := currentProjectID(ctx, h.pool, s, s.TenantID)
	limit, err := productAlertLimit(ctx, h.pool, s.TenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	rows, err := h.readProductAlerts(ctx, s.TenantID, projectID, pgtype.UUID{})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, productAlertJSON(a, limit))
	}
	writeAPIJSON(w, http.StatusOK, out)
}

// POST /v1/product-alerts — add a rule, gated by the workspace's count.
func (h *writeAPI) createProductAlert(w http.ResponseWriter, r *http.Request, s sqlc.Session) {
	var req productAlertWrite
	if !decodeStrict(w, r, &req) {
		return
	}
	rule := req.mergeInto(productAlertRule{})
	if msg := validateProductAlert(rule); msg != "" {
		writeAPIErrMsg(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	// A silence rule stores the columns' own defaults for the change fields.
	if rule.Direction == "" {
		rule.Direction = "any"
	}
	if rule.Percent == 0 {
		rule.Percent = 50
	}
	if rule.Labels == nil {
		rule.Labels = map[string]string{}
	}
	labels, err := json.Marshal(rule.Labels)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	ctx := r.Context()
	tenantID := s.TenantID
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	if projectID == 0 {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	limit, err := productAlertLimit(ctx, h.pool, tenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	tx, err := h.pool.Raw().Begin(ctx)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The workspace row is the lock, as for projects: the axis counts across
	// the workspace, and READ COMMITTED hides a concurrent create's
	// uncommitted row from a count that is merely inside a transaction.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM tenant WHERE id = $1 FOR UPDATE`, tenantID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	var have int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM product_alert WHERE tenant_id = $1`, tenantID).Scan(&have); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	if limit != nil && have >= *limit {
		writeProductAlertWall(ctx, w, h.pool, *limit, have)
		return
	}
	var publicID pgtype.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO product_alert (tenant_id, project_id, name, event, labels, measure, currency,
		   kind, direction, percent, window_hours, widget_id, paused, state)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, $11, $12, $13, 'learning')
		 RETURNING public_id`,
		tenantID, projectID, rule.Name, rule.Event, string(labels), rule.Measure, rule.Currency,
		rule.Kind, rule.Direction, rule.Percent, rule.WindowHours, rule.WidgetID, rule.Paused).Scan(&publicID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	h.answerProductAlert(w, ctx, tenantID, projectID, publicID, http.StatusCreated)
}

// PATCH /v1/product-alerts/{id} — merge, validate the merged rule, and reset
// the episode when what the rule measures changed.
func (h *writeAPI) patchProductAlert(w http.ResponseWriter, r *http.Request, s sqlc.Session) {
	var req productAlertWrite
	if !decodeStrict(w, r, &req) {
		return
	}
	ctx := r.Context()
	tenantID := s.TenantID
	projectID := currentProjectID(ctx, h.pool, s, tenantID)
	id := productAlertPathID(r.URL.Path)
	if !id.Valid || projectID == 0 {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	rows, err := h.readProductAlerts(ctx, tenantID, projectID, id)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	if len(rows) == 0 {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	stored := rows[0].productAlertRule
	rule := req.mergeInto(stored)
	if msg := validateProductAlert(rule); msg != "" {
		writeAPIErrMsg(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	if rule.Labels == nil {
		rule.Labels = map[string]string{}
	}
	labels, err := json.Marshal(rule.Labels)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	tag, err := h.pool.Raw().Exec(ctx,
		`UPDATE product_alert SET name = $4, event = $5, labels = $6::jsonb, measure = $7,
		   currency = $8, kind = $9, direction = $10, percent = $11, window_hours = $12,
		   widget_id = $13, paused = $14,
		   state = CASE WHEN $15::boolean THEN 'learning' ELSE state END,
		   last_fired_at = CASE WHEN $15::boolean THEN NULL ELSE last_fired_at END
		 WHERE public_id = $1 AND tenant_id = $2 AND project_id = $3`,
		id, tenantID, projectID, rule.Name, rule.Event, string(labels), rule.Measure, rule.Currency,
		rule.Kind, rule.Direction, rule.Percent, rule.WindowHours, rule.WidgetID, rule.Paused,
		episodeChanged(stored, rule))
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	if tag.RowsAffected() == 0 {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	h.answerProductAlert(w, ctx, tenantID, projectID, id, http.StatusOK)
}

// DELETE /v1/product-alerts/{id} — 204, or 404 for a rule this project lacks.
func (h *writeAPI) deleteProductAlert(w http.ResponseWriter, r *http.Request, s sqlc.Session) {
	ctx := r.Context()
	projectID := currentProjectID(ctx, h.pool, s, s.TenantID)
	id := productAlertPathID(r.URL.Path)
	if !id.Valid || projectID == 0 {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	tag, err := h.pool.Raw().Exec(ctx,
		`DELETE FROM product_alert WHERE public_id = $1 AND tenant_id = $2 AND project_id = $3`,
		id, s.TenantID, projectID)
	switch {
	case err != nil:
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
	case tag.RowsAffected() == 0:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
