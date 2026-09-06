// The board's two reads over the logs table: what a project actually sends
// (the picker's catalog) and the per-bucket counts a chart draws. Same rule as
// the rest of the package — every query carries the tenant and the project.

package query

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// canonicalLevel folds the stored level into the three buckets the API knows,
// matching appendLevelFilter: anything that is neither an error nor a warning
// is info, so a group list and a level filter can never disagree.
const canonicalLevel = "CASE WHEN level IN ('error', 'warn') THEN level ELSE 'info' END"

// Groups lists the window's message groups per (service, level, fingerprint):
// how many lines, the newest one as a sample, and when it last arrived. Only
// the top `perLevel` groups of each (service, level) survive — a picker is a
// short list, not the whole ring.
func (q *QueryBuilder) Groups(window time.Duration, perLevel int) LogQuery {
	conditions := []string{"tenant_id = ?", "project_id = ?"}
	args := []any{q.tenantID, q.projectID}
	if window > 0 {
		conditions = append(conditions, "ts >= ?")
		args = append(args, time.Now().UTC().Add(-window))
	}
	// The sample is capped in SQL: a 4 KB line must not cross the wire just to
	// be trimmed for a label.
	return bind(fmt.Sprintf(
		"SELECT service, level, fingerprint, lines, sample, last_ts FROM ("+
			"SELECT service, %s AS level, fingerprint, count(*) AS lines, "+
			"left((array_agg(message ORDER BY ts DESC))[1], 200) AS sample, max(ts) AS last_ts, "+
			"row_number() OVER (PARTITION BY service, %s ORDER BY count(*) DESC) AS rn "+
			"FROM logs WHERE %s GROUP BY service, 2, fingerprint"+
			") g WHERE rn <= %d ORDER BY lines DESC",
		canonicalLevel, canonicalLevel, strings.Join(conditions, " AND "), perLevel), args)
}

// AttrPairs lists the window's most common attribute (key, value) pairs per
// service. It reads the newest `scan` lines rather than the whole window: the
// LATERAL expansion multiplies rows by the attribute count, and the picker
// only needs the shape of recent traffic. Values longer than 80 characters are
// dropped — an id or a stack trace is not a facet.
func (q *QueryBuilder) AttrPairs(window time.Duration, scan, perService int) LogQuery {
	conditions := []string{"tenant_id = ?", "project_id = ?", "attrs IS NOT NULL"}
	args := []any{q.tenantID, q.projectID}
	if window > 0 {
		conditions = append(conditions, "ts >= ?")
		args = append(args, time.Now().UTC().Add(-window))
	}
	return bind(fmt.Sprintf(
		"SELECT service, key, value, lines FROM ("+
			"SELECT service, key, value, count(*) AS lines, "+
			"row_number() OVER (PARTITION BY service ORDER BY count(*) DESC) AS rn FROM ("+
			"SELECT service, attrs FROM logs WHERE %s ORDER BY ts DESC LIMIT %d"+
			") recent CROSS JOIN LATERAL jsonb_each_text(attrs) AS pair(key, value) "+
			"WHERE length(value) <= 80 GROUP BY service, key, value"+
			") a WHERE rn <= %d ORDER BY service, lines DESC",
		strings.Join(conditions, " AND "), scan, perService), args)
}

// SeriesFilter narrows a counted series to one slice of the logs table. A nil
// Service is every service (the empty string is the unlabelled one, a real
// name); an empty Level is every level.
type SeriesFilter struct {
	Service     *string
	Level       string
	Fingerprint *int64
	Search      string
	Attrs       map[string]string
}

// SeriesBuckets counts the range's lines into fixed-width buckets, indexed
// from `within.From`. The base is bound rather than spliced and the index is
// relative, so a previous-span read (one bucket as wide as the whole span)
// uses the same builder as the chart's.
func (q *QueryBuilder) SeriesBuckets(within Range, stepSeconds int, f SeriesFilter) LogQuery {
	if stepSeconds <= 0 || within.From.IsZero() || within.To.IsZero() {
		return LogQuery{}
	}
	// The base rides first because bind numbers placeholders in the order they
	// appear in the SQL, and this one is in the SELECT list.
	args := []any{float64(within.From.Unix())}
	conditions := []string{"tenant_id = ?", "project_id = ?"}
	args = append(args, q.tenantID, q.projectID)
	conditions, args = appendSeriesFilter(conditions, args, f)
	conditions, args = appendRangeFilter(conditions, args, within)
	return bind(fmt.Sprintf(
		"SELECT floor((extract(epoch from ts)::float8 - ?) / %d)::bigint AS bucket, count(*) AS lines "+
			"FROM logs WHERE %s GROUP BY bucket ORDER BY bucket",
		stepSeconds, strings.Join(conditions, " AND ")), args)
}

// appendSeriesFilter binds every value the board can filter on. The attribute
// key is a parameter like its value: it comes from the request.
func appendSeriesFilter(conditions []string, args []any, f SeriesFilter) ([]string, []any) {
	if f.Service != nil {
		conditions, args = appendServiceFilter(conditions, args, []string{*f.Service})
	}
	if f.Level != "" {
		conditions, args = appendLevelFilter(conditions, args, []string{f.Level})
	}
	if f.Fingerprint != nil {
		conditions = append(conditions, "fingerprint = ?")
		args = append(args, *f.Fingerprint)
	}
	if f.Search != "" {
		conditions = append(conditions, "message ILIKE ?")
		args = append(args, "%"+f.Search+"%")
	}
	keys := make([]string, 0, len(f.Attrs))
	for k := range f.Attrs {
		keys = append(keys, k)
	}
	// Sorted so the same filter always builds the same SQL: a map's order is
	// not one, and a plan cache keyed by text would see a new query per read.
	sort.Strings(keys)
	for _, k := range keys {
		// The cast picks the text form of ->>; an untyped parameter leaves the
		// operator ambiguous.
		conditions = append(conditions, "attrs ->> ?::text = ?")
		args = append(args, k, f.Attrs[k])
	}
	return conditions, args
}
