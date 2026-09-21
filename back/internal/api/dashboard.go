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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	apigen "go.upcontrol.io/back/gen/api"
	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/ingest"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/pg"
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
	// The event field pass expands every event into one row per label key, so
	// it reads the newest rows rather than the whole week.
	catalogEventScan      = 20000
	catalogFieldsPerEvent = 10
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
	// The widest query decides, so the one refusal names the plan that lifts
	// the whole batch rather than the first rung on the way there.
	days := 0
	for _, q := range qs {
		if d := rangeDays(seriesRanges[q.Range]); d > days {
			days = d
		}
	}
	return historyRefusalDays(ctx, h.pool, tenantID, days)
}

// historyRefusalDays is that wall for a read reaching `days` back: the series
// batch's widest query, or a heatmap range.
func historyRefusalDays(ctx context.Context, pool *pg.Pool, tenantID int64, days int) (msg, plan string, err error) {
	tenantPlan, _ := pool.Queries().GetTenantPlan(ctx, tenantID)
	if tenantPlan == "" {
		tenantPlan = "Free"
	}
	ent, err := pool.Queries().GetPlanEntitlement(ctx, tenantPlan)
	if err != nil {
		return "", "", err
	}
	if ent.HistoryDays == nil {
		return "", "", nil // NULL = unlimited
	}
	if days <= int(*ent.HistoryDays) {
		return "", "", nil
	}
	lift := cheapestPlan(ctx, pool, func(e sqlc.PlanEntitlement) bool {
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
	// Count is what a one-key people fold counts: "" and "people" are distinct
	// actors, "events" is rows. A dimension nobody is behind — a delivery
	// outcome, an HTTP status — has no actors at all, and every people read
	// filters them out, so without this such a card reads 0 and says nothing
	// about why.
	Count string `json:"count"`
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
			// A group key is a label key in both sources; the SDK lets a
			// people one run to 60, a metric one to 40.
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
				// Breakdown (one key) or an A/B test (two keys, the arm and
				// the stat label keys); `name` is the event name in both.
				if q.Name == "" {
					return "a people group query needs a name in " + q.ID
				}
			default:
				return "a people query needs steps, a cohort, or a group in " + q.ID
			}
		}
		// A fold of one label may count rows instead of actors; nothing else may.
		// A funnel, a retention grid and an A/B test ARE people by definition, and
		// a conversion rate over rows is not a rate.
		switch q.Count {
		case "", "people":
		case "events":
			if q.Source != "people" || len(q.Group) != 1 {
				return "count is for a people query folding one label key in " + q.ID
			}
		default:
			return "unknown count " + q.Count + " in " + q.ID
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
	fields, err := h.pgs.CatalogEventFields(ctx, tenantID, projectID, since, catalogEventScan, catalogFieldsPerEvent)
	if err != nil {
		return nil, err
	}
	for _, e := range rows {
		// An event that carried no field gets an empty list, never null: an
		// empty list is a real answer.
		keys := fields[e.Name]
		if keys == nil {
			keys = []string{}
		}
		out = append(out, map[string]any{
			"name":   e.Name,
			"times":  e.Times,
			"lastTs": e.LastTS.UTC().Format(time.RFC3339),
			"fields": keys,
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
// one event name to one label's values (breakdown); a group of two folds one
// event name by its arm and stat label keys (an A/B test). In both grouped
// shapes `name` is the event name.
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
			sums, err = h.pgs.BreakdownValues(ctx, tenantID, projectID, q.Name, q.Group[0], q.Count == "events", from, to)
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
	// The web door's page views are the exception: history-trim expires them
	// past the web depth, so they read like the rollup, floored at the oldest
	// one kept.
	var o *oldest
	if q.Name == "uc.pageview" {
		at, found, err := h.pgs.OldestPageview(ctx, tenantID, projectID)
		if err != nil {
			return nil, nil, nil, err
		}
		o = &oldest{at: at, found: found}
	}
	points, total := countPoints(rows, r, from, o)
	span := to.Sub(from)
	prevRows, err := h.pgs.EventBuckets(ctx, tenantID, projectID, q.Name, from.Add(-span), from, int(span/time.Second))
	if err != nil {
		return nil, nil, nil, err
	}
	return points, total, previousTotal(prevRows, from, span, o), nil
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
// The boards: a project holds several named ones, each a layout stored whole,
// read by any member and replaced by a login member (the writeAPI's own
// non-GET gate), plus the agent's key-authenticated doors and the proposal a
// key leaves behind. /v1/dashboard and its two sub-paths stay for good as the
// alias of the oldest board: a CLI already on somebody's machine knows no
// other path.

// dashboardColumns is the grid the front lays widgets on; a widget that runs
// past it would be drawn clipped or wrapped, so it never gets stored.
const dashboardColumns = 12

// dashboardMaxBody caps ONE board's document. The board is a layout, not a
// data store, and 64 KB is far more than a screenful of widgets needs.
const dashboardMaxBody = 64 << 10

// mainBoard is the alias every {id} position accepts: the project's oldest
// board. The alias outranks a board that happens to be NAMED main, or a
// rename would take the agent's one handle away from the board it has been
// writing since before a project could hold two.
const mainBoard = "main"

// firstBoardName is what that oldest board is called when nothing named it:
// the migration's default, and the name the alias materialises under.
const firstBoardName = "Main"

// maxBoardName is the contract's own ceiling, in runes: a name is a tab's
// label, not a document.
const maxBoardName = 40

// maxSaveBoards bounds one atomic save. A person saves the boards they
// changed, and a widget dragged from one board onto another is the whole
// reason a save ever carries more than one.
const maxSaveBoards = 16

// emptyLayout answers a board that was never stored. An empty board is a real
// answer and never a 404 on the alias: the front reads a 404 there as "this
// core does not have the endpoint yet". The version is the CURRENT one,
// because it is the only thing this document says: it has no heights to be in
// one unit or the other, and an agent that reads an empty board, fills it and
// sends it back would otherwise return version-2 sizes under a version-1
// label, which the front draws at twice the height asked for.
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

// board is one stored board as every door needs it: its identity, who curated
// it, the document, the offer waiting on it, and the rank the freeze counts
// in (1 is the alias, and the alias is never frozen).
type board struct {
	id        int64
	publicID  pgtype.UUID
	name      string
	writtenBy string
	layout    []byte
	proposed  []byte
	rank      int64
}

// boardDoor is what a board request acts on: the caller's tenant and project,
// and which door they came through. Provenance follows the door, three of the
// acts are a session's alone, and everything else the two doors do is the
// same code answering the same way.
type boardDoor struct {
	tenantID  int64
	projectID int64
	session   bool
}

// sessionBoards is the session door: the current project of the caller's
// workspace. 0 means they reach no project here, which the reads render as an
// empty board and every write as a 404.
func (h *writeAPI) sessionBoards(r *http.Request, tenantID int64) boardDoor {
	return boardDoor{tenantID: tenantID, projectID: h.currentProject(r.Context(), r, tenantID), session: true}
}

// keyBoards is the agent's door: the project is the key's own. It answers the
// request itself (401) when the presented key is not one that may reach a
// board at all.
func (h *writeAPI) keyBoards(w http.ResponseWriter, r *http.Request) (boardDoor, bool) {
	tenant, ok := h.resolveAgentKey(w, r)
	if !ok {
		return boardDoor{}, false
	}
	return boardDoor{tenantID: tenant.TenantID, projectID: tenant.ProjectID}, true
}

// resolveBoardQ is the one way a board id becomes a row, and it cannot lie:
// the lookup carries the caller's tenant AND project, so an id from another
// workspace, or from a sibling project of the same workspace, resolves to
// nothing exactly like an id that never existed. false means "no such board
// here" — every door turns that into a 404, except on the alias, which is
// allowed not to exist yet. q so a caller inside a transaction resolves what
// it has already written (the atomic save).
func resolveBoardQ(ctx context.Context, q *sqlc.Queries, tenantID, projectID int64, raw string) (board, bool, error) {
	row, err := q.ResolveBoard(ctx, sqlc.ResolveBoardParams{
		TenantID: tenantID, ProjectID: projectID,
		Alias: raw == mainBoard, PublicID: parseUUID(raw),
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return board{}, false, nil
	case err != nil:
		return board{}, false, err
	}
	return board{
		id: row.ID, publicID: row.PublicID, name: row.Name, writtenBy: row.WrittenBy,
		layout: row.Layout, proposed: row.Proposed, rank: row.Rank,
	}, true, nil
}

func (h *writeAPI) resolveBoard(ctx context.Context, d boardDoor, raw string) (board, bool, error) {
	return resolveBoardQ(ctx, h.pool.Queries(), d.tenantID, d.projectID, raw)
}

// boardPlan answers the workspace's plan and how many boards ONE of its
// projects may hold. nil is unlimited (Self-hosted), the contract every axis
// in the entitlement table carries; a missing tenant plan reads as Free, like
// every other wall here.
func boardPlan(ctx context.Context, pool *pg.Pool, tenantID int64) (string, *int, error) {
	plan, _ := pool.Queries().GetTenantPlan(ctx, tenantID)
	if plan == "" {
		plan = "Free"
	}
	ent, err := pool.Queries().GetPlanEntitlement(ctx, plan)
	if err != nil {
		return plan, nil, err
	}
	if ent.Dashboards == nil {
		return plan, nil, nil
	}
	limit := int(*ent.Dashboards)
	return plan, &limit, nil
}

// boardsReason is the create wall's copy, and the front spells it identically.
// The sentence is built from the entitlement rows rather than typed as a
// literal, the way historyReason is: `lift` is the cheapest plan that answers
// the ask, "" when the ladder holds nothing above the caller.
func boardsReason(plan string, carries int, lift string, liftMax int) string {
	if lift == "" {
		return plan + " carries " + strconv.Itoa(carries) + " dashboards per project."
	}
	noun := " dashboards"
	if carries == 1 {
		noun = " dashboard"
	}
	return "Your plan carries " + strconv.Itoa(carries) + noun + " per project. " +
		lift + " carries " + strconv.Itoa(liftMax) + "."
}

// frozenReason is the frozen wall's copy. The board is kept either way; with
// nothing above the caller to buy, saying that IS the whole sentence.
func frozenReason(lift string, liftMax int) string {
	if lift == "" {
		return "This dashboard is kept, not running."
	}
	return "This dashboard is kept, not running. " + lift + " carries " +
		strconv.Itoa(liftMax) + " per project."
}

// boardsLift names the cheapest ladder plan whose board count satisfies fits,
// and how many that plan carries — read off the row cheapestPlan accepted, so
// the answer costs one query rather than two. The two walls ask different
// questions of the same table, so the predicate belongs to the caller.
func boardsLift(ctx context.Context, pool *pg.Pool, fits func(max int) bool) (string, int) {
	liftMax := 0
	lift := cheapestPlan(ctx, pool, func(e sqlc.PlanEntitlement) bool {
		if e.Dashboards == nil || !fits(int(*e.Dashboards)) {
			return false
		}
		liftMax = int(*e.Dashboards)
		return true
	})
	return lift, liftMax
}

// writeBoardWall answers the create wall: the cheapest plan with room for one
// MORE board. The projects axis learned this distinction first — the create
// wall asks for room, the frozen wall asks for a plan that carries what is
// already there. Two numbers, because after a downgrade they differ: room is
// measured against what the project HOLDS, the sentence states what the plan
// CARRIES, and printing the first as the second quotes Free as carrying three.
func writeBoardWall(ctx context.Context, w http.ResponseWriter, pool *pg.Pool, plan string, carries, have int) {
	lift, liftMax := boardsLift(ctx, pool, func(max int) bool { return max > have })
	writeUpgradeRequired(w, boardsReason(plan, carries, lift, liftMax), strings.ToLower(lift))
}

// writeFrozenBoard answers the frozen wall, modelled on writeFrozen: the plan
// it names CARRIES every board the project holds (>=, not room for one more),
// because the ask is to run what is there.
func writeFrozenBoard(ctx context.Context, w http.ResponseWriter, pool *pg.Pool, have int) {
	lift, liftMax := boardsLift(ctx, pool, func(max int) bool { return max >= have })
	writeUpgradeRequired(w, frozenReason(lift, liftMax), strings.ToLower(lift))
}

// frozenWall counts what the project holds and words the 402 about it; a count
// that broke is the 500 instead. Either way the request has been answered when
// it returns. q, so the atomic save counts inside its own transaction.
func (h *writeAPI) frozenWall(ctx context.Context, w http.ResponseWriter, q *sqlc.Queries, d boardDoor) {
	have, err := q.CountBoards(ctx, sqlc.CountBoardsParams{
		TenantID: d.tenantID, ProjectID: d.projectID,
	})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	writeFrozenBoard(ctx, w, h.pool, int(have))
}

// boardRuns answers the frozen wall itself and reports false when it did. A
// board past the plan's count is kept whole and reached by nobody — on either
// door, reading or writing — until a plan carries it again. Deleting one
// stays open: the owner chooses what to keep.
func (h *writeAPI) boardRuns(ctx context.Context, w http.ResponseWriter, d boardDoor, b board) bool {
	_, limit, err := boardPlan(ctx, h.pool, d.tenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return false
	}
	if limit == nil || b.rank <= int64(*limit) {
		return true
	}
	h.frozenWall(ctx, w, h.pool.Queries(), d)
	return false
}

// unknownBoard is the one answer an id nobody reaches gets, whatever the verb:
// a stranger's board, a sibling project's and one that never existed all read
// alike, so the door confirms nothing about which ids exist.
func unknownBoard(w http.ResponseWriter) {
	writeAPIErrMsg(w, http.StatusNotFound, "unknown_board", "this project has no such dashboard")
}

// knownBoard is the preamble every by-id door shares: the board resolved, the
// 500 on a read that broke, and the 404 an id nobody reaches gets. ok false
// means the request has been answered. found false means the ALIAS is not
// stored yet, which is allowed and which every door words for itself.
func (h *writeAPI) knownBoard(ctx context.Context, w http.ResponseWriter, d boardDoor, raw string) (board, bool, bool) {
	b, found, err := h.resolveBoard(ctx, d, raw)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return board{}, false, false
	}
	if !found && raw != mainBoard {
		unknownBoard(w)
		return board{}, false, false
	}
	return b, found, true
}

// liveBoard is that preamble plus the frozen wall, which is what every door
// but one wants. deleteBoard is the exception and stays on knownBoard: the
// owner chooses what to keep.
func (h *writeAPI) liveBoard(ctx context.Context, w http.ResponseWriter, d boardDoor, raw string) (board, bool, bool) {
	b, found, ok := h.knownBoard(ctx, w, d, raw)
	if !ok || !found {
		return b, found, ok
	}
	if !h.boardRuns(ctx, w, d, b) {
		return board{}, false, false
	}
	return b, true, true
}

// layAlias materialises the alias: the project's own Main, laid down by
// whichever door got there first, taking over a row of that name an earlier
// tenant of a released and re-claimed project left behind. q, so the atomic
// save lays it down inside its own transaction.
func layAlias(ctx context.Context, q *sqlc.Queries, d boardDoor, layout []byte, writtenBy string) (sqlc.UpsertBoardRow, error) {
	return q.UpsertBoard(ctx, sqlc.UpsertBoardParams{
		TenantID: d.tenantID, ProjectID: d.projectID, Name: firstBoardName,
		Layout: layout, WrittenBy: writtenBy,
	})
}

// boardPathID is the {id} of a per-board path, with the sub-path taken off
// first: the last segment of /v1/dashboards/{id}/proposal is the sub-path,
// never the board.
func boardPathID(path string) string {
	return pathLast(strings.TrimSuffix(strings.TrimSuffix(path, "/proposal"), "/widgets"))
}

// boardName trims and checks a name the caller typed. "" means the door has
// already answered.
func boardName(w http.ResponseWriter, raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || len([]rune(name)) > maxBoardName {
		writeAPIErrMsg(w, http.StatusBadRequest, "bad_name",
			fmt.Sprintf("a dashboard's name is 1 to %d characters", maxBoardName))
		return "", false
	}
	return name, true
}

// listBoards answers GET /v1/dashboards on both doors: what the project holds,
// without the layouts. A project that never saved one still lists a board —
// the synthetic `main` — because the alias always names something, and a
// front with no tab to draw would have nowhere to put the `+`.
func (h *writeAPI) listBoards(w http.ResponseWriter, r *http.Request, d boardDoor) {
	ctx := r.Context()
	_, limit, err := boardPlan(ctx, h.pool, d.tenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	var rows []sqlc.ListBoardsRow
	if d.projectID != 0 {
		rows, err = h.pool.Queries().ListBoards(ctx, sqlc.ListBoardsParams{
			TenantID: d.tenantID, ProjectID: d.projectID,
		})
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "read_failed")
			return
		}
	}
	boards := make([]map[string]any, 0, len(rows)+1)
	for _, row := range rows {
		boards = append(boards, map[string]any{
			"id":        uuidStr(row.PublicID),
			"name":      row.Name,
			"frozen":    limit != nil && row.Rank > int64(*limit),
			"writtenBy": row.WrittenBy,
			"widgets":   int(row.Widgets),
			"proposed":  row.Proposed,
		})
	}
	if len(boards) == 0 {
		// Nothing stored yet. The alias is a board all the same, and it is
		// nobody's curation until somebody saves it, so the key may write it.
		boards = append(boards, map[string]any{
			"id": mainBoard, "name": firstBoardName, "frozen": false,
			"writtenBy": "key", "widgets": 0, "proposed": false,
		})
	}
	resp := map[string]any{"boards": boards}
	// Absent when the plan is unlimited: what nothing caps is not a number to
	// print, the same silence every other axis keeps.
	if limit != nil {
		resp["max"] = *limit
	}
	writeAPIJSON(w, http.StatusOK, resp)
}

// createBoard answers POST /v1/dashboards on both doors. Both create freely up
// to the plan's count; past it the answer is the 402. No lock stands behind
// the count: two creates arriving together can land one board too many, and
// that board is frozen by the same rank rule a downgrade uses, which is
// exactly what computing the freeze on read is for.
func (h *writeAPI) createBoard(w http.ResponseWriter, r *http.Request, d boardDoor) {
	// The first layout may ride in this body, so the board's own cap is the
	// one that applies, exactly as it does on a replace.
	r.Body = http.MaxBytesReader(w, r.Body, dashboardMaxBody)
	var req struct {
		Name   string                  `json:"name"`
		Layout *apigen.DashboardLayout `json:"layout"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	name, ok := boardName(w, req.Name)
	if !ok {
		return
	}
	layout := emptyLayout
	if req.Layout != nil {
		if reason := validateLayout(*req.Layout); reason != "" {
			writeAPIErrMsg(w, http.StatusBadRequest, "bad_layout", reason)
			return
		}
		stored, err := json.Marshal(*req.Layout)
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "write_failed")
			return
		}
		layout = stored
	}
	if d.projectID == 0 {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	ctx := r.Context()
	plan, limit, err := boardPlan(ctx, h.pool, d.tenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	count, err := h.pool.Queries().CountBoards(ctx, sqlc.CountBoardsParams{
		TenantID: d.tenantID, ProjectID: d.projectID,
	})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	// A project with nothing stored still holds one board, the alias, so the
	// cap counts what the reader sees rather than what the table holds.
	have := int(count)
	if have < 1 {
		have = 1
	}
	if limit != nil && have >= *limit {
		writeBoardWall(ctx, w, h.pool, plan, *limit, have)
		return
	}
	// The alias is the project's OLDEST board, so laying down a board somebody
	// just named would hand `main` to it and the CLI would write there from
	// then on. Materialising Main first keeps the alias where every caller
	// left it, in the current unit so the append door accepts what the server
	// itself just made.
	if count == 0 {
		if _, err := layAlias(ctx, h.pool.Queries(), d, emptyLayout, "key"); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "write_failed")
			return
		}
	}
	// A board nobody has put a widget on is nobody's curation, whichever door
	// made it: the agent asked to fill a board a person just named writes it
	// rather than parking a proposal on an empty board. A session that sends
	// widgets with the create IS curating.
	writtenBy := "key"
	if d.session && req.Layout != nil {
		writtenBy = "session"
	}
	row, err := h.pool.Queries().CreateBoard(ctx, sqlc.CreateBoardParams{
		TenantID: d.tenantID, ProjectID: d.projectID, Name: name,
		Layout: layout, WrittenBy: writtenBy,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeAPIErrMsg(w, http.StatusConflict, "name_taken",
			"this project already has a dashboard called "+name)
		return
	case err != nil:
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	writeAPIJSON(w, http.StatusCreated, map[string]any{
		"id": uuidStr(row.PublicID), "name": row.Name,
	})
}

// getBoardLayout answers one board's document on both doors. The alias is
// allowed not to exist: a project that never saved a board reads the empty
// layout, never a 404, because the front reads a 404 there as a core without
// this endpoint.
func (h *writeAPI) getBoardLayout(w http.ResponseWriter, r *http.Request, d boardDoor, raw string) {
	b, found, ok := h.liveBoard(r.Context(), w, d, raw)
	if !ok {
		return
	}
	if !found {
		writeLayout(w, emptyLayout)
		return
	}
	writeLayout(w, b.layout)
}

// putBoardLayout replaces one board, on both doors. The narrowness the key
// keeps is not "a board exists" but "a human made this board": it may freely
// replace what the key itself wrote and what nobody has curated, and a curated
// board is never overwritten by a credential that lives in `.env` on every
// deployed server. Writing over a curated board is not a refusal — the offered
// layout is kept as a proposal (202) that one click in the app applies or
// drops.
func (h *writeAPI) putBoardLayout(w http.ResponseWriter, r *http.Request, d boardDoor, raw string) {
	ctx := r.Context()
	// The board is resolved before the body is read: an unknown or a frozen
	// board is that whatever the document would have been.
	b, found, ok := h.liveBoard(ctx, w, d, raw)
	if !ok {
		return
	}
	if !found && d.projectID == 0 {
		// The alias names a board in a project, and this caller reaches none.
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	doc, ok := readLayout(w, r)
	if !ok {
		return
	}
	stored, err := json.Marshal(doc)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	if found && !d.session && b.writtenBy == "session" {
		rows, err := h.pool.Queries().ProposeBoard(ctx, sqlc.ProposeBoardParams{
			ID: b.id, TenantID: d.tenantID, Proposed: stored,
		})
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "write_failed")
			return
		}
		if rows == 0 {
			unknownBoard(w)
			return
		}
		// The id and the name so a caller that addressed the board by alias or
		// by name can point a human at the right tab.
		writeAPIJSON(w, http.StatusAccepted, map[string]any{
			"status": "proposed", "id": uuidStr(b.publicID), "name": b.name,
		})
		return
	}
	// Provenance follows the door: what a key writes stays the key's, and what
	// a session writes is a human's curation.
	writtenBy := "key"
	if d.session {
		writtenBy = "session"
	}
	if !found {
		// The alias materialises on its first write rather than answering 200
		// beside a row that is already there.
		if _, err := layAlias(ctx, h.pool.Queries(), d, stored, writtenBy); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "write_failed")
			return
		}
		writeLayout(w, stored)
		return
	}
	rows, err := h.pool.Queries().PutBoardLayout(ctx, sqlc.PutBoardLayoutParams{
		ID: b.id, TenantID: d.tenantID, Layout: stored, WrittenBy: writtenBy,
	})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	if rows == 0 {
		unknownBoard(w)
		return
	}
	writeLayout(w, stored)
}

// saveBoards answers PUT /v1/dashboards: every board one Save changed, in one
// transaction. A widget moved between two boards is two documents, and saving
// one without the other would duplicate it or lose it; one refused board
// refuses them all, which is what the rollback is for. Session only, so every
// board it writes is a human's curation.
func (h *writeAPI) saveBoards(w http.ResponseWriter, r *http.Request, tenantID int64) {
	// decodeStrict's own 1 MB limit bounds the request; each board's stored
	// document is then held to the same 64 KB a single save is, below, so the
	// rule that refuses is the per-board one and its message names the board.
	var req struct {
		Boards []struct {
			ID     string                 `json:"id"`
			Layout apigen.DashboardLayout `json:"layout"`
		} `json:"boards"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if len(req.Boards) == 0 || len(req.Boards) > maxSaveBoards {
		writeAPIErrMsg(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("a save carries 1 to %d boards, got %d", maxSaveBoards, len(req.Boards)))
		return
	}
	d := h.sessionBoards(r, tenantID)
	if d.projectID == 0 {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	ctx := r.Context()
	_, limit, err := boardPlan(ctx, h.pool, tenantID)
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
	q := h.pool.Queries().WithTx(tx)
	saved := make([]map[string]any, 0, len(req.Boards))
	for _, entry := range req.Boards {
		b, found, err := resolveBoardQ(ctx, q, d.tenantID, d.projectID, entry.ID)
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "read_failed")
			return
		}
		if !found && entry.ID != mainBoard {
			unknownBoard(w)
			return
		}
		if found && limit != nil && b.rank > int64(*limit) {
			h.frozenWall(ctx, w, q, d)
			return
		}
		if reason := validateLayout(entry.Layout); reason != "" {
			writeAPIErrMsg(w, http.StatusBadRequest, "bad_layout", entry.ID+": "+reason)
			return
		}
		stored, err := json.Marshal(entry.Layout)
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "write_failed")
			return
		}
		if len(stored) > dashboardMaxBody {
			writeAPIErrMsg(w, http.StatusBadRequest, "bad_layout",
				fmt.Sprintf("%s: the board would pass the %d-byte cap", entry.ID, dashboardMaxBody))
			return
		}
		var id string
		if !found {
			row, err := layAlias(ctx, q, d, stored, "session")
			if err != nil {
				writeAPIErr(w, http.StatusInternalServerError, "write_failed")
				return
			}
			id = uuidStr(row.PublicID)
		} else {
			rows, err := q.PutBoardLayout(ctx, sqlc.PutBoardLayoutParams{
				ID: b.id, TenantID: d.tenantID, Layout: stored, WrittenBy: "session",
			})
			if err != nil {
				writeAPIErr(w, http.StatusInternalServerError, "write_failed")
				return
			}
			if rows == 0 {
				unknownBoard(w)
				return
			}
			id = uuidStr(b.publicID)
		}
		saved = append(saved, map[string]any{"id": id, "layout": json.RawMessage(stored)})
	}
	if err := tx.Commit(ctx); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"boards": saved})
}

// renameBoard answers PATCH /v1/dashboards/{id}. Session only: a key writes
// boards, it does not name them. A name is not part of the layout, so this
// never touches provenance.
func (h *writeAPI) renameBoard(w http.ResponseWriter, r *http.Request, tenantID int64) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	name, ok := boardName(w, req.Name)
	if !ok {
		return
	}
	ctx := r.Context()
	d := h.sessionBoards(r, tenantID)
	b, found, ok := h.liveBoard(ctx, w, d, boardPathID(r.URL.Path))
	if !ok {
		return
	}
	if !found {
		// Naming the alias of a project that never stored a board is simply
		// creating that board under the name: it is the same board either way,
		// and refusing would leave the first tab unrenameable.
		if d.projectID == 0 {
			unknownBoard(w)
			return
		}
		row, err := h.pool.Queries().CreateBoard(ctx, sqlc.CreateBoardParams{
			TenantID: d.tenantID, ProjectID: d.projectID, Name: name,
			Layout: emptyLayout, WrittenBy: "key",
		})
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "write_failed")
			return
		}
		writeAPIJSON(w, http.StatusOK, map[string]any{"id": uuidStr(row.PublicID), "name": row.Name})
		return
	}
	rows, err := h.pool.Queries().RenameBoard(ctx, sqlc.RenameBoardParams{
		ID: b.id, TenantID: d.tenantID, Name: name,
	})
	if err != nil {
		// The project already holds that name: the unique index is what
		// arbitrates, so two renames racing cannot both win.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeAPIErrMsg(w, http.StatusConflict, "name_taken",
				"this project already has a dashboard called "+name)
			return
		}
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	if rows == 0 {
		unknownBoard(w)
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"id": uuidStr(b.publicID), "name": name})
}

// deleteBoard answers DELETE /v1/dashboards/{id}. Session only, and open on a
// frozen board: the owner chooses what to keep, and deleting a live one thaws
// the oldest frozen one on the next read. A project keeps its last board —
// there is always somewhere for the alias to point.
func (h *writeAPI) deleteBoard(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	d := h.sessionBoards(r, tenantID)
	b, found, ok := h.knownBoard(ctx, w, d, boardPathID(r.URL.Path))
	if !ok {
		return
	}
	if !found {
		// The alias of a project with nothing stored is the project's one
		// board: there is nothing to delete, and it would be the last anyway.
		if d.projectID != 0 {
			writeAPIErrMsg(w, http.StatusConflict, "last_board",
				"a project keeps at least one dashboard")
			return
		}
		unknownBoard(w)
		return
	}
	have, err := h.pool.Queries().CountBoards(ctx, sqlc.CountBoardsParams{
		TenantID: d.tenantID, ProjectID: d.projectID,
	})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	if have <= 1 {
		writeAPIErrMsg(w, http.StatusConflict, "last_board",
			"a project keeps at least one dashboard")
		return
	}
	rows, err := h.pool.Queries().DeleteBoard(ctx, sqlc.DeleteBoardParams{ID: b.id, TenantID: d.tenantID})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	if rows == 0 {
		unknownBoard(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	// A public key is printed in a page's source: it may name events on /i and
	// nothing else. The same answer as a key that is not ours, so the door
	// tells a stranger holding one nothing about it.
	if err != nil || tenant.Kind == ingest.KeyKindPublic {
		writeAPIErr(w, http.StatusUnauthorized, "bad_key")
		return ingest.Tenant{}, false
	}
	return tenant, true
}

// appendBoardWidgets is the key's additive door. Append is deliberately
// allowed on a curated board: it removes nothing, and an unwanted card is one
// click away — that is why it needs no confirmation where a replace becomes a
// proposal. Appending keeps the board's provenance exactly as it was.
func (h *writeAPI) appendBoardWidgets(w http.ResponseWriter, r *http.Request, d boardDoor, raw string) {
	ctx := r.Context()
	b, found, ok := h.liveBoard(ctx, w, d, raw)
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
	var doc apigen.DashboardLayout
	if found {
		if err := json.Unmarshal(b.layout, &doc); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "read_failed")
			return
		}
	} else {
		// No board yet: the incoming widgets become the whole board, in the
		// current unit the front saves in.
		doc = apigen.DashboardLayout{Version: apigen.N2}
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
	// unseen. A board that is not there yet is this key laying the alias down.
	if !found {
		if _, err := layAlias(ctx, h.pool.Queries(), d, stored, "key"); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "write_failed")
			return
		}
		writeLayout(w, stored)
		return
	}
	rows, err := h.pool.Queries().AppendBoardLayout(ctx, sqlc.AppendBoardLayoutParams{
		ID: b.id, TenantID: d.tenantID, Layout: stored,
	})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	if rows == 0 {
		unknownBoard(w)
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

// getBoardProposal answers the one proposal pending on a board, if any. A
// notify member may read it, like every other GET.
func (h *writeAPI) getBoardProposal(w http.ResponseWriter, r *http.Request, d boardDoor, raw string) {
	b, found, ok := h.liveBoard(r.Context(), w, d, raw)
	if !ok {
		return
	}
	if !found {
		writeAPIErr(w, http.StatusNotFound, "no_proposal")
		return
	}
	// The column is nullable and a NULL arrives as a nil slice: that is "no
	// proposal", which is not the same fact as an empty one.
	if len(b.proposed) == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_proposal")
		return
	}
	writeLayout(w, b.proposed)
}

// clearBoardProposal drops the pending proposal without applying it. A write,
// so it rides the same login gate every other mutation does.
func (h *writeAPI) clearBoardProposal(w http.ResponseWriter, r *http.Request, d boardDoor, raw string) {
	ctx := r.Context()
	// The read of a frozen board's proposal is walled, so the drop is too: an
	// offer nobody may look at is not one to throw away unseen.
	b, found, ok := h.liveBoard(ctx, w, d, raw)
	if !ok {
		return
	}
	if !found {
		// No board, so no offer waiting on it: the caller asked for it gone
		// and it is gone.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := h.pool.Queries().ClearBoardProposal(ctx, sqlc.ClearBoardProposalParams{
		ID: b.id, TenantID: d.tenantID,
	}); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "write_failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The session's own doors on the alias: /v1/dashboard and its two sub-paths,
// which every front and every shipped CLI still call.

func (h *writeAPI) getDashboard(w http.ResponseWriter, r *http.Request, tenantID int64) {
	h.getBoardLayout(w, r, h.sessionBoards(r, tenantID), mainBoard)
}

func (h *writeAPI) putDashboard(w http.ResponseWriter, r *http.Request, tenantID int64) {
	h.putBoardLayout(w, r, h.sessionBoards(r, tenantID), mainBoard)
}

func (h *writeAPI) getDashboardProposal(w http.ResponseWriter, r *http.Request, tenantID int64) {
	h.getBoardProposal(w, r, h.sessionBoards(r, tenantID), mainBoard)
}

func (h *writeAPI) clearDashboardProposal(w http.ResponseWriter, r *http.Request, tenantID int64) {
	h.clearBoardProposal(w, r, h.sessionBoards(r, tenantID), mainBoard)
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
		// The app drops a widget bound to nothing rather than drawing an empty card, so one
		// stored here is a card that vanishes on the next read and is erased by the next save.
		case len(wd.Metrics) == 0:
			return fmt.Sprintf("widget %q reads nothing", wd.Id)
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
