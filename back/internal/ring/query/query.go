// Package query is the only SQL path to the Postgres logs table (depguard's
// `logs-only-via-ring` enforces it); every query appends `seq >= cutoff_seq`.
package query

import (
	"fmt"
	"strings"
	"time"
)

// QueryBuilder is the single entry point to the logs table; it carries the
// project's cutoff_seq so every query is scoped to the visible window.
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
// pgx; the builder guarantees the cutoff is present.
type LogQuery struct {
	SQL  string
	Args []any
}

// bind rewrites each ? placeholder to $1, $2, … in order and freezes the query:
// conditions append with ? and the numbering is assigned here, so a placeholder
// can never drift from the args slice.
func bind(sql string, args []any) LogQuery {
	var b strings.Builder
	n := 0
	for i := 0; i < len(sql); i++ {
		if sql[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteByte(sql[i])
	}
	return LogQuery{SQL: b.String(), Args: args}
}

// Range optionally bounds a query to a half-open [From, To) time slice; a zero
// side is the ring's edge. A Range never widens past the cutoff.
type Range struct {
	From time.Time
	To   time.Time
}

// Bounded reports whether either side of the range is set.
func (r Range) Bounded() bool { return !r.From.IsZero() || !r.To.IsZero() }

// appendRangeFilter binds the range's ends as parameters after the constants,
// so no range text reaches the engine.
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

// Stream is the latest N lines of the visible window (GET /v1/logs): empty
// slices mean no filter, the zero Range the whole window (panning needs it).
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
	return bind(sql, args)
}

// Evidence is the incident slice's read: error and warn lines fill the budget
// first, ordinary traffic tops up the rest (info-only projects still get context).
func (q *QueryBuilder) Evidence(limit int) LogQuery {
	sql := fmt.Sprintf("SELECT seq, ts, level, service, message FROM logs "+
		"WHERE tenant_id = ? AND project_id = ? AND seq >= ? "+
		"ORDER BY level IN ('error', 'warn') DESC, seq DESC LIMIT %d", limit)
	return bind(sql, []any{q.tenantID, q.projectID, q.cutoffSeq})
}

// appendLevelFilter narrows to the picked buckets: `info` means "not error
// nor warn", and unknown values are dropped so a crafted level cannot invert this.
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

// appendServiceFilter narrows to the named services; the empty string is a
// real name (the unlabelled service), while the empty slice means no filter.
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

// WindowCount counts the window before the stream limit with the SAME filters,
// cutoff and range as Stream; a different predicate would make `total` a lie.
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

	return bind("SELECT count(*) FROM logs WHERE "+strings.Join(conditions, " AND "), args)
}

// Services lists services present in the window with line counts; window <= 0
// means the whole ring. Not service-filtered: the picker must describe the window.
func (q *QueryBuilder) Services(window time.Duration) LogQuery {
	conditions := []string{"tenant_id = ?", "project_id = ?", "seq >= ?"}
	args := []any{q.tenantID, q.projectID, q.cutoffSeq}
	if window > 0 {
		conditions = append(conditions, "ts >= ?")
		args = append(args, time.Now().UTC().Add(-window))
	}
	return bind("SELECT service, count(*) AS lines FROM logs WHERE "+
		strings.Join(conditions, " AND ")+
		" GROUP BY service ORDER BY lines DESC", args)
}

// Volume returns per-minute line counts for the histogram, taking Stream's
// filters (the strip describes the lines below it). The bucket is a minute.
func (q *QueryBuilder) Volume(levels, services []string) LogQuery {
	conditions := []string{"tenant_id = ?", "project_id = ?", "seq >= ?"}
	args := []any{q.tenantID, q.projectID, q.cutoffSeq}
	conditions, args = appendLevelFilter(conditions, args, levels)
	conditions, args = appendServiceFilter(conditions, args, services)
	return bind("SELECT date_trunc('minute', ts) AS minute, level, count(*) AS lines "+
		"FROM logs WHERE "+strings.Join(conditions, " AND ")+
		" GROUP BY minute, level ORDER BY minute", args)
}

// detailBuckets are the resolutions VolumeDetail answers at, finest first;
// the same ladder the strip draws with.
var detailBuckets = []int{1, 2, 5, 10, 15, 30, 60}

// maxDetailBuckets caps one answer; it keeps a wide range from grouping a
// whole ring one second at a time.
const maxDetailBuckets = 600

// DetailBucketSeconds picks the resolution a detail read uses; 0 means "the
// per-minute map already answers this" (no detail finer than a minute exists).
func DetailBucketSeconds(requested int, within Range) int {
	if requested <= 0 || within.From.IsZero() || within.To.IsZero() {
		return 0
	}
	span := within.To.Sub(within.From).Seconds()
	if span <= 0 {
		return 0
	}
	for _, size := range detailBuckets {
		if size < requested {
			continue
		}
		if span/float64(size) <= maxDetailBuckets {
			return size
		}
	}
	return 0
}

// VolumeDetail counts lines inside the range at bucketSeconds, the fine-grained
// companion to Volume; size is snapped to detailBuckets, never caller text.
func (q *QueryBuilder) VolumeDetail(bucketSeconds int, within Range, levels, services []string) LogQuery {
	size := DetailBucketSeconds(bucketSeconds, within)
	if size == 0 {
		return LogQuery{}
	}
	conditions := []string{"tenant_id = ?", "project_id = ?", "seq >= ?"}
	args := []any{q.tenantID, q.projectID, q.cutoffSeq}
	conditions, args = appendLevelFilter(conditions, args, levels)
	conditions, args = appendServiceFilter(conditions, args, services)
	conditions, args = appendRangeFilter(conditions, args, within)
	return bind(fmt.Sprintf(
		"SELECT to_timestamp(floor(extract(epoch from ts) / %d) * %d) AS bucket, level, count(*) AS lines "+
			"FROM logs WHERE %s GROUP BY bucket, level ORDER BY bucket",
		size, size, strings.Join(conditions, " AND ")), args)
}

// Slice returns log lines in a [from, to) seq range for the incident's frozen slice.
func (q *QueryBuilder) Slice(fromSeq, toSeq int64, limit int) LogQuery {
	return bind(fmt.Sprintf(
		"SELECT ts, level, service, message FROM logs "+
			"WHERE tenant_id = ? AND project_id = ? AND seq >= ? AND seq < ? "+
			"ORDER BY seq LIMIT %d", limit),
		[]any{q.tenantID, q.projectID, fromSeq, toSeq})
}

// Summary returns how many lines the window holds and when the last arrived:
// "is this project sending anything, and is it still sending?"
func (q *QueryBuilder) Summary() LogQuery {
	return bind("SELECT count(*) AS lines, max(ts) AS last_ts FROM logs "+
		"WHERE tenant_id = ? AND project_id = ? AND seq >= ?",
		[]any{q.tenantID, q.projectID, q.cutoffSeq})
}

// BeyondErrors counts errors with seq < cutoff (displaced by the ring); the
// field is absent when 0 (zero is silence).
func (q *QueryBuilder) BeyondErrors(retainSeq int64) LogQuery {
	return bind("SELECT count(*) AS errors, "+
		"COALESCE(extract(epoch from (now() - min(ts))) / 3600, 0)::bigint AS hours "+
		"FROM logs WHERE tenant_id = ? AND project_id = ? "+
		"AND seq >= ? AND seq < ? AND level = 'error'",
		[]any{q.tenantID, q.projectID, retainSeq, q.cutoffSeq})
}

// ErrorGroups aggregates recent error lines by fingerprint (count, sample,
// last seen) for the notification scanner; still behind the cutoff.
func (q *QueryBuilder) ErrorGroups(since time.Time) LogQuery {
	return bind("SELECT fingerprint, count(*) AS lines, "+
		"(array_agg(service ORDER BY ts DESC))[1] AS service, "+
		"(array_agg(message ORDER BY ts DESC))[1] AS message, max(ts) AS last_ts "+
		"FROM logs WHERE tenant_id = ? AND project_id = ? AND seq >= ? "+
		"AND level = 'error' AND ts >= ? GROUP BY fingerprint",
		[]any{q.tenantID, q.projectID, q.cutoffSeq, since})
}

// EventSeen reports how many times a named event arrived inside the window;
// an event the ring displaced no longer counts.
func (q *QueryBuilder) EventSeen(name string) LogQuery {
	return bind("SELECT count(*) AS times, min(ts) AS first_ts, max(ts) AS last_ts "+
		"FROM logs WHERE tenant_id = ? AND project_id = ? AND seq >= ? AND message = ?",
		[]any{q.tenantID, q.projectID, q.cutoffSeq, name})
}

// RecentEvents groups the trailing window's lines by message, so a name
// drifted off the dictionary shows up next to the expected ones.
func (q *QueryBuilder) RecentEvents(window time.Duration, limit int) LogQuery {
	return bind(fmt.Sprintf("SELECT message, count(*) AS times, max(ts) AS last_ts "+
		"FROM logs WHERE tenant_id = ? AND project_id = ? AND seq >= ? AND ts >= ? "+
		"GROUP BY message ORDER BY times DESC LIMIT %d", limit),
		[]any{q.tenantID, q.projectID, q.cutoffSeq, time.Now().UTC().Add(-window)})
}
