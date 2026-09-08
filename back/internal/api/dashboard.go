// The board's two reads: GET /v1/dashboard/catalog (what this project actually
// sends, so a widget is picked from real names) and POST /v1/series (every
// widget's points in one round trip), and the board itself: GET and PUT
// /v1/dashboard, one stored layout per project. The session doors are scoped
// to the session's current project, like /v1/logs; the agent's key-authenticated
// doors into the same document (see resolveAgentKey) are taken before the
// session gate in ServeHTTP.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	apigen "go.upcontrol.io/back/gen/api"
	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/ingest"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// The catalog describes the last week, and describes it in short lists: a
// picker is read, not scrolled.
const (
	catalogWindow          = 7 * 24 * time.Hour
	catalogGroupsPerLevel  = 8
	catalogAttrsPerService = 10
	// The attribute pass expands every line into one row per attribute, so it
	// reads the newest lines rather than the whole week.
	catalogAttrScan = 20000
)

// seriesRange is one entry of the range enum: how wide a bucket is and how
// many of them a range holds. step × buckets IS the span, so the two numbers
// cannot drift from each other.
type seriesRange struct {
	step    int
	buckets int
}

var seriesRanges = map[string]seriesRange{
	"1h":   {60, 60},
	"4h":   {300, 48},
	"12h":  {900, 48},
	"24h":  {1800, 48},
	"7d":   {10800, 56},
	"31d":  {43200, 62},
	"365d": {86400, 365},
}

// maxSeriesQueries bounds one board's read: a widget asks for one series per
// metric it draws, and forty is more than a screen can hold.
const maxSeriesQueries = 40

// maxPeopleSteps bounds a funnel's step list: its answer is one row per step,
// so the list's width is the answer's width — the same cap the grouped reads
// put on rows.
const maxPeopleSteps = 200

// rangeDays is how deep a range reaches, rounded up to whole days, the unit
// the plan sells. Every sub-day range is 1: the axis is depth, and a 1h chart
// asks no more of the store than a 24h one.
func rangeDays(r seriesRange) int {
	return (r.step*r.buckets + 86399) / 86400
}

// readsRollup says which table a log query reads: the hourly rollup when the
// range steps in whole hours and the filter is one the rollup's key can
// answer. A substring or attribute filter has to see the lines themselves.
func readsRollup(r seriesRange, f query.SeriesFilter) bool {
	return r.step >= 3600 && f.Search == "" && len(f.Attrs) == 0
}

// historyLabel words a depth the way the front words it: a day is the window
// a reader recognises as "24 hours", two months and beyond read in months.
// 30.4 is the mean month, so 365 lands on 12 rather than 11.
func historyLabel(days int) string {
	switch {
	case days <= 1:
		return "24 hours"
	case days < 60:
		return strconv.Itoa(days) + " days"
	default:
		return strconv.Itoa(int(math.Round(float64(days)/30.4))) + " months"
	}
}

// historyReason is the 402's copy, and the front spells it identically. The
// top of the ladder drops " and up": there is nothing above it to buy.
func historyReason(days int, plan string) string {
	andUp := " and up"
	if plan == planLadder[len(planLadder)-1] {
		andUp = ""
	}
	return historyLabel(days) + " of history is on " + plan + andUp +
		". It counts from the day you switch."
}

// historyRefusal is the history axis's wall: "" while every query in the batch
// sits inside the plan's depth, else the 402's message and the plan that lifts
// it. Unlike the projects wall this one does not fail open: the depth decides
// what is read at all, so an entitlement read that broke is a 500, not a free
// year of history.
func (h *writeAPI) historyRefusal(ctx context.Context, tenantID int64, qs []seriesQuery) (msg, plan string, err error) {
	tenantPlan, _ := h.pool.Queries().GetTenantPlan(ctx, tenantID)
	if tenantPlan == "" {
		tenantPlan = "Free"
	}
	ent, err := h.pool.Queries().GetPlanEntitlement(ctx, tenantPlan)
	if err != nil {
		return "", "", err
	}
	if ent.HistoryDays == nil {
		return "", "", nil // NULL = unlimited
	}
	limit := int(*ent.HistoryDays)
	// The widest query decides, so the one refusal names the plan that lifts
	// the whole batch rather than the first rung on the way there.
	days := 0
	for _, q := range qs {
		if d := rangeDays(seriesRanges[q.Range]); d > days {
			days = d
		}
	}
	if days <= limit {
		return "", "", nil
	}
	lift := cheapestPlan(ctx, h.pool, func(e sqlc.PlanEntitlement) bool {
		return e.HistoryDays == nil || int(*e.HistoryDays) >= days
	})
	if lift == "" {
		return "History this deep is not on any plan.", "", nil
	}
	return historyReason(days, lift), strings.ToLower(lift), nil
}

type seriesQuery struct {
	ID     string            `json:"id"`
	Source string            `json:"source"`
	Range  string            `json:"range"`
	Name   string            `json:"name"`
	Where  map[string]string `json:"where"`
	Group  []string          `json:"group"`
	// Steps and Cohort serve the people source alone: a funnel's step names
	// and the retention grid's unit.
	Steps  []string `json:"steps"`
	Cohort string   `json:"cohort"`
}

type seriesRequest struct {
	Queries []seriesQuery `json:"queries"`
}

// validateSeries returns "" when the whole batch is answerable, else the
// message naming what is wrong and which query it is wrong in. Nothing is
// queried until this passes: a bad entry must not cost a round trip to the
// database for the good ones.
func validateSeries(qs []seriesQuery) string {
	if len(qs) == 0 {
		return "queries must hold at least one entry"
	}
	if len(qs) > maxSeriesQueries {
		return fmt.Sprintf("at most %d queries per request, got %d", maxSeriesQueries, len(qs))
	}
	seen := make(map[string]bool, len(qs))
	for _, q := range qs {
		if q.ID == "" {
			return "every query needs a non-empty id"
		}
		if seen[q.ID] {
			return "duplicate query id " + q.ID
		}
		seen[q.ID] = true
		switch q.Source {
		case "logs", "check", "event", "metric", "people":
		default:
			return "unknown source " + q.Source + " in " + q.ID
		}
		if _, ok := seriesRanges[q.Range]; !ok {
			return "unknown range " + q.Range + " in " + q.ID
		}
		if q.Source != "people" && (len(q.Steps) > 0 || q.Cohort != "") {
			return "steps and cohort are only for the people source in " + q.ID
		}
		if q.Cohort != "" && q.Cohort != "week" {
			return "unknown cohort " + q.Cohort + " in " + q.ID
		}
		if len(q.Group) > 0 {
			if q.Source != "metric" && q.Source != "people" {
				return "group is only for the metric and people sources in " + q.ID
			}
			if len(q.Group) > 2 {
				return "group takes one or two label keys in " + q.ID
			}
			// A people group can carry event names (an experiment's two), which
			// the SDK lets run to 60; a metric group key is a label key at 40.
			keyMax := 40
			if q.Source == "people" {
				keyMax = 60
			}
			for _, k := range q.Group {
				if k == "" || len(k) > keyMax {
					return "bad group key in " + q.ID
				}
			}
		}
		if q.Source == "people" {
			// The shape decides the read; a query with none of them names
			// nothing answerable.
			switch {
			case len(q.Steps) > 0:
				if len(q.Steps) > maxPeopleSteps {
					return fmt.Sprintf("at most %d steps per query, got %d in %s", maxPeopleSteps, len(q.Steps), q.ID)
				}
				for _, st := range q.Steps {
					if st == "" || len(st) > 40 {
						return "bad step name in " + q.ID
					}
				}
			case q.Cohort == "week":
				// Retention: the range is the whole question.
			case len(q.Group) == 1 || len(q.Group) == 2:
				// Breakdown (one key, an event name in `name`) or an experiment
				// (two keys, the exposed and converted event names, with `name`
				// the label key that carries the arm).
				if q.Name == "" {
					return "a people group query needs a name in " + q.ID
				}
			default:
				return "a people query needs steps, a cohort, or a group in " + q.ID
			}
		}
	}
	return ""
}

// seriesWindow places a range on the clock: `to` is rounded UP to the next
// bucket boundary so the last bucket is the current, partial one, and `from`
// is a whole span back — which leaves both ends on a boundary, so a bucket
// index is arithmetic rather than a lookup.
func seriesWindow(rng string, now time.Time) (from, to time.Time, r seriesRange, ok bool) {
	r, ok = seriesRanges[rng]
	if !ok {
		return time.Time{}, time.Time{}, r, false
	}
	step := int64(r.step)
	end := ((now.Unix() + step - 1) / step) * step
	to = time.Unix(end, 0).UTC()
	from = to.Add(-time.Duration(r.step*r.buckets) * time.Second)
	return from, to, r, true
}

// getDashboardCatalog answers every list or none of them. An empty list is a
// real answer: a project that has sent nothing has an empty picker. A list
// that is empty because its read failed is not, and the two must never look
// alike on screen, so the first failure ends the answer with a 500.
func (h *writeAPI) getDashboardCatalog(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	projectID := h.currentProject(ctx, r, tenantID)
	qb := query.New(tenantID, projectID)
	since := time.Now().UTC().Add(-catalogWindow)

	out := map[string]any{}
	for _, part := range []struct {
		key  string
		read func() ([]map[string]any, error)
	}{
		{"services", func() ([]map[string]any, error) { return h.catalogServices(ctx, qb) }},
		{"groups", func() ([]map[string]any, error) { return h.catalogGroups(ctx, qb) }},
		{"attrs", func() ([]map[string]any, error) { return h.catalogAttrs(ctx, qb) }},
		{"checks", func() ([]map[string]any, error) { return h.catalogChecks(ctx, projectID) }},
		{"events", func() ([]map[string]any, error) { return h.catalogEvents(ctx, tenantID, projectID, since) }},
		{"metrics", func() ([]map[string]any, error) { return h.catalogMetrics(ctx, tenantID, projectID, since) }},
		{"funnels", func() ([]map[string]any, error) { return h.catalogFunnels(ctx, tenantID, projectID, since) }},
		{"experiments", func() ([]map[string]any, error) { return h.catalogExperiments(ctx, tenantID, projectID, since) }},
		{"retentions", func() ([]map[string]any, error) { return h.catalogRetentions(ctx, tenantID, projectID, since) }},
		{"dimensions", func() ([]map[string]any, error) { return h.catalogDimensions(ctx, tenantID, projectID, since) }},
	} {
		rows, err := part.read()
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "read_failed")
			return
		}
		out[part.key] = rows
	}
	writeAPIJSON(w, http.StatusOK, out)
}

// currentProject resolves the session's project the way logQueryBuilder does;
// the series reads need the id itself, not only a builder holding it.
func (h *writeAPI) currentProject(ctx context.Context, r *http.Request, tenantID int64) int64 {
	s, _ := h.sess.FromRequest(ctx, r)
	return currentProjectID(ctx, h.pool, s, tenantID)
}

// runCatalogRows executes one of the ring's catalog queries and turns each row
// into its answer. A query error, a scan error and a torn read are all errors:
// a short list is a claim about the project, and only the database may make it.
func (h *writeAPI) runCatalogRows(ctx context.Context, lq query.LogQuery, scan func(pgx.Rows) (map[string]any, error)) ([]map[string]any, error) {
	out := []map[string]any{}
	if h.pgs == nil {
		return out, nil
	}
	rows, err := h.pgs.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		row, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// catalogServices tallies the week's lines per service — the picker's roots.
func (h *writeAPI) catalogServices(ctx context.Context, qb *query.QueryBuilder) ([]map[string]any, error) {
	return h.runCatalogRows(ctx, qb.Services(catalogWindow), func(rows pgx.Rows) (map[string]any, error) {
		var name string
		var lines uint64
		if err := rows.Scan(&name, &lines); err != nil {
			return nil, err
		}
		return map[string]any{"name": name, "lines": lines}, nil
	})
}

// catalogGroups lists the week's message groups. The fingerprint is decimal
// text because it is a uint64: JSON would round it past 2^53.
func (h *writeAPI) catalogGroups(ctx context.Context, qb *query.QueryBuilder) ([]map[string]any, error) {
	return h.runCatalogRows(ctx, qb.Groups(catalogWindow, catalogGroupsPerLevel), func(rows pgx.Rows) (map[string]any, error) {
		var service, level, sample string
		var fingerprint int64
		var lines uint64
		var lastTS time.Time
		if err := rows.Scan(&service, &level, &fingerprint, &lines, &sample, &lastTS); err != nil {
			return nil, err
		}
		return map[string]any{
			"service":     service,
			"level":       level,
			"fingerprint": strconv.FormatUint(uint64(fingerprint), 10),
			"sample":      sample,
			"lines":       lines,
			"lastTs":      lastTS.UTC().Format(time.RFC3339),
		}, nil
	})
}

func (h *writeAPI) catalogAttrs(ctx context.Context, qb *query.QueryBuilder) ([]map[string]any, error) {
	return h.runCatalogRows(ctx, qb.AttrPairs(catalogWindow, catalogAttrScan, catalogAttrsPerService), func(rows pgx.Rows) (map[string]any, error) {
		var service, key, value string
		var lines uint64
		if err := rows.Scan(&service, &key, &value, &lines); err != nil {
			return nil, err
		}
		return map[string]any{"service": service, "key": key, "value": value, "lines": lines}, nil
	})
}

// catalogChecks is the project's monitors in the same id and type shape
// GET /v1/monitors hands out: two shapes for one entity is how a board ends up
// asking for a check the checks list never named.
func (h *writeAPI) catalogChecks(ctx context.Context, projectID int64) ([]map[string]any, error) {
	out := []map[string]any{}
	rows, err := h.pool.Queries().ListMonitorsByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":   uuidStr(row.PublicID),
			"name": row.Name,
			"type": monitorTypeLabel(row.Kind),
		})
	}
	return out, nil
}

func (h *writeAPI) catalogEvents(ctx context.Context, tenantID, projectID int64, since time.Time) ([]map[string]any, error) {
	out := []map[string]any{}
	if h.pgs == nil {
		return out, nil
	}
	rows, err := h.pgs.CatalogEvents(ctx, tenantID, projectID, since)
	if err != nil {
		return nil, err
	}
	for _, e := range rows {
		out = append(out, map[string]any{
			"name":   e.Name,
			"times":  e.Times,
			"lastTs": e.LastTS.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func (h *writeAPI) catalogMetrics(ctx context.Context, tenantID, projectID int64, since time.Time) ([]map[string]any, error) {
	out := []map[string]any{}
	if h.pgs == nil {
		return out, nil
	}
	rows, err := h.pgs.CatalogMetrics(ctx, tenantID, projectID, since)
	if err != nil {
		return nil, err
	}
	for _, m := range rows {
		labels := m.Labels
		if labels == nil {
			labels = []string{}
		}
		out = append(out, map[string]any{"name": m.Name, "readings": m.Readings, "labels": labels})
	}
	return out, nil
}

// counterCatalog reads one reported kind through CatalogCounters into the
// catalog's row shape. A nil member list is normalised to empty, so it
// reaches JSON as [] and not null — an empty list is a real answer.
func (h *writeAPI) counterCatalog(ctx context.Context, tenantID, projectID int64, name, keyLabel, memberLabel string, since time.Time, shape func(pgstore.CatalogCounter) map[string]any) ([]map[string]any, error) {
	out := []map[string]any{}
	if h.pgs == nil {
		return out, nil
	}
	counters, err := h.pgs.CatalogCounters(ctx, tenantID, projectID, name, keyLabel, memberLabel, since)
	if err != nil {
		return nil, err
	}
	for _, c := range counters {
		if c.Members == nil {
			c.Members = []string{}
		}
		out = append(out, shape(c))
	}
	return out, nil
}

func (h *writeAPI) catalogFunnels(ctx context.Context, tenantID, projectID int64, since time.Time) ([]map[string]any, error) {
	return h.counterCatalog(ctx, tenantID, projectID, "funnel", "funnel", "step", since, func(c pgstore.CatalogCounter) map[string]any {
		return map[string]any{"name": c.Name, "steps": c.Members}
	})
}

func (h *writeAPI) catalogExperiments(ctx context.Context, tenantID, projectID int64, since time.Time) ([]map[string]any, error) {
	return h.counterCatalog(ctx, tenantID, projectID, "experiment", "experiment", "variant", since, func(c pgstore.CatalogCounter) map[string]any {
		return map[string]any{"name": c.Name, "variants": c.Members}
	})
}

func (h *writeAPI) catalogRetentions(ctx context.Context, tenantID, projectID int64, since time.Time) ([]map[string]any, error) {
	return h.counterCatalog(ctx, tenantID, projectID, "retention", "retention", "cohort", since, func(c pgstore.CatalogCounter) map[string]any {
		return map[string]any{"name": c.Name, "cohorts": len(c.Members)}
	})
}

func (h *writeAPI) catalogDimensions(ctx context.Context, tenantID, projectID int64, since time.Time) ([]map[string]any, error) {
	return h.counterCatalog(ctx, tenantID, projectID, "breakdown", "dimension", "value", since, func(c pgstore.CatalogCounter) map[string]any {
		return map[string]any{"name": c.Name, "values": len(c.Members)}
	})
}

func (h *writeAPI) postSeries(w http.ResponseWriter, r *http.Request, tenantID int64) {
	var req seriesRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if msg := validateSeries(req.Queries); msg != "" {
		writeAPIErrMsg(w, http.StatusBadRequest, "bad_query", msg)
		return
	}
	ctx := r.Context()
	// The depth gate runs before any read: a range the plan does not carry is
	// refused whole, never answered clipped and labelled as if it were not.
	msg, plan, err := h.historyRefusal(ctx, tenantID, req.Queries)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	if msg != "" {
		writeUpgradeRequired(w, msg, plan)
		return
	}
	projectID := h.currentProject(ctx, r, tenantID)
	qb := query.New(tenantID, projectID)
	now := time.Now().UTC()
	memo := &oldestMemo{h: h, tenantID: tenantID, projectID: projectID}
	out := make([]any, 0, len(req.Queries))
	for _, q := range req.Queries {
		s, err := h.oneSeries(ctx, qb, tenantID, projectID, q, now, memo)
		if err != nil {
			// No partial body: a board that draws some of its charts and
			// silently drops the rest reads as measured, and it is not.
			writeAPIErr(w, http.StatusInternalServerError, "read_failed")
			return
		}
		out = append(out, s)
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"series": out})
}

// oneSeries answers one query. An empty project is zeros and nulls, never an
// error: a board drawn before the first line arrives is the normal first view.
// A read that failed is an error, though — a chart may not print a number
// nothing measured.
func (h *writeAPI) oneSeries(ctx context.Context, qb *query.QueryBuilder, tenantID, projectID int64, q seriesQuery, now time.Time, memo *oldestMemo) (map[string]any, error) {
	from, to, r, _ := seriesWindow(q.Range, now)
	if q.Source == "people" {
		return h.peopleSeries(ctx, tenantID, projectID, q, from, to, r)
	}
	if len(q.Group) > 0 {
		return h.groupedSeries(ctx, tenantID, projectID, q, from, to, r)
	}
	var points []any
	var total, previous any
	var err error
	switch q.Source {
	case "logs":
		points, total, previous, err = h.logsSeries(ctx, qb, q, from, to, r, memo)
	case "check":
		points, total, previous, err = h.checkSeries(ctx, tenantID, q, from, to, r)
	case "event":
		points, total, previous, err = h.eventSeries(ctx, tenantID, projectID, q, from, to, r)
	case "metric":
		points, total, previous, err = h.metricSeries(ctx, tenantID, projectID, q, from, to, r)
	}
	if err != nil {
		return nil, err
	}
	if points == nil {
		points = nullPoints(r)
	}
	return map[string]any{
		"id":       q.ID,
		"from":     from.Format(time.RFC3339),
		"to":       to.Format(time.RFC3339),
		"step":     r.step,
		"points":   points,
		"total":    total,
		"previous": previous,
	}, nil
}

// groupedSeries answers a grouped counter read: one sum per label
// combination, ordered by value.
func (h *writeAPI) groupedSeries(ctx context.Context, tenantID, projectID int64, q seriesQuery, from, to time.Time, r seriesRange) (map[string]any, error) {
	rows := []any{}
	if h.pgs != nil {
		sums, err := h.pgs.CounterGroups(ctx, tenantID, projectID, q.Name, q.Where, q.Group, from, to)
		if err != nil {
			return nil, err
		}
		rows = labelRows(sums)
	}
	return groupedAnswer(q, from, to, r, rows), nil
}

// labelRows is the one row shape every grouped answer speaks — the counter
// folds' and the people reads' alike: a label map and its value, in the order
// the read returned them.
func labelRows(sums []pgstore.LabelSum) []any {
	rows := []any{}
	for _, s := range sums {
		rows = append(rows, map[string]any{"labels": s.Labels, "value": s.Sum})
	}
	return rows
}

// groupedAnswer is a grouped read's envelope: no time axis to invent and no
// single number for the whole range, so points is empty and the totals are
// nil — a series of nulls would claim an axis the fold does not have.
func groupedAnswer(q seriesQuery, from, to time.Time, r seriesRange, rows []any) map[string]any {
	return map[string]any{
		"id":       q.ID,
		"from":     from.Format(time.RFC3339),
		"to":       to.Format(time.RFC3339),
		"step":     r.step,
		"points":   []any{},
		"rows":     rows,
		"total":    nil,
		"previous": nil,
	}
}

// peopleSeries answers the people source's four reads, every one a GROUP BY
// over events with a person behind them. The shape picks the read: steps name
// a funnel's events; cohort "week" the retention grid; a group of one narrows
// one event name to one label's values (breakdown); a group of two names an
// experiment's exposed and converted events, with `name` the label key that
// carries the arm.
func (h *writeAPI) peopleSeries(ctx context.Context, tenantID, projectID int64, q seriesQuery, from, to time.Time, r seriesRange) (map[string]any, error) {
	rows := []any{}
	if h.pgs != nil {
		var (
			sums []pgstore.LabelSum
			err  error
		)
		switch {
		case len(q.Steps) > 0:
			sums, err = h.pgs.FunnelSteps(ctx, tenantID, projectID, q.Steps, from, to)
		case q.Cohort == "week":
			sums, err = h.pgs.RetentionCohorts(ctx, tenantID, projectID, from, to)
		case len(q.Group) == 1:
			sums, err = h.pgs.BreakdownValues(ctx, tenantID, projectID, q.Name, q.Group[0], from, to)
		default: // two group keys: validateSeries refused every other shape
			sums, err = h.pgs.ExperimentArms(ctx, tenantID, projectID, q.Name, q.Group[0], q.Group[1], from, to)
		}
		if err != nil {
			return nil, err
		}
		rows = labelRows(sums)
	}
	return groupedAnswer(q, from, to, r, rows), nil
}

// nullPoints is what a series with nothing behind it draws: a gauge with no
// reading has no value, and 0 would be one.
func nullPoints(r seriesRange) []any {
	out := make([]any, r.buckets)
	return out
}

// oldest is the first row a store holds for one project on one axis; found is
// false when it holds none.
type oldest struct {
	at    time.Time
	found bool
}

// oldestMemo reads those two facts at most once per request. A board of forty
// queries would otherwise ask the same min() forty times. One request, one
// goroutine, so nothing here needs a lock.
type oldestMemo struct {
	h                   *writeAPI
	tenantID, projectID int64
	rollup, ring        *oldest
}

// of is the floor of the table the read is going to: the rollup's first hour
// or the ring's first line. They are separate facts: an upgrade starts the
// rollup fresh while the ring still carries its own window. No store at all
// reads as no rows.
func (m *oldestMemo) of(ctx context.Context, rollup bool) (*oldest, error) {
	slot := &m.ring
	if rollup {
		slot = &m.rollup
	}
	if *slot != nil {
		return *slot, nil
	}
	if m.h.pgs == nil {
		*slot = &oldest{}
		return *slot, nil
	}
	read := m.h.pgs.OldestLine
	if rollup {
		read = m.h.pgs.OldestHistory
	}
	at, found, err := read(ctx, m.tenantID, m.projectID)
	if err != nil {
		return nil, err
	}
	*slot = &oldest{at: at, found: found}
	return *slot, nil
}

// bucketMeasured says whether bucket i could have held anything. Bucket i covers
// [from + i·step, from + (i+1)·step); one that ends at or before the store's
// oldest row counted nothing because nothing was there yet, and that is not a
// zero. A store with no row at all is that for every bucket. A nil oldest is
// an axis with no such floor, where every bucket is a measured 0.
func bucketMeasured(from time.Time, r seriesRange, i int, o *oldest) bool {
	if o == nil {
		return true
	}
	if !o.found {
		return false
	}
	return from.Add(time.Duration((i+1)*r.step) * time.Second).After(o.at)
}

// countPoints turns bucket rows into a full array: a count bucket with nothing
// in it is a measured 0, not a gap, unless the whole bucket predates the
// store's oldest row, where the gap is the honest answer. total sums the
// measured buckets and is nil when there are none.
func countPoints(rows []pgstore.Bucket, r seriesRange, from time.Time, o *oldest) ([]any, any) {
	points := make([]any, r.buckets)
	var total int64
	var anyMeasured bool
	for i := range points {
		if !bucketMeasured(from, r, i, o) {
			continue // nil: the store held nothing to count yet
		}
		points[i] = int64(0)
		anyMeasured = true
	}
	for _, b := range rows {
		if b.Index < 0 || b.Index >= int64(r.buckets) {
			continue
		}
		points[b.Index] = b.Count
		total += b.Count
	}
	if !anyMeasured {
		return points, nil
	}
	return points, total
}

// previousTotal folds the previous span's buckets into one number, or nil when
// the store does not reach the whole span: comparing this range against a span
// that was only partly stored would print a rise nobody measured.
func previousTotal(rows []pgstore.Bucket, from time.Time, span time.Duration, o *oldest) any {
	if o != nil && (!o.found || from.Add(-span).Before(o.at)) {
		return nil
	}
	var previous int64
	for _, b := range rows {
		previous += b.Count
	}
	return previous
}

// logsFilter reads the widget's `where` into the builder's filter. Unknown
// keys are ignored: a board saved against a newer front must still draw.
func logsFilter(where map[string]string) query.SeriesFilter {
	var f query.SeriesFilter
	if v, ok := where["service"]; ok {
		service := v
		f.Service = &service
	}
	f.Level = where["level"]
	f.Search = where["q"]
	if v, ok := where["fingerprint"]; ok {
		// The catalog serialises the uint64 as decimal text; this is the exact
		// inverse of the int64 wrap InsertLogs stores it with.
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			fp := int64(n)
			f.Fingerprint = &fp
		}
	}
	for k, v := range where {
		if key, ok := strings.CutPrefix(k, "attr."); ok && key != "" {
			if f.Attrs == nil {
				f.Attrs = map[string]string{}
			}
			f.Attrs[key] = v
		}
	}
	return f
}

// logsSeries reads the hourly rollup when the range steps in whole hours and
// the filter is one the rollup's key can answer; a substring or attribute
// filter has to see the lines themselves, so those keep the ring at every
// range. Both paths answer the same question, and both draw nothing where the
// store held nothing rather than a zero.
func (h *writeAPI) logsSeries(ctx context.Context, qb *query.QueryBuilder, q seriesQuery, from, to time.Time, r seriesRange, memo *oldestMemo) ([]any, any, any, error) {
	f := logsFilter(q.Where)
	span := to.Sub(from)
	fromRollup := readsRollup(r, f)
	o, err := memo.of(ctx, fromRollup)
	if err != nil {
		return nil, nil, nil, err
	}
	read := func(a, b time.Time, step int) ([]pgstore.Bucket, error) {
		if fromRollup {
			if h.pgs == nil {
				return nil, nil
			}
			return h.pgs.HistoryBuckets(ctx, memo.tenantID, memo.projectID, a, b, step, f.Service, f.Level, f.Fingerprint)
		}
		return h.runSeriesBuckets(ctx, qb.SeriesBuckets(query.Range{From: a, To: b}, step, f))
	}
	rows, err := read(from, to, r.step)
	if err != nil {
		return nil, nil, nil, err
	}
	points, total := countPoints(rows, r, from, o)
	// The previous span is the same query at one bucket per span: same
	// predicate, so the comparison cannot be against a different question.
	prev, err := read(from.Add(-span), from, int(span/time.Second))
	if err != nil {
		return nil, nil, nil, err
	}
	return points, total, previousTotal(prev, from, span, o), nil
}

// runSeriesBuckets executes a ring bucket query. Every way the read can fail —
// the query, one row's scan, a connection torn mid-stream — is an error: an
// unfinished read rendered as zeros is a measurement nobody took.
func (h *writeAPI) runSeriesBuckets(ctx context.Context, lq query.LogQuery) ([]pgstore.Bucket, error) {
	if h.pgs == nil {
		return nil, nil
	}
	if lq.SQL == "" {
		return nil, fmt.Errorf("series query could not be built")
	}
	rows, err := h.pgs.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pgstore.Bucket
	for rows.Next() {
		var b pgstore.Bucket
		if err := rows.Scan(&b.Index, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (h *writeAPI) eventSeries(ctx context.Context, tenantID, projectID int64, q seriesQuery, from, to time.Time, r seriesRange) ([]any, any, any, error) {
	if h.pgs == nil {
		return nil, nil, nil, nil
	}
	rows, err := h.pgs.EventBuckets(ctx, tenantID, projectID, q.Name, from, to, r.step)
	if err != nil {
		return nil, nil, nil, err
	}
	// Events have no depth floor: the table is never displaced, so it holds
	// every event since the project's first, and an empty bucket is a measured 0.
	points, total := countPoints(rows, r, from, nil)
	span := to.Sub(from)
	prevRows, err := h.pgs.EventBuckets(ctx, tenantID, projectID, q.Name, from.Add(-span), from, int(span/time.Second))
	if err != nil {
		return nil, nil, nil, err
	}
	var previous int64
	for _, b := range prevRows {
		previous += b.Count
	}
	return points, total, previous, nil
}

// checkNames is what the check source reads: the uptime share, the whole
// response time, and the four phases a probe measures.
var checkNames = map[string]bool{
	"response": true, "uptime": true, "dns": true, "tcp": true, "tls": true, "wait": true,
}

// checkSeries answers `response` (the average response time of the ok probes),
// `uptime` (their share) or one of the four phases. An unknown check or an
// unknown name answers a null series rather than an error: the widget names
// something that used to exist, and a chart saying "no data" is the honest
// reading.
func (h *writeAPI) checkSeries(ctx context.Context, tenantID int64, q seriesQuery, from, to time.Time, r seriesRange) ([]any, any, any, error) {
	if h.pgs == nil || !checkNames[q.Name] {
		return nil, nil, nil, nil
	}
	mon, err := h.pool.Queries().GetMonitorByPublicID(ctx, sqlc.GetMonitorByPublicIDParams{
		PublicID: parseUUID(q.Where["check"]), TenantID: tenantID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The widget names a check that is gone: a null series says so. A read
		// that broke on the way to that answer does not.
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	rows, err := h.pgs.CheckBuckets(ctx, tenantID, mon.ID, from, to, r.step)
	if err != nil {
		return nil, nil, nil, err
	}
	points := make([]any, r.buckets)
	span := to.Sub(from)
	var whole pgstore.CheckBucket
	for _, b := range rows {
		if b.Index < 0 || b.Index >= int64(r.buckets) {
			continue
		}
		points[b.Index] = checkValue(q.Name, b)
		accCheck(&whole, b)
	}
	prevRows, err := h.pgs.CheckBuckets(ctx, tenantID, mon.ID, from.Add(-span), from, int(span/time.Second))
	if err != nil {
		return nil, nil, nil, err
	}
	var prev pgstore.CheckBucket
	for _, b := range prevRows {
		accCheck(&prev, b)
	}
	return points, checkValue(q.Name, whole), checkValue(q.Name, prev), nil
}

// accCheck adds one bucket into a running whole, so a single bucket's reading
// and the range's share one arithmetic.
func accCheck(dst *pgstore.CheckBucket, b pgstore.CheckBucket) {
	dst.SumMs += b.SumMs
	dst.OK += b.OK
	dst.Total += b.Total
	dst.SumDNSMs += b.SumDNSMs
	dst.SumConnectMs += b.SumConnectMs
	dst.SumTLSMs += b.SumTLSMs
	dst.SumTTFBMs += b.SumTTFBMs
	dst.NDNS += b.NDNS
	dst.NConnect += b.NConnect
	dst.NTLS += b.NTLS
	dst.NTTFB += b.NTTFB
}

// checkValue is the one place a check's readings are computed, so a bucket
// and a total can never be different arithmetic. nil means "no probe ran"
// (for a phase: no probe that measured it — a reused connection records no
// lookup and no connect), which is not the same fact as 0 ms or 0% uptime.
func checkValue(name string, b pgstore.CheckBucket) any {
	switch name {
	case "uptime":
		if b.Total == 0 {
			return nil
		}
		return 100 * float64(b.OK) / float64(b.Total)
	case "response":
		if b.OK == 0 {
			return nil
		}
		return b.SumMs / float64(b.OK)
	case "dns":
		if b.NDNS == 0 {
			return nil
		}
		return b.SumDNSMs / float64(b.NDNS)
	case "tcp":
		if b.NConnect == 0 {
			return nil
		}
		return b.SumConnectMs / float64(b.NConnect)
	case "tls":
		if b.NTLS == 0 {
			return nil
		}
		return b.SumTLSMs / float64(b.NTLS)
	case "wait":
		if b.NTTFB == 0 {
			return nil
		}
		return b.SumTTFBMs / float64(b.NTTFB)
	default:
		return nil
	}
}

// metricSeries draws a metric as a gauge. The reported counters — funnels,
// tests, retentions, dimensions — read through the grouped path instead.
func (h *writeAPI) metricSeries(ctx context.Context, tenantID, projectID int64, q seriesQuery, from, to time.Time, r seriesRange) ([]any, any, any, error) {
	if h.pgs == nil {
		return nil, nil, nil, nil
	}
	rows, err := h.pgs.MetricBuckets(ctx, tenantID, projectID, q.Name, q.Where, from, to, r.step)
	if err != nil {
		return nil, nil, nil, err
	}
	points := make([]any, r.buckets)
	var total any
	for _, b := range rows {
		if b.Index < 0 || b.Index >= int64(r.buckets) {
			continue
		}
		points[b.Index] = b.Avg
		// Rows arrive in bucket order, so the last one that lands holds the
		// newest reading of the range.
		total = b.Last
	}
	v, ok, err := h.pgs.MetricLast(ctx, tenantID, projectID, q.Name, q.Where, from)
	if err != nil {
		return nil, nil, nil, err
	}
	var previous any
	if ok {
		previous = v
	}
	return points, total, previous, nil
}

// ---------------------------------------------------------------------------
// The board itself: one stored layout per project, read by any member and
// replaced whole by a login member (the writeAPI's own non-GET gate), plus
// the agent's key-authenticated doors and the proposal a key leaves behind.

// dashboardColumns is the grid the front lays widgets on; a widget that runs
// past it would be drawn clipped or wrapped, so it never gets stored.
const dashboardColumns = 12

// dashboardMaxBody caps the document. The board is a layout, not a data store,
// and 64 KB is far more than a screenful of widgets needs.
const dashboardMaxBody = 64 << 10

// emptyLayout answers a project that never saved a board. An empty board is a
// real answer and never a 404: the front reads a 404 as "this core does not
// have the endpoint yet" and falls back to the browser's own copy. The version
// is the CURRENT one, because it is the only thing this document says: it has
// no heights to be in one unit or the other, and an agent that reads an empty
// board, fills it and sends it back would otherwise return version-2 sizes
// under a version-1 label, which the front draws at twice the height asked for.
var emptyLayout = []byte(`{"version":2,"widgets":[]}`)

// writeLayout hands the stored bytes back unchanged. The column is jsonb, so
// what comes out is postgres's own spelling of the document that went in:
// re-decoding it here would cost a round trip and change nothing a reader can
// see, since key order in a JSON object means nothing.
func writeLayout(w http.ResponseWriter, layout []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(layout)
}

// writeStoredBoard answers whichever board that tenant's project holds; both
// doors that read one — the session's and the key's — differ only in how they
// resolved the project, never in what an answer looks like.
func (h *writeAPI) writeStoredBoard(w http.ResponseWriter, ctx context.Context, tenantID, projectID int64) {
	row, err := h.pool.Queries().GetDashboard(ctx, sqlc.GetDashboardParams{
		TenantID: tenantID, ProjectID: projectID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeLayout(w, emptyLayout)
	case err != nil:
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
	default:
		writeLayout(w, row.Layout)
	}
}

func (h *writeAPI) getDashboard(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	projectID := h.currentProject(ctx, r, tenantID)
	if projectID == 0 {
		writeLayout(w, emptyLayout)
		return
	}
	h.writeStoredBoard(w, ctx, tenantID, projectID)
}

// presentedKey reads an ingest key off the request. Header or bearer only: a key in a
// query string lands in every access log between here and the caller, and the body of
// this particular request is the layout.
func presentedKey(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("X-Upcontrol-Key")); k != "" {
		return k
	}
	if k := r.Header.Get("Authorization"); strings.HasPrefix(k, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(k, "Bearer "))
	}
	return ""
}

// resolveAgentKey is the shared front door of the key-authenticated board
// handlers. An unwired resolver or an unusable key refuses rather than
// panics: a missing dependency must cost a caller a 401, never the process.
func (h *writeAPI) resolveAgentKey(w http.ResponseWriter, r *http.Request) (ingest.Tenant, bool) {
	if h.keys == nil {
		writeAPIErr(w, http.StatusUnauthorized, "bad_key")
		return ingest.Tenant{}, false
	}
	tenant, err := h.keys.Resolve(r.Context(), presentedKey(r))
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "bad_key")
		return ingest.Tenant{}, false
	}
	return tenant, true
}

// putAgentDashboard is the key's replace door. The narrowness it keeps is no
// longer "a board exists" but "a human made this board": the key may freely
// replace what the key itself wrote, and a curated board is never overwritten
// by a credential that lives in `.env` on every deployed server. Writing over
// a curated board is not a refusal — the offered layout is kept as a proposal
// (202) that one click in the app applies or drops.
func (h *writeAPI) putAgentDashboard(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.resolveAgentKey(w, r)
	if !ok {
		return
	}
	doc, ok := readLayout(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	stored, err := json.Marshal(doc)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	row, err := h.pool.Queries().GetDashboard(ctx, sqlc.GetDashboardParams{
		TenantID: tenant.TenantID, ProjectID: tenant.ProjectID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows), err == nil && row.WrittenBy == "key":
		// No board yet, or the key's own: either way this write replaces it.
	case err != nil:
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	default:
		// A human curated this board. The layout is kept as a proposal, not
		// refused: one click in the app applies it.
		if err := h.pool.Queries().ProposeDashboard(ctx, sqlc.ProposeDashboardParams{
			TenantID: tenant.TenantID, ProjectID: tenant.ProjectID, Proposed: stored,
		}); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "write_failed")
			return
		}
		writeAPIJSON(w, http.StatusAccepted, map[string]any{"status": "proposed"})
		return
	}
	if err := h.pool.Queries().PutDashboard(ctx, sqlc.PutDashboardParams{
		TenantID: tenant.TenantID, ProjectID: tenant.ProjectID, Layout: stored, WrittenBy: "key",
	}); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	writeLayout(w, stored)
}

// getAgentDashboard is the key's one read: the board document and nothing
// else. The catalog stays session-only — it reports what actually arrived,
// including attribute values, and the agent builds from what it declared.
func (h *writeAPI) getAgentDashboard(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.resolveAgentKey(w, r)
	if !ok {
		return
	}
	h.writeStoredBoard(w, r.Context(), tenant.TenantID, tenant.ProjectID)
}

// appendDashboardWidgets is the key's additive door. Append is deliberately
// allowed on a curated board: it removes nothing, and an unwanted card is one
// click away — that is why it needs no confirmation where a replace becomes a
// proposal. Appending keeps the board's provenance exactly as it was.
func (h *writeAPI) appendDashboardWidgets(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.resolveAgentKey(w, r)
	if !ok {
		return
	}
	// readLayout's own discipline, on a body of widgets rather than a layout:
	// the tighter reader wraps the body first, so decodeStrict's own limit
	// never applies here.
	r.Body = http.MaxBytesReader(w, r.Body, dashboardMaxBody)
	var req struct {
		Widgets []apigen.DashboardWidget `json:"widgets"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	ctx := r.Context()
	row, err := h.pool.Queries().GetDashboard(ctx, sqlc.GetDashboardParams{
		TenantID: tenant.TenantID, ProjectID: tenant.ProjectID,
	})
	var doc apigen.DashboardLayout
	hadRow := false
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No board yet: the incoming widgets become the whole board, in the
		// current unit the front saves in.
		doc = apigen.DashboardLayout{Version: apigen.N2}
	case err != nil:
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	default:
		if err := json.Unmarshal(row.Layout, &doc); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "read_failed")
			return
		}
		hadRow = true
	}
	// The agent writes in the current unit, so appending onto a board still
	// stored in the older one would silently draw its cards at twice the
	// height asked for. The server does not convert between units — `h` is a
	// drawing decision and the front is what draws — so it says so instead.
	if doc.Version != apigen.N2 {
		writeAPIErrMsg(w, http.StatusBadRequest, "bad_layout",
			"this board is stored in the older row unit; open it in the app and save it once, then append")
		return
	}
	// The merge: y0 is the board's bottom (largest y + h, 0 for an empty
	// board); every incoming widget keeps its x, w and h and lands at y + y0,
	// so the agent's block arrives whole below everything already there and
	// can never collide with it. An id the board already holds is re-minted —
	// ids are opaque, and a collision must not cost the agent a round trip.
	occupied := make(map[string]bool, len(doc.Widgets))
	y0 := 0
	for _, wd := range doc.Widgets {
		occupied[wd.Id] = true
		if bottom := wd.Y + wd.H; bottom > y0 {
			y0 = bottom
		}
	}
	for _, wd := range req.Widgets {
		if occupied[wd.Id] {
			wd.Id = mintWidgetID(occupied)
		}
		occupied[wd.Id] = true
		wd.Y += y0
		doc.Widgets = append(doc.Widgets, wd)
	}
	// The MERGED document is what gets validated and size-checked: the pieces
	// were each fine on their own many times over.
	if reason := validateLayout(doc); reason != "" {
		writeAPIErrMsg(w, http.StatusBadRequest, "bad_layout", reason)
		return
	}
	stored, err := json.Marshal(doc)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	if len(stored) > dashboardMaxBody {
		writeAPIErrMsg(w, http.StatusBadRequest, "bad_layout",
			fmt.Sprintf("the merged board would pass the %d-byte cap", dashboardMaxBody))
		return
	}
	// Two different writes, because they say two different things. An existing
	// board takes the layout alone: appending keeps its provenance, and it does
	// NOT resolve a pending proposal — only the reader resolves an offer the
	// agent made them, and a second agent run must not delete the first one's
	// unseen. A project with no board is this key laying one down.
	var writeErr error
	if hadRow {
		writeErr = h.pool.Queries().AppendDashboardLayout(ctx, sqlc.AppendDashboardLayoutParams{
			TenantID: tenant.TenantID, ProjectID: tenant.ProjectID, Layout: stored,
		})
	} else {
		writeErr = h.pool.Queries().PutDashboard(ctx, sqlc.PutDashboardParams{
			TenantID: tenant.TenantID, ProjectID: tenant.ProjectID, Layout: stored, WrittenBy: "key",
		})
	}
	if writeErr != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	writeLayout(w, stored)
}

// mintWidgetID answers a short id no widget on this board holds.
func mintWidgetID(occupied map[string]bool) string {
	for {
		id := "w_" + randomHex()[:8]
		if !occupied[id] {
			return id
		}
	}
}

// getDashboardProposal answers the one pending proposal, if any. A notify
// member may read it, like every other GET.
func (h *writeAPI) getDashboardProposal(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	projectID := h.currentProject(ctx, r, tenantID)
	if projectID == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_proposal")
		return
	}
	proposed, err := h.pool.Queries().GetDashboardProposal(ctx, sqlc.GetDashboardProposalParams{
		TenantID: tenantID, ProjectID: projectID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	// The column is nullable and a NULL arrives as a nil slice: that is "no
	// proposal", which is not the same fact as an empty one.
	if len(proposed) == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_proposal")
		return
	}
	writeLayout(w, proposed)
}

// clearDashboardProposal drops the pending proposal without applying it. A
// write, so it rides the same login gate every other mutation does.
func (h *writeAPI) clearDashboardProposal(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	if err := h.pool.Queries().ClearDashboardProposal(ctx, sqlc.ClearDashboardProposalParams{
		TenantID: tenantID, ProjectID: h.currentProject(ctx, r, tenantID),
	}); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readLayout is the one way a layout enters this server, whichever door it came
// through: two paths validating a stored document differently is how one of them
// eventually accepts what the other refuses.
func readLayout(w http.ResponseWriter, r *http.Request) (apigen.DashboardLayout, bool) {
	// The tighter reader wraps the body first, so it is the one that fires;
	// decodeStrict's own 1 MB limit sits outside it and never applies here.
	r.Body = http.MaxBytesReader(w, r.Body, dashboardMaxBody)
	var doc apigen.DashboardLayout
	if !decodeStrict(w, r, &doc) {
		return doc, false
	}
	if reason := validateLayout(doc); reason != "" {
		writeAPIErrMsg(w, http.StatusBadRequest, "bad_layout", reason)
		return doc, false
	}
	return doc, true
}

func (h *writeAPI) putDashboard(w http.ResponseWriter, r *http.Request, tenantID int64) {
	doc, ok := readLayout(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	projectID := h.currentProject(ctx, r, tenantID)
	if projectID == 0 {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	stored, err := json.Marshal(doc)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	// A session save is a human's curation by definition: provenance follows
	// the door, and this is the door that makes the key treat the board as
	// curated from here on.
	if err := h.pool.Queries().PutDashboard(ctx, sqlc.PutDashboardParams{
		TenantID: tenantID, ProjectID: projectID, Layout: stored, WrittenBy: "session",
	}); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	writeLayout(w, stored)
}

// validateLayout checks the envelope and nothing inside it: a widget's refs
// are the front's to interpret, and a server that second-guessed them would
// refuse boards a newer front draws fine. "" means the layout may be stored.
// Every enum is asked through the generated Valid(), so the contract stays the
// one list and this file cannot drift from it.
func validateLayout(doc apigen.DashboardLayout) string {
	if !doc.Version.Valid() {
		return fmt.Sprintf("version %d is not a layout this server stores", int(doc.Version))
	}
	seen := make(map[string]bool, len(doc.Widgets))
	for _, wd := range doc.Widgets {
		switch {
		case wd.Id == "":
			return "every widget needs an id"
		case seen[wd.Id]:
			return fmt.Sprintf("widget id %q appears twice", wd.Id)
		case !wd.Kind.Valid():
			return fmt.Sprintf("widget %q has an unknown kind %q", wd.Id, string(wd.Kind))
		case wd.Range != nil && !wd.Range.Valid():
			return fmt.Sprintf("widget %q has an unknown range %q", wd.Id, string(*wd.Range))
		case wd.X < 0 || wd.Y < 0:
			return fmt.Sprintf("widget %q sits off the grid at %d,%d", wd.Id, wd.X, wd.Y)
		case wd.W < 1 || wd.H < 1:
			return fmt.Sprintf("widget %q has no size", wd.Id)
		case wd.X+wd.W > dashboardColumns:
			return fmt.Sprintf("widget %q runs past the %d columns", wd.Id, dashboardColumns)
		}
		seen[wd.Id] = true
		for _, m := range wd.Metrics {
			if !m.Source.Valid() {
				return fmt.Sprintf("widget %q reads an unknown source %q", wd.Id, string(m.Source))
			}
		}
	}
	return ""
}
