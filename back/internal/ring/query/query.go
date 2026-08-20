// Package query builds the ONLY SQL path to the ClickHouse logs table (plan
// invariant 4: "every query to logs goes through internal/ring.QueryBuilder").
// The depguard rule `logs-only-via-ring` enforces this at lint time — no other
// package may import storage/ch/logs. QueryBuilder appends `seq >= cutoff_seq`
// to every query so the ring displacement is transparent to the caller.
//
// The caller (internal/api, internal/incident) constructs a Query, the builder
// produces a parameterized SQL string + args, and the ClickHouse connection runs
// it. The caller never sees raw `logs` — only the builder's typed result.
package query

import (
	"fmt"
	"strings"
	"time"
)

// QueryBuilder is the single entry point to the logs table. It carries the
// project_window's cutoff_seq so every query is automatically scoped to the
// visible window.
type QueryBuilder struct {
	cutoffSeq int64
	projectID int64
	tenantID  int64
}

// New builds a QueryBuilder for a project with the given cutoff boundary.
func New(tenantID, projectID, cutoffSeq int64) *QueryBuilder {
	return &QueryBuilder{tenantID: tenantID, projectID: projectID, cutoffSeq: cutoffSeq}
}

// CutoffSeq returns the visibility boundary (for the caller's /v1/plan response).
func (q *QueryBuilder) CutoffSeq() int64 { return q.cutoffSeq }

// LogQuery is a parameterised logs query. The caller passes the SQL and args to
// the ClickHouse driver; the builder guarantees the cutoff is present.
type LogQuery struct {
	SQL  string
	Args []any
}

// Range optionally bounds a query to a slice of time, half-open [From, To). A
// zero time leaves that side at the ring's own edge, so the zero Range means
// "the whole visible window" and every caller that has no range keeps its
// meaning unchanged.
//
// This is NOT the cutoff and never replaces it. The cutoff is what the plan
// still stores and is not negotiable; a Range is only what the reader is
// currently looking at. Widening a Range past the ring therefore narrows
// nothing, which is the honest answer — the lines outside it were never kept.
type Range struct {
	From time.Time
	To   time.Time
}

// Bounded reports whether either side of the range is set.
func (r Range) Bounded() bool { return !r.From.IsZero() || !r.To.IsZero() }

// appendRangeFilter binds the range's ends. Resolved to time.Time here for the
// same reason WindowCount's cutoff is: ClickHouse cannot take a bound parameter
// inside an INTERVAL, so nothing about the range reaches the engine as text.
func appendRangeFilter(conditions []string, args []any, within Range) ([]string, []any) {
	if !within.From.IsZero() {
		conditions = append(conditions, "ts >= ?")
		args = append(args, within.From.UTC())
	}
	if !within.To.IsZero() {
		conditions = append(conditions, "ts < ?")
		args = append(args, within.To.UTC())
	}
	return conditions, args
}

// Stream returns the latest N log lines within the visible window, optionally
// filtered by levels/services/text and bounded to an instant range. This is what
// GET /v1/logs uses. Empty slices mean "no filter" — a reader who picked nothing
// asked for everything — and the zero Range means the whole window.
//
// The range matters more than it looks: without it the panel could only ever
// hold the newest `limit` lines, so panning the timeline anywhere but the live
// edge showed volume with an empty stream underneath it.
func (q *QueryBuilder) Stream(limit int, levels, services []string, search string, within Range) LogQuery {
	var conditions []string
	var args []any

	// Always-present: tenant + project + cutoff.
	conditions = append(conditions, "tenant_id = ?", "project_id = ?", "seq >= ?")
	args = append(args, q.tenantID, q.projectID, q.cutoffSeq)

	conditions, args = appendLevelFilter(conditions, args, levels)
	conditions, args = appendServiceFilter(conditions, args, services)
	if search != "" {
		conditions = append(conditions, "message ILIKE ?")
		args = append(args, "%"+search+"%")
	}
	conditions, args = appendRangeFilter(conditions, args, within)

	sql := fmt.Sprintf("SELECT seq, ts, level, service, message FROM logs WHERE %s ORDER BY seq DESC LIMIT %d",
		strings.Join(conditions, " AND "), limit)
	return LogQuery{SQL: sql, Args: args}
}

// Evidence is the incident slice's read: the same bounded window Stream uses,
// ordered so error and warning lines fill the budget first and ordinary traffic
// only tops up whatever is left.
//
// The slice is both what the card shows and what the AI read is given, so its
// composition is the ceiling on both. `ORDER BY seq DESC LIMIT n` returns the
// tail of the stream whatever it happens to hold: on a busy project that is n
// lines of healthy traffic surrounding one failure, which is evidence in name
// only. Errors first keeps the failing lines whatever the ratio is.
//
// The top-up is not a nicety. An availability incident on a project that ships
// only info lines has no error level at all, and a bare filter would freeze an
// empty slice — the card drops the whole pane when the slice is empty, so the
// reader would lose the context they have today to gain evidence that does not
// exist.
func (q *QueryBuilder) Evidence(limit int) LogQuery {
	sql := fmt.Sprintf("SELECT seq, ts, level, service, message FROM logs "+
		"WHERE tenant_id = ? AND project_id = ? AND seq >= ? "+
		"ORDER BY level IN ('error', 'warn') DESC, seq DESC LIMIT %d", limit)
	return LogQuery{SQL: sql, Args: []any{q.tenantID, q.projectID, q.cutoffSeq}}
}

// appendLevelFilter narrows to the picked level buckets. The API's `info`
// bucket is "neither an error nor a warning" — it covers debug and anything
// else a collector labels a line with, so the three buckets always partition
// the stream. Unknown values are dropped here rather than matched literally,
// so a crafted level can never widen or invert the predicate.
func appendLevelFilter(conditions []string, args []any, levels []string) ([]string, []any) {
	var parts []string
	for _, level := range levels {
		switch level {
		case "error", "warn":
			parts = append(parts, "level = '"+level+"'")
		case "info":
			parts = append(parts, "level NOT IN ('error', 'warn')")
		}
	}
	if len(parts) == 0 {
		return conditions, args
	}
	return append(conditions, "("+strings.Join(parts, " OR ")+")"), args
}

// appendServiceFilter narrows to lines from any of the named services. The
// empty string is a real name — the unlabelled service — so it binds like any
// other value rather than meaning "no filter" (that is the empty slice's job).
func appendServiceFilter(conditions []string, args []any, services []string) ([]string, []any) {
	if len(services) == 0 {
		return conditions, args
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(services)), ", ")
	conditions = append(conditions, "service IN ("+placeholders+")")
	for _, service := range services {
		args = append(args, service)
	}
	return conditions, args
}

// WindowCount counts the lines the window holds before the stream limit, with
// the SAME filters, cutoff and range as Stream — a different predicate here
// would make GET /v1/logs's `total` a lie, and "showing 200 of 4,000" is a
// sentence about two counts of one question. The zero Range means the whole
// visible ring.
func (q *QueryBuilder) WindowCount(within Range, levels, services []string, search string) LogQuery {
	var conditions []string
	var args []any

	conditions = append(conditions, "tenant_id = ?", "project_id = ?", "seq >= ?")
	args = append(args, q.tenantID, q.projectID, q.cutoffSeq)

	conditions, args = appendLevelFilter(conditions, args, levels)
	conditions, args = appendServiceFilter(conditions, args, services)
	if search != "" {
		conditions = append(conditions, "message ILIKE ?")
		args = append(args, "%"+search+"%")
	}
	conditions, args = appendRangeFilter(conditions, args, within)

	return LogQuery{
		SQL:  "SELECT count() FROM logs WHERE " + strings.Join(conditions, " AND "),
		Args: args,
	}
}

// Services lists the services present in the window with their line counts —
// what the logs panel builds its picker from. window <= 0 means the whole
// visible ring.
//
// Deliberately NOT filtered by service: the list has to describe the window,
// not the reader's current choice, or picking one service leaves a picker
// holding a single option and no way back to the rest.
func (q *QueryBuilder) Services(window time.Duration) LogQuery {
	conditions := []string{"tenant_id = ?", "project_id = ?", "seq >= ?"}
	args := []any{q.tenantID, q.projectID, q.cutoffSeq}
	if window > 0 {
		conditions = append(conditions, "ts >= ?")
		args = append(args, time.Now().UTC().Add(-window))
	}
	return LogQuery{
		SQL: "SELECT service, count() AS lines FROM logs WHERE " +
			strings.Join(conditions, " AND ") +
			" GROUP BY service ORDER BY lines DESC",
		Args: args,
	}
}

// Volume returns per-minute line counts for the histogram on the logs panel.
// It takes the same filters as Stream for the same reason WindowCount does:
// the strip sits directly above the lines it describes, and an unfiltered
// chart over a filtered list is two counts of two different things, side by
// side.
func (q *QueryBuilder) Volume(bucketMinutes int, levels, services []string) LogQuery {
	conditions := []string{"tenant_id = ?", "project_id = ?", "seq >= ?"}
	args := []any{q.tenantID, q.projectID, q.cutoffSeq}
	conditions, args = appendLevelFilter(conditions, args, levels)
	conditions, args = appendServiceFilter(conditions, args, services)
	return LogQuery{
		SQL: "SELECT toStartOfMinute(ts) AS minute, level, count() AS lines " +
			"FROM logs WHERE " + strings.Join(conditions, " AND ") +
			" GROUP BY minute, level ORDER BY minute",
		Args: args,
	}
}

// Slice returns log lines in a [from, to) seq range for the incident's frozen
// 2-phase slice (plan §5.8: Phase 1 at opened_at, Phase 2 at +15m).
func (q *QueryBuilder) Slice(fromSeq, toSeq int64, limit int) LogQuery {
	return LogQuery{
		SQL: fmt.Sprintf(
			"SELECT ts, level, service, message FROM logs "+
				"WHERE tenant_id = ? AND project_id = ? AND seq >= ? AND seq < ? "+
				"ORDER BY seq LIMIT %d", limit),
		Args: []any{q.tenantID, q.projectID, fromSeq, toSeq},
	}
}

// Summary returns how many lines are inside the visible window and when the
// last one arrived. It exists so the rest of the app can ask "is this project
// sending anything, and is it still sending?" without reaching into the logs
// table itself: invariant 4 makes this builder the only door, and the ledger
// that would answer it from Postgres is not written yet.
func (q *QueryBuilder) Summary() LogQuery {
	return LogQuery{
		SQL: "SELECT count() AS lines, max(ts) AS last_ts FROM logs " +
			"WHERE tenant_id = ? AND project_id = ? AND seq >= ?",
		Args: []any{q.tenantID, q.projectID, q.cutoffSeq},
	}
}

// BeyondErrors counts errors with seq < cutoff (displaced by the ring) — the
// "zero is silence" rule (§5.1): the field is absent when the count is 0.
func (q *QueryBuilder) BeyondErrors(retainSeq int64) LogQuery {
	return LogQuery{
		SQL: "SELECT count() AS errors, " +
			"dateDiff('hour', min(ts), now()) AS hours " +
			"FROM logs WHERE tenant_id = ? AND project_id = ? " +
			"AND seq >= ? AND seq < ? AND level = 'error'",
		Args: []any{q.tenantID, q.projectID, retainSeq, q.cutoffSeq},
	}
}

// ErrorGroups aggregates recent error lines by fingerprint for the
// notification scanner (docs/plans/channel-notify-settings.md): per
// fingerprint, how often it fired since `since`, a sample message and the last
// time it was seen. Aggregates, not raw rows (invariant 2), still behind the
// cutoff (invariant 4) — a line the ring already displaced must not page
// anybody.
func (q *QueryBuilder) ErrorGroups(since time.Time) LogQuery {
	return LogQuery{
		SQL: "SELECT fingerprint, count() AS lines, anyLast(service) AS service, " +
			"anyLast(message) AS message, max(ts) AS last_ts " +
			"FROM logs WHERE tenant_id = ? AND project_id = ? AND seq >= ? " +
			"AND level = 'error' AND ts >= ? GROUP BY fingerprint",
		Args: []any{q.tenantID, q.projectID, q.cutoffSeq, since},
	}
}

// EventSeen reports whether a named event has arrived inside the visible
// window: how many times, first and last arrival. GET /v1/install/status asks
// it about install_verified — the chain proof the SDK sends once; an event the
// ring has displaced no longer counts, which is why the CLI's verify also
// reads the summary before declaring failure.
func (q *QueryBuilder) EventSeen(name string) LogQuery {
	return LogQuery{
		SQL: "SELECT count() AS times, min(ts) AS first_ts, max(ts) AS last_ts " +
			"FROM logs WHERE tenant_id = ? AND project_id = ? AND seq >= ? AND message = ?",
		Args: []any{q.tenantID, q.projectID, q.cutoffSeq, name},
	}
}

// RecentEvents groups the trailing window's lines by message — what verify
// prints as "arriving now", so a name drifted off the dictionary shows up next
// to the ones that were expected.
func (q *QueryBuilder) RecentEvents(window time.Duration, limit int) LogQuery {
	return LogQuery{
		SQL: fmt.Sprintf("SELECT message, count() AS times, max(ts) AS last_ts "+
			"FROM logs WHERE tenant_id = ? AND project_id = ? AND seq >= ? AND ts >= ? "+
			"GROUP BY message ORDER BY times DESC LIMIT %d", limit),
		Args: []any{q.tenantID, q.projectID, q.cutoffSeq, time.Now().UTC().Add(-window)},
	}
}

// LatestExplain fetches the lines around an error fingerprint for the AI explain
// feature. The caller passes the fingerprint from the incident card.
func (q *QueryBuilder) LatestExplain(limit int) LogQuery {
	return LogQuery{
		SQL: fmt.Sprintf(
			"SELECT ts, level, service, message FROM logs "+
				"WHERE tenant_id = ? AND project_id = ? AND seq >= ? "+
				"ORDER BY ts DESC LIMIT %d", limit),
		Args: []any{q.tenantID, q.projectID, q.cutoffSeq},
	}
}
