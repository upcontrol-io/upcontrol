// The board's two reads: GET /v1/dashboard/catalog (what this project actually
// sends, so a widget is picked from real names) and POST /v1/series (every
// widget's points in one round trip), and the board itself: GET and PUT
// /v1/dashboard, one stored layout per project. All of them are session-gated
// and scoped to the session's current project, like /v1/logs.

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
		case "logs", "check", "event", "metric":
		default:
			return "unknown source " + q.Source + " in " + q.ID
		}
		if _, ok := seriesRanges[q.Range]; !ok {
			return "unknown range " + q.Range + " in " + q.ID
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

func (h *writeAPI) catalogFunnels(ctx context.Context, tenantID, projectID int64, since time.Time) ([]map[string]any, error) {
	out := []map[string]any{}
	if h.pgs == nil {
		return out, nil
	}
	rows, err := h.pgs.CatalogFunnels(ctx, tenantID, projectID, since)
	if err != nil {
		return nil, err
	}
	for _, f := range rows {
		out = append(out, map[string]any{"name": f.Name, "steps": f.Steps})
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

// sumPoints is countPoints for a counter's folded increments: the database
// summed the deltas, and a bucket the counter did not move in is a measured 0
// rather than a gap.
func sumPoints(rows []pgstore.SumBucket, r seriesRange) ([]any, float64) {
	points := make([]any, r.buckets)
	var total float64
	for i := range points {
		points[i] = float64(0)
	}
	for _, b := range rows {
		if b.Index < 0 || b.Index >= int64(r.buckets) {
			continue
		}
		points[b.Index] = b.Sum
		total += b.Sum
	}
	return points, total
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

// checkSeries answers `response` (the average response time of the ok probes)
// or `uptime` (their share). An unknown check or an unknown name answers a
// null series rather than an error: the widget names something that used to
// exist, and a chart saying "no data" is the honest reading.
func (h *writeAPI) checkSeries(ctx context.Context, tenantID int64, q seriesQuery, from, to time.Time, r seriesRange) ([]any, any, any, error) {
	if h.pgs == nil || (q.Name != "response" && q.Name != "uptime") {
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
		whole.SumMs += b.SumMs
		whole.OK += b.OK
		whole.Total += b.Total
	}
	prevRows, err := h.pgs.CheckBuckets(ctx, tenantID, mon.ID, from.Add(-span), from, int(span/time.Second))
	if err != nil {
		return nil, nil, nil, err
	}
	var prev pgstore.CheckBucket
	for _, b := range prevRows {
		prev.SumMs += b.SumMs
		prev.OK += b.OK
		prev.Total += b.Total
	}
	return points, checkValue(q.Name, whole), checkValue(q.Name, prev), nil
}

// checkValue is the one place a check's two readings are computed, so a bucket
// and a total can never be different arithmetic. nil means "no probe ran",
// which is not the same fact as 0 ms or 0% uptime.
func checkValue(name string, b pgstore.CheckBucket) any {
	if name == "uptime" {
		if b.Total == 0 {
			return nil
		}
		return 100 * float64(b.OK) / float64(b.Total)
	}
	if b.OK == 0 {
		return nil
	}
	return b.SumMs / float64(b.OK)
}

// metricSeries draws a counter (the funnel's steps) as increments and every
// other metric as a gauge.
func (h *writeAPI) metricSeries(ctx context.Context, tenantID, projectID int64, q seriesQuery, from, to time.Time, r seriesRange) ([]any, any, any, error) {
	if h.pgs == nil {
		return nil, nil, nil, nil
	}
	span := to.Sub(from)
	if q.Name == "funnel" {
		// The fold runs in the database, so a year of a busy counter is a
		// grouped scan rather than a slice held in memory here.
		rows, err := h.pgs.FunnelBuckets(ctx, tenantID, projectID, q.Name, q.Where, from, to, r.step)
		if err != nil {
			return nil, nil, nil, err
		}
		points, total := sumPoints(rows, r)
		// The previous span is the same fold at one bucket per span; it seeds
		// itself from the reading before it, like the range does.
		prevRows, err := h.pgs.FunnelBuckets(ctx, tenantID, projectID, q.Name, q.Where, from.Add(-span), from, int(span/time.Second))
		if err != nil {
			return nil, nil, nil, err
		}
		var previous float64
		for _, b := range prevRows {
			previous += b.Sum
		}
		return points, total, previous, nil
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
// replaced whole by a login member (the writeAPI's own non-GET gate).

// dashboardColumns is the grid the front lays widgets on; a widget that runs
// past it would be drawn clipped or wrapped, so it never gets stored.
const dashboardColumns = 12

// dashboardMaxBody caps the document. The board is a layout, not a data store,
// and 64 KB is far more than a screenful of widgets needs.
const dashboardMaxBody = 64 << 10

// emptyLayout answers a project that never saved a board. An empty board is a
// real answer and never a 404: the front reads a 404 as "this core does not
// have the endpoint yet" and falls back to the browser's own copy.
var emptyLayout = []byte(`{"version":1,"widgets":[]}`)

// writeLayout hands the stored bytes back unchanged. The column is jsonb, so
// what comes out is postgres's own spelling of the document that went in:
// re-decoding it here would cost a round trip and change nothing a reader can
// see, since key order in a JSON object means nothing.
func writeLayout(w http.ResponseWriter, layout []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(layout)
}

func (h *writeAPI) getDashboard(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	projectID := h.currentProject(ctx, r, tenantID)
	if projectID == 0 {
		writeLayout(w, emptyLayout)
		return
	}
	layout, err := h.pool.Queries().GetDashboard(ctx, sqlc.GetDashboardParams{
		TenantID: tenantID, ProjectID: projectID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeLayout(w, emptyLayout)
	case err != nil:
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
	default:
		writeLayout(w, layout)
	}
}

func (h *writeAPI) putDashboard(w http.ResponseWriter, r *http.Request, tenantID int64) {
	// The tighter reader wraps the body first, so it is the one that fires;
	// decodeStrict's own 1 MB limit sits outside it and never applies here.
	r.Body = http.MaxBytesReader(w, r.Body, dashboardMaxBody)
	var doc apigen.DashboardLayout
	if !decodeStrict(w, r, &doc) {
		return
	}
	if reason := validateLayout(doc); reason != "" {
		writeAPIErrMsg(w, http.StatusBadRequest, "bad_layout", reason)
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
	if err := h.pool.Queries().PutDashboard(ctx, sqlc.PutDashboardParams{
		TenantID: tenantID, ProjectID: projectID, Layout: stored,
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
