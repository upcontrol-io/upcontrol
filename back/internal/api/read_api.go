// Read-only API handlers for the remaining /v1/* endpoints: session-scoped,
// mockData-shaped. Static parts (connectable lists, the ladder) are constants.

package api

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"

	"go.upcontrol.io/back/internal/account/session"
	notifysettings "go.upcontrol.io/back/internal/channel/notify"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// readAPI serves all the GET endpoints that don't need their own file.
type readAPI struct {
	pool *pg.Pool
	ch   *ch.Conn
	sess *session.Manager
	// botUsername resolves to "" when no bot is configured: no Telegram
	// destination is offered. A func: the username arrives at runtime.
	botUsername func(context.Context) string
}

func NewReadAPI(p *pg.Pool, chConn *ch.Conn, sm *session.Manager, botUsername func(context.Context) string) *readAPI {
	return &readAPI{pool: p, ch: chConn, sess: sm, botUsername: botUsername}
}

func (h *readAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, err := h.sess.FromRequest(r.Context(), r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	switch r.URL.Path {
	case "/v1/plan":
		h.plan(w, r, s.TenantID)
	case "/v1/channels":
		h.channels(w, r, s.TenantID)
	case "/v1/recipients":
		h.recipients(w, r, s.TenantID)
	case "/v1/incidents":
		h.incidents(w, r, s.TenantID)
	case "/v1/sources":
		h.sources(w, r, s.TenantID)
	case "/v1/overview":
		h.overview(w, r, s.TenantID)
	default:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	}
}

// GET /v1/plan — the plan numbers (single-sourced with Pricing.tsx).
func (h *readAPI) plan(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	var plan string
	_ = h.pool.Raw().QueryRow(ctx, `SELECT plan FROM tenant WHERE id = $1`, tenantID).Scan(&plan)
	if plan == "" {
		plan = "Free"
	}
	ent, _ := h.pool.Queries().GetPlanEntitlement(ctx, plan)
	used, _ := h.pool.Queries().CountMonitors(ctx, tenantID)
	tgUsed, tgMax, _ := countTelegramRecipients(ctx, h.pool, tenantID)
	resp := map[string]any{
		"plan": plan,
		// How often this plan may check: the picker needs the wall, and a
		// hardcoded 5m would be a second home for this row's number.
		"minIntervalSec": int(ent.MinIntervalSec),
		"httpChecks":     map[string]int{"used": int(used), "max": int(ent.HttpChecks)},
		// Depth, not consumption — the ring has no remainder, so the client says
		// it in a sentence. Both numbers are the plan's own entitlement row.
		"logWindow":           map[string]any{"lines": int(ent.WindowLines), "approxHours": float64(ent.WindowHours)},
		"incidentHistoryDays": int(ent.IncidentDays),
		// Telegram recipients are the one counted human axis: both numbers
		// are measured here, never hardcoded client-side.
		"telegramRecipients": map[string]int{"used": tgUsed, "max": tgMax},
	}
	writeAPIJSON(w, http.StatusOK, resp)
}

// GET /v1/channels — connected channels + what is still connectable.
func (h *readAPI) channels(w http.ResponseWriter, r *http.Request, tenantID int64) {
	rows, _ := h.pool.Queries().ListChannelsByTenant(r.Context(), tenantID)
	channels := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		ch := map[string]any{
			"id":     uuidStr(row.PublicID),
			"kind":   row.Kind,
			"target": row.Target,
			// Always the resolved object, never the sparse column: the screen
			// renders state, not guesses.
			"notify": notifysettings.Resolve(row.Notify),
		}
		// label is the Telegram chat's own name, sent only when the column is
		// set: channels with no name keep their sparse shape.
		if row.Label != nil {
			ch["label"] = *row.Label
		}
		// mutedUntil travels only while the window runs: the screen must not
		// render "muted" over a channel whose alerts already flow.
		if row.MutedUntil.Valid && row.MutedUntil.Time.After(time.Now()) {
			ch["mutedUntil"] = row.MutedUntil.Time.UTC().Format(time.RFC3339)
		}
		channels = append(channels, ch)
	}
	// What can still be connected. Telegram is absent: a personal destination
	// is a person, connected by a one-time invite from People.
	connectable := []map[string]any{}
	connectable = append(connectable,
		map[string]any{"kind": "email", "name": "Email", "field": "Someone on the team", "hint": "Goes out alongside Telegram, never instead of it."},
		map[string]any{"kind": "discord", "name": "Discord", "field": "Webhook URL", "placeholder": "https://discord.com/api/webhooks/…", "hint": "Server Settings → Integrations → Webhooks."},
		map[string]any{"kind": "slack", "name": "Slack", "field": "Webhook URL", "placeholder": "https://hooks.slack.com/services/…", "hint": "Slack app settings → Incoming Webhooks."},
	)
	// Alerts that died in the queue: a broken channel must be tellable from a
	// working one. Tests are excluded; next_try_at is the recency stamp.
	undelivered := 0
	_ = h.pool.Raw().QueryRow(r.Context(),
		`SELECT count(*) FROM delivery_queue
		  WHERE tenant_id = $1 AND state = 'dead' AND class <> 'test'
		    AND next_try_at > now() - interval '24 hours'`, tenantID).Scan(&undelivered)
	writeAPIJSON(w, http.StatusOK, map[string]any{"channels": channels, "connectableChannels": connectable, "undelivered": undelivered})
}

// GET /v1/recipients — people (e-mail + Telegram members) + pending invite links.
func (h *readAPI) recipients(w http.ResponseWriter, r *http.Request, tenantID int64) {
	rows, _ := h.pool.Queries().ListRecipientsByTenant(r.Context(), tenantID)
	recs := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		name := row.Name
		if name == "" {
			name = emailLocal(ptrStrSafe(row.Email))
		}
		rec := map[string]any{
			"id":       uuidStr(row.PublicID),
			"initials": initials2(name, ptrStrSafe(row.Email)),
			"name":     name,
			"email":    ptrStrSafe(row.Email),
			"role":     row.Role,
			"status":   row.Status,
		}
		// A member with a linked telegram_id is a verified Telegram recipient
		// (written only by invite redemption).
		if row.TelegramID != nil {
			rec["telegram"] = true
		}
		recs = append(recs, rec)
	}
	resp := map[string]any{"recipients": recs}
	// Telegram invites exist only when a bot does: without a configured bot
	// username the whole Telegram surface is absent (the e-mail list stays).
	if h.botUsername(r.Context()) != "" {
		resp["telegramEnabled"] = true
		invites := []map[string]any{}
		// LEFT JOIN person: a person-bound invite carries the person's public
		// id so the Team row finds its own pending link.
		if rows, ierr := h.pool.Raw().Query(r.Context(),
			`SELECT ti.id, ti.created_at, ti.expires_at, p.public_id
			  FROM telegram_invite ti
			  LEFT JOIN person p ON p.id = ti.person_id
			 WHERE ti.tenant_id = $1 AND ti.redeemed_at IS NULL AND ti.expires_at > now()
			 ORDER BY ti.created_at DESC`, tenantID); ierr == nil {
			for rows.Next() {
				var id int64
				var createdAt, expiresAt time.Time
				var personID pgtype.UUID
				if rows.Scan(&id, &createdAt, &expiresAt, &personID) == nil {
					invite := map[string]any{
						"id":        strconv.FormatInt(id, 10),
						"status":    "pending",
						"createdAt": createdAt.UTC().Format(time.RFC3339),
						"expiresAt": expiresAt.UTC().Format(time.RFC3339),
					}
					if personID.Valid {
						invite["personId"] = uuidStr(personID)
					}
					invites = append(invites, invite)
				}
			}
			rows.Close()
		}
		resp["telegramInvites"] = invites
	}
	writeAPIJSON(w, http.StatusOK, resp)
}

// GET /v1/incidents — incident history (newest first).
func (h *readAPI) incidents(w http.ResponseWriter, r *http.Request, tenantID int64) {
	rows, _ := h.pool.Queries().ListIncidentsByTenant(r.Context(),
		sqlc.ListIncidentsByTenantParams{TenantID: tenantID, Limit: 20})
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, h.incidentWithEvidence(r.Context(), row))
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GET /v1/sources — connected sources + what is still connectable.
func (h *readAPI) sources(w http.ResponseWriter, r *http.Request, tenantID int64) {
	// The connectable list is a constant on purpose: it describes what CAN be
	// connected, not what this tenant has. What the tenant has is read below.
	connectable := []map[string]any{
		{"key": "installer", "name": "Add it to your code", "setupTime": "1 command", "installer": true},
		{"key": "deployhooks", "name": "Deploy hooks", "setupTime": "1 min", "docs": "/docs/integrations/vercel"},
		// No grafana tile: internal/source/webhook verifies stripe, github and
		// vercel only, so the tile would create a row that can receive nothing.
	}
	signals, _ := h.pool.Queries().TenantSignals(r.Context(), tenantID)
	lines, lastLog := h.logSummary(r.Context(), tenantID, signals)
	conns, _ := h.pool.Queries().ListSourceConnections(r.Context(), tenantID)
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"sources":            append(sourcesFromSignals(signals, lines, lastLog), connectedSources(conns)...),
		"connectableSources": connectable,
	})
}

// GET /v1/overview — the Dashboard aggregate.
func (h *readAPI) overview(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	monRows, _ := h.pool.Queries().ListMonitorsByTenant(ctx, tenantID)
	monitors := make([]map[string]any, 0, len(monRows))
	for _, row := range monRows {
		// The overview tile carries no ping URL: the checks list is where the
		// customer copies it from.
		monitors = append(monitors, monitorRowToAPI(row.Kind, row.Name, row.Target,
			ptrStrSafe(row.Keyword), row.IntervalSec, ptrStrSafe(row.Status),
			row.SslExpiresAt, row.DomainExpiresAt, row.PublicID, ""))
	}
	// Sources and ladder both derive from what the tenant has actually connected
	// (same signals as /v1/sources), never from a fixed list.
	signals, _ := h.pool.Queries().TenantSignals(ctx, tenantID)
	logLines, lastLog := h.logSummary(ctx, tenantID, signals)
	conns, _ := h.pool.Queries().ListSourceConnections(ctx, tenantID)
	sources := append(sourcesFromSignals(signals, logLines, lastLog), connectedSources(conns)...)
	ladder := []map[string]any{
		{"key": "account", "title": "Create your account", "done": true},
		// "Add the ingest key" is done when lines have actually arrived: a
		// created monitor is a different rung.
		{"key": "install", "title": "Add the ingest key", "done": logLines > 0},
		{"key": "alert", "title": "Set up an alert channel", "done": signals.ChannelCount > 0},
	}
	metrics, uptime, health := h.availability(ctx, tenantID)
	resp := map[string]any{
		"sources":  sources,
		"monitors": monitors,
		"metrics":  metrics,
		"ladder":   ladder,
	}
	// Absent until a check has run in the last 24 hours: a made-up "100%" is
	// the one number a monitoring product may not invent.
	if uptime != nil {
		resp["uptime"] = uptime
	}
	if health != nil {
		resp["health"] = health
	}
	// Open incident (if any).
	incRows, _ := h.pool.Queries().ListIncidentsByTenant(ctx,
		sqlc.ListIncidentsByTenantParams{TenantID: tenantID, Limit: 1})
	if len(incRows) > 0 && (incRows[0].Status == "down" || incRows[0].Status == "check") {
		// Same shape as /v1/incidents: the Dashboard branches on `ongoing`, and a
		// three-field incident silently read as "nothing is wrong".
		resp["incident"] = h.incidentWithEvidence(ctx, incRows[0])
	}
	writeAPIJSON(w, http.StatusOK, resp)
}

// logSummary reads "is this project sending lines, and when was the last one"
// through ring.QueryBuilder, the only permitted path to the logs table.
func (h *readAPI) logSummary(ctx context.Context, tenantID int64, s sqlc.TenantSignalsRow) (uint64, time.Time) {
	if h.ch == nil || s.ProjectID == 0 {
		return 0, time.Time{}
	}
	lq := query.New(tenantID, s.ProjectID, s.CutoffSeq).Summary()
	var lines uint64
	var last time.Time
	if err := h.ch.Raw().QueryRow(ctx, lq.SQL, lq.Args...).Scan(&lines, &last); err != nil {
		return 0, time.Time{}
	}
	return lines, last
}

// healthSegments is the width of the 7-day line: 42 four-hour buckets, exactly
// what `checks` retains, so the line spans the record and never claims more.
const (
	healthSegments  = 42
	healthBucketDur = 4 * time.Hour
)

// availability computes the uptime tile and the 7-day health line from the
// checks table; nil/empty when nothing has been checked.
func (h *readAPI) availability(ctx context.Context, tenantID int64) (metrics []map[string]any, uptime map[string]any, health map[string]any) {
	// The product tiles are the events pipeline's output, read from
	// ClickHouse; under 7 days of history produces no tile.
	metrics = []map[string]any{}
	if h.ch == nil {
		return metrics, nil, nil
	}
	if stats, err := h.ch.MetricSummary(ctx, tenantID); err == nil {
		metrics = metricTiles(stats)
	}
	rows, err := h.ch.Raw().Query(ctx, `
		SELECT toStartOfInterval(ts, INTERVAL 4 HOUR) AS bucket,
		       countIf(ok = 1) AS ok_count,
		       count() AS total_count
		  FROM checks
		 WHERE tenant_id = ? AND ts >= now() - INTERVAL 7 DAY
		 GROUP BY bucket ORDER BY bucket`, uint64(tenantID))
	if err != nil {
		return metrics, nil, nil
	}
	defer func() { _ = rows.Close() }()

	type bucket struct{ ok, total uint64 }
	byBucket := map[time.Time]bucket{}
	for rows.Next() {
		var at time.Time
		var ok, total uint64
		if err := rows.Scan(&at, &ok, &total); err != nil {
			return metrics, nil, nil
		}
		byBucket[at.UTC()] = bucket{ok: ok, total: total}
	}
	if len(byBucket) == 0 {
		return metrics, nil, nil
	}

	// Walk the 42 buckets back from the current one so the last segment is "now"
	// however sparse the data in between is.
	now := time.Now().UTC()
	end := now.Truncate(healthBucketDur)
	segments := make([]string, healthSegments)
	var day, dayOK, week, weekOK uint64
	for i := range healthSegments {
		at := end.Add(-time.Duration(healthSegments-1-i) * healthBucketDur)
		b, seen := byBucket[at]
		switch {
		case !seen || b.total == 0:
			segments[i] = "nodata"
		case b.ok == b.total:
			segments[i] = "ok"
		case b.ok*100 >= b.total*95:
			segments[i] = "check"
		default:
			segments[i] = "down"
		}
		week += b.total
		weekOK += b.ok
		if at.After(now.Add(-24 * time.Hour)) {
			day += b.total
			dayOK += b.ok
		}
	}

	// Uptime travels as its own field, not a tile: `metrics` stays the product
	// tiles, and the Dashboard puts uptime in the calm line.
	if day > 0 {
		uptime = map[string]any{"value": pctLabel(dayOK, day), "note": "last 24h"}
	}
	if week == 0 {
		return metrics, uptime, nil
	}
	return metrics, uptime, map[string]any{
		"uptimeLabel": pctLabel(weekOK, week) + " over 7 days · " + strconv.FormatUint(week, 10) + " checks",
		"segments":    segments,
	}
}

// metricTiles turns metric stats into the Dashboard's tile shape; under 7 days
// of history ships no tile. Only names metricTileUnits knows are shown.
func metricTiles(stats []ch.MetricStat) []map[string]any {
	tiles := make([]map[string]any, 0, len(stats))
	for _, s := range stats {
		unit, known := ch.MetricTileUnits()[s.Name]
		if !known || s.Days < 7 {
			continue
		}
		tiles = append(tiles, map[string]any{
			"label": tileLabel(s.Name),
			"value": tileValue(s.Latest, unit),
			"note":  "usually " + tileRange(s.P10, s.P90, unit),
			"spark": sparkOf(s),
		})
	}
	return tiles
}

// sparkOf is the 12-point series the tile draws, never nil: the front maps
// over it and an absent key is a crash.
func sparkOf(s ch.MetricStat) []float64 {
	if len(s.Spark) == 0 {
		return []float64{}
	}
	return s.Spark
}

func tileLabel(name string) string {
	switch name {
	case "signups", "signups_today":
		return "Sign-ups today"
	case "checkout_latency_ms", "checkout_latency", "latency_ms":
		return "Checkout latency"
	case "checkouts", "checkouts_today":
		return "Check-outs today"
	default:
		return name
	}
}

func tileValue(v float64, unit string) string {
	if unit == "" {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'f', -1, 64) + " " + unit
}

func tileRange(p10, p90 float64, unit string) string {
	lo := strconv.FormatFloat(p10, 'f', -1, 64)
	hi := strconv.FormatFloat(p90, 'f', -1, 64)
	if unit != "" {
		return lo + "–" + hi + " " + unit
	}
	return lo + "–" + hi
}

// pctLabel renders a ratio the way the screens do: two decimals, and a bare
// "100%" only when nothing failed at all.
func pctLabel(ok, total uint64) string {
	if total == 0 {
		return "—"
	}
	if ok == total {
		return "100%"
	}
	return strconv.FormatFloat(float64(ok)/float64(total)*100, 'f', 2, 64) + "%"
}

// sourcesFromSignals builds the Sources list from what the tenant actually
// connected: no checks and no lines means an EMPTY list, not a claim.
func sourcesFromSignals(s sqlc.TenantSignalsRow, logLines uint64, lastLog time.Time) []map[string]any {
	sources := make([]map[string]any, 0, 2)
	if s.MonitorCount > 0 {
		status, signal := "nodata", "no checks yet"
		if s.LastCheckAt.Valid {
			signal = "up · " + agoLabel(s.LastCheckAt.Time)
			status = "ok"
			if s.MonitorsDown > 0 {
				status, signal = "down", "failing · "+agoLabel(s.LastCheckAt.Time)
			}
		}
		sources = append(sources, map[string]any{
			"id": "src_checks", "mark": "URL", "name": "Site checks",
			"status": status, "lastSignal": signal, "paused": false,
		})
	}
	if logLines > 0 {
		// Silence is the signal here: a stream that stopped an hour ago is not
		// the same as one that is flowing, and both are "connected".
		status, signal := "ok", "no data yet"
		if !lastLog.IsZero() {
			signal = agoLabel(lastLog)
			if time.Since(lastLog) > time.Hour {
				status = "nodata"
			}
		}
		sources = append(sources, map[string]any{
			"id": "src_logs", "mark": "LOG", "name": "App logs",
			"status": status, "lastSignal": signal, "paused": false,
		})
	}
	return sources
}

// connectedSources renders the rows the tenant connected by hand. A paused
// source keeps its row and says so: pausing is not deleting.
func connectedSources(rows []sqlc.ListSourceConnectionsRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// Status is derived from the signal, not the column: a connection that
		// never delivered anything cannot show a green dot.
		signal, status := "waiting...", "nodata"
		if row.LastSignalAt.Valid {
			signal, status = agoLabel(row.LastSignalAt.Time), row.Status
		}
		if row.Paused {
			status, signal = "nodata", "paused"
		}
		mark := strings.ToUpper(row.Kind)
		if len(mark) > 3 {
			mark = mark[:3]
		}
		entry := map[string]any{
			"id":   "src_" + strconv.FormatInt(row.ID, 10),
			"mark": mark,
			// The kind ties this row back to its Connect tile. The derived rows
			// (src_checks, src_logs) carry none: nothing offers to add them.
			"kind":       row.Kind,
			"name":       sourceName(row.Kind),
			"status":     status,
			"lastSignal": signal,
			"paused":     row.Paused,
		}
		// The connection's inbound hook token: the front builds
		// {origin}/hooks/{token} from it.
		if row.HookToken != nil && *row.HookToken != "" {
			entry["hookToken"] = *row.HookToken
		}
		// The receipt: what the last event was, so the panel can show it.
		if row.LastEvent != nil && *row.LastEvent != "" {
			entry["lastEvent"] = *row.LastEvent
		}
		out = append(out, entry)
	}
	return out
}

// sourceName turns the stored kind into the name the screen shows. Kinds are
// ours (they match the webhook routes); the names are the reader's.
func sourceName(kind string) string {
	switch kind {
	case "deployhooks", "vercel":
		return "Deploy hooks"
	// case "grafana": returns with the receiver (see the connectable list above).
	case "github":
		return "GitHub"
	case "stripe":
		return "Payments"
	default:
		return kind
	}
}

// agoLabel renders a timestamp the way the app's rows do: relative, coarse, and
// never in the future ("just now" covers clock skew).
func agoLabel(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + " d ago"
	}
}

// incidentWithEvidence is incidentToAPI plus the timeline and the frozen log
// slice. The timeline folds in the tenant's events around the break.
func (h *readAPI) incidentWithEvidence(ctx context.Context, row sqlc.ListIncidentsByTenantRow) map[string]any {
	inc := incidentToAPI(row)

	var lifecycle []timelineMark
	if updates, err := h.pool.Queries().ListIncidentUpdates(ctx, row.ID); err == nil {
		lifecycle = make([]timelineMark, 0, len(updates))
		for _, u := range updates {
			mark := timelineMark{
				Kind: timelineKind(u.Kind),
				Text: u.Text,
			}
			if u.At.Valid {
				mark.At = u.At.Time
			}
			lifecycle = append(lifecycle, mark)
		}
	}

	// 30 minutes before the open through the close; `detectedAt` is the pivot,
	// so the 50-row budget goes to the events NEAREST the break.
	var events []ch.EventRow
	if h.ch != nil && row.DetectedAt.Valid {
		end := time.Now()
		if row.ResolvedAt.Valid {
			end = row.ResolvedAt.Time
		}
		events, _ = h.ch.EventsAround(ctx, row.TenantID,
			row.DetectedAt.Time.Add(-30*time.Minute), end, row.DetectedAt.Time, 50)
	}
	if merged := mergeTimeline(lifecycle, events); len(merged) > 0 {
		inc["timeline"] = merged
	}

	if lines, err := h.pool.Queries().ListIncidentSlice(ctx, row.ID); err == nil && len(lines) > 0 {
		slice := make([]string, 0, len(lines))
		for _, l := range lines {
			// Same "HH:MM:SS  message" shape the live stream uses, so the card's
			// renderer (LogMessage) colours a JSON line here exactly as there.
			at := ""
			if l.Ts.Valid {
				at = l.Ts.Time.Format("15:04:05") + "  "
			}
			slice = append(slice, at+l.Message)
		}
		inc["logSlice"] = slice
	}
	return inc
}

// timelineKind maps a lifecycle mark to the front's five event kinds: `opened`
// an error, `resolved` a check, anything with a person is people.
func timelineKind(kind string) string {
	switch kind {
	case "opened":
		return "error"
	case "resolved":
		return "check"
	case "escalated", "acked":
		return "people"
	default:
		return "check"
	}
}

// eventKind maps an event name to the front's timeline kinds: `triageOf` reads
// `kind === 'deploy'` to build its hypothesis and rollback command.
func eventKind(name string) string {
	switch {
	case strings.HasPrefix(name, "deploy"), strings.Contains(name, "deployment"):
		return "deploy"
	case strings.HasPrefix(name, "payment"), strings.HasPrefix(name, "charge"),
		strings.HasPrefix(name, "invoice"):
		return "payment"
	case strings.Contains(name, "error"), strings.Contains(name, "fail"):
		return "error"
	default:
		return "check"
	}
}

// eventText is the line the card prints. A deploy names its sha: the triage's
// fix is `vercel rollback <sha>`.
func eventText(e ch.EventRow) string {
	if sha := e.Labels["sha"]; sha != "" {
		return "Deploy " + sha
	}
	return e.Name
}

// timelineMark is one lifecycle row before rendering; a zero At renders as
// an empty time, as before, and sorts first.
type timelineMark struct {
	At   time.Time
	Kind string
	Text string
}

// mergeTimeline folds events into the lifecycle marks, oldest first by timestamp.
// The old sort on the display string put midnight before eleven.
func mergeTimeline(lifecycle []timelineMark, events []ch.EventRow) []map[string]any {
	if len(lifecycle) == 0 && len(events) == 0 {
		return nil
	}

	type entry struct {
		at time.Time
		m  map[string]any
	}
	out := make([]entry, 0, len(lifecycle)+len(events))

	for _, e := range events {
		out = append(out, entry{
			at: e.TS,
			m: map[string]any{
				"time": e.TS.Format("15:04"),
				"ago":  agoLabel(e.TS),
				"kind": eventKind(e.Name),
				"text": eventText(e),
			},
		})
	}
	for _, mark := range lifecycle {
		timeStr, agoStr := "", ""
		if !mark.At.IsZero() {
			timeStr = mark.At.Format("15:04")
			agoStr = agoLabel(mark.At)
		}
		out = append(out, entry{
			at: mark.At,
			m: map[string]any{
				"time": timeStr,
				"ago":  agoStr,
				"kind": mark.Kind,
				"text": mark.Text,
			},
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].at.Before(out[j].at)
	})

	maps := make([]map[string]any, len(out))
	for i, e := range out {
		maps[i] = e.m
	}
	return maps
}

// incidentToAPI builds the front's Incident shape. `timeline` and `logSlice`
// ship as empty arrays, never omitted: the card iterates them.
func incidentToAPI(row sqlc.ListIncidentsByTenantRow) map[string]any {
	ongoing := row.Status == "down" || row.Status == "check"
	inc := map[string]any{
		"id":              uuidStr(row.PublicID),
		"title":           row.Title,
		"status":          row.Status,
		"affectedCount":   row.AffectedCount,
		"ongoing":         ongoing,
		"since":           "",
		"durationMinutes": 0,
		"timeline":        []any{},
		"logSlice":        []string{},
	}
	if row.DetectedAt.Valid {
		inc["since"] = row.DetectedAt.Time.Format("15:04")
		end := time.Now()
		if row.ResolvedAt.Valid {
			end = row.ResolvedAt.Time
		}
		if minutes := int(end.Sub(row.DetectedAt.Time).Minutes()); minutes > 0 {
			inc["durationMinutes"] = minutes
		}
	}
	if row.ResolvedAt.Valid {
		inc["result"] = map[string]any{"status": "fixed", "text": "Resolved."}
	}
	return inc
}

func emailLocal(email string) string {
	if i := strings.LastIndexByte(email, '@'); i >= 0 {
		return email[:i]
	}
	return email
}

func initials2(name, email string) string {
	if len(name) >= 2 {
		return string(toUpper(name[0])) + string(toUpper(name[1]))
	}
	if len(email) >= 2 {
		return string(toUpper(email[0])) + string(toUpper(email[1]))
	}
	return "U"
}

func toUpper(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - 32
	}
	return b
}
