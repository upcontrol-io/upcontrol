// The board's reads: the catalog of what a project sends (events, metrics,
// counters) and the bucketed series a widget draws from checks, events and
// metrics. Logs stay behind ring.QueryBuilder; this file never touches them.

package pgstore

import (
	"context"
	"fmt"
	"time"
)

// CatalogEvent is one event name the project sent inside the catalog window.
type CatalogEvent struct {
	Name   string
	Times  int64
	LastTS time.Time
}

// CatalogMetric is one metric name with how many readings arrived and which
// label keys were seen on them.
type CatalogMetric struct {
	Name     string
	Readings int64
	Labels   []string
}

// CatalogCounter is one reported counter and its members — a funnel's steps,
// a test's variants — in the order the latest readings declared them.
type CatalogCounter struct {
	Name    string
	Members []string
}

// Bucket is one counted bucket of a series, indexed from the range's start.
type Bucket struct {
	Index int64
	Count int64
}

// CheckBucket carries both answers a check series can give: the summed
// response time of the ok probes (with their count, so an average over any
// span is a division rather than an average of averages) and the total number
// of probes, which is what uptime is a share of. The four phase sums carry
// their own run counts: a reused connection records no lookup and no connect,
// and averaging those zeros in would report a lookup nobody measured.
type CheckBucket struct {
	Index        int64
	SumMs        float64
	OK           int64
	Total        int64
	SumDNSMs     float64
	SumConnectMs float64
	SumTLSMs     float64
	SumTTFBMs    float64
	NDNS         int64
	NConnect     int64
	NTLS         int64
	NTTFB        int64
}

// GaugeBucket is one bucket of a metric series: the mean of its readings and
// the newest one, which is what a "latest value" total is read from.
type GaugeBucket struct {
	Index int64
	Avg   float64
	Last  float64
}

// SumBucket is one bucket of a counter series: the increments that landed in
// it. A counter arrives as a running total, so what a chart draws is the
// difference between consecutive readings, not the readings themselves.
type SumBucket struct {
	Index int64
	Sum   float64
}

// LabelSum is one label combination's share of a counter's increase over the
// range.
type LabelSum struct {
	Labels map[string]string
	Sum    float64
}

// bucketExpr indexes a timestamp into fixed-width buckets counted from a base
// epoch bound as a parameter. The width is an integer the caller took from the
// range enum, never request text.
func bucketExpr(baseParam int, stepSeconds int) string {
	return fmt.Sprintf("floor((extract(epoch from ts)::float8 - $%d) / %d)::bigint", baseParam, stepSeconds)
}

// CatalogEvents lists the event names the project sent since `since`.
func (s *Store) CatalogEvents(ctx context.Context, tenantID, projectID int64, since time.Time) ([]CatalogEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, count(*) AS times, max(ts) AS last_ts
		  FROM events
		 WHERE tenant_id = $1 AND project_id = $2 AND ts >= $3
		 GROUP BY name ORDER BY times DESC`, tenantID, projectID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogEvent{}
	for rows.Next() {
		var e CatalogEvent
		if err := rows.Scan(&e.Name, &e.Times, &e.LastTS); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// reservedCounters are the reported kinds the Metrics picker must not offer
// as gauges: each is a counter with a shape of its own, and drawing its
// running total would label it a reading. CatalogCounters reads them. One
// list, so every predicate that excludes them cannot drift.
var reservedCounters = []string{"funnel", "experiment", "retention", "breakdown"}

// CatalogMetrics lists the metric names except the reserved counters, which
// are shapes of their own (CatalogCounters reads those). The label keys come
// from a second pass: expanding them in the counting query would multiply the
// reading count by the number of keys.
func (s *Store) CatalogMetrics(ctx context.Context, tenantID, projectID int64, since time.Time) ([]CatalogMetric, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.name, m.readings, COALESCE(k.keys, ARRAY[]::text[])
		  FROM (SELECT name, count(*) AS readings FROM metrics
		         WHERE tenant_id = $1 AND project_id = $2 AND ts >= $3 AND name <> ALL($4)
		         GROUP BY name) m
		  LEFT JOIN (SELECT name, array_agg(DISTINCT key) AS keys
		               FROM metrics, LATERAL jsonb_object_keys(labels) AS key
		              WHERE tenant_id = $1 AND project_id = $2 AND ts >= $3 AND name <> ALL($4)
		              GROUP BY name) k ON k.name = m.name
		 ORDER BY m.name`, tenantID, projectID, since, reservedCounters)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogMetric{}
	for rows.Next() {
		var m CatalogMetric
		if err := rows.Scan(&m.Name, &m.Readings, &m.Labels); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// maxCatalogMembers caps one counter's member list in the catalog. A
// breakdown dimension can have thousands of values, and the catalog only
// needs enough to count and to name.
const maxCatalogMembers = 200

// CatalogCounters groups one reported counter kind into its names and their
// members. The order is the `i` label of the newest reading of each member; a
// member whose `i` is not a number sorts last rather than failing the read.
// The two label names ride as parameters like every other value.
func (s *Store) CatalogCounters(ctx context.Context, tenantID, projectID int64,
	name, keyLabel, memberLabel string, since time.Time) ([]CatalogCounter, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key, member FROM (
			SELECT labels->>$4 AS key, labels->>$5 AS member,
			       (array_agg(labels->>'i' ORDER BY ts DESC))[1] AS i
			  FROM metrics
			 WHERE tenant_id = $1 AND project_id = $2 AND ts >= $3 AND name = $6
			   AND labels->>$4 IS NOT NULL AND labels->>$5 IS NOT NULL
			 GROUP BY 1, 2
		) f
		 ORDER BY key, (CASE WHEN i ~ '^-?[0-9]+$' THEN i::int END) NULLS LAST, member`,
		tenantID, projectID, since, keyLabel, memberLabel, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogCounter{}
	at := map[string]int{}
	for rows.Next() {
		var key, member string
		if err := rows.Scan(&key, &member); err != nil {
			return nil, err
		}
		i, ok := at[key]
		if !ok {
			i = len(out)
			at[key] = i
			out = append(out, CatalogCounter{Name: key, Members: []string{}})
		}
		if len(out[i].Members) < maxCatalogMembers {
			out[i].Members = append(out[i].Members, member)
		}
	}
	return out, rows.Err()
}

// HistoryBuckets counts the hourly rollup into buckets of stepSeconds over
// [from, to). It answers the same question SeriesBuckets answers over the raw
// lines, so the service and level predicates have to MEAN the same thing:
// a nil service is every service and the empty string the unlabelled one,
// and `info` is "neither error nor warn" rather than a level of its own.
// A level that is none of the three narrows nothing, as the ring's does.
func (s *Store) HistoryBuckets(ctx context.Context, tenantID, projectID int64, from, to time.Time, stepSeconds int, service *string, level string, fingerprint *int64) ([]Bucket, error) {
	if stepSeconds <= 0 {
		return nil, fmt.Errorf("history bucket width must be positive")
	}
	args := []any{tenantID, projectID, float64(from.Unix()), from, to}
	conditions := "tenant_id = $1 AND project_id = $2 AND hour >= $4 AND hour < $5"
	if service != nil {
		args = append(args, *service)
		conditions += fmt.Sprintf(" AND service = $%d", len(args))
	}
	switch level {
	case "error", "warn":
		args = append(args, level)
		conditions += fmt.Sprintf(" AND level = $%d", len(args))
	case "info":
		conditions += " AND level NOT IN ('error', 'warn')"
	}
	if fingerprint != nil {
		args = append(args, *fingerprint)
		conditions += fmt.Sprintf(" AND fingerprint = $%d", len(args))
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(
		"SELECT floor((extract(epoch from hour)::float8 - $3) / %d)::bigint AS bucket, sum(lines)::bigint"+
			" FROM series_1h WHERE %s GROUP BY bucket ORDER BY bucket", stepSeconds, conditions), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Index, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// OldestHistory is the first hour the rollup holds for the project; false when
// it holds none. What the store never kept was never zero, so a chart drawn
// before it has no value to draw rather than a measured 0.
func (s *Store) OldestHistory(ctx context.Context, tenantID, projectID int64) (time.Time, bool, error) {
	return s.oldest(ctx, `SELECT min(hour) FROM series_1h WHERE tenant_id = $1 AND project_id = $2`, tenantID, projectID)
}

// OldestLine is OldestHistory over the raw lines: the same fact for the ranges
// the ring still answers.
func (s *Store) OldestLine(ctx context.Context, tenantID, projectID int64) (time.Time, bool, error) {
	return s.oldest(ctx, `SELECT min(ts) FROM logs WHERE tenant_id = $1 AND project_id = $2`, tenantID, projectID)
}

func (s *Store) oldest(ctx context.Context, sql string, tenantID, projectID int64) (time.Time, bool, error) {
	var at *time.Time
	if err := s.pool.QueryRow(ctx, sql, tenantID, projectID).Scan(&at); err != nil {
		return time.Time{}, false, err
	}
	if at == nil {
		return time.Time{}, false, nil
	}
	return at.UTC(), true, nil
}

// TrimHistory drops rollup rows past the tenant's plan depth and answers how
// many went. One statement, joined through the entitlement table so a limit
// moves without a redeploy; a NULL history_days trims nothing, which is what
// makes Self-hosted unlimited.
func (s *Store) TrimHistory(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM series_1h s
		 USING project pr, tenant t, plan_entitlement p
		 WHERE pr.id = s.project_id AND pr.tenant_id = s.tenant_id
		   AND t.id = pr.tenant_id AND p.plan = t.plan
		   AND p.history_days IS NOT NULL
		   AND s.hour < date_trunc('hour', now()) - make_interval(days => p.history_days)`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// EventBuckets counts a named event into buckets of stepSeconds over
// [from, to). One bucket as wide as the whole span is how a previous-span
// total is read.
func (s *Store) EventBuckets(ctx context.Context, tenantID, projectID int64, name string, from, to time.Time, stepSeconds int) ([]Bucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+bucketExpr(4, stepSeconds)+` AS bucket, count(*)::bigint
		  FROM events
		 WHERE tenant_id = $1 AND project_id = $2 AND name = $3 AND ts >= $5 AND ts < $6
		 GROUP BY bucket ORDER BY bucket`,
		tenantID, projectID, name, float64(from.Unix()), from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Index, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CheckBuckets reads a monitor's probes into buckets. The monitor id is
// resolved inside the tenant by the caller; the checks table carries no
// project, so the tenant is the whole scope there.
func (s *Store) CheckBuckets(ctx context.Context, tenantID, monitorID int64, from, to time.Time, stepSeconds int) ([]CheckBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+bucketExpr(3, stepSeconds)+` AS bucket,
		       COALESCE(sum(total_ms) FILTER (WHERE ok), 0)::float8,
		       count(*) FILTER (WHERE ok)::bigint,
		       count(*)::bigint,
		       COALESCE(sum(dns_ms) FILTER (WHERE ok), 0)::float8,
		       COALESCE(sum(connect_ms) FILTER (WHERE ok), 0)::float8,
		       COALESCE(sum(tls_ms) FILTER (WHERE ok), 0)::float8,
		       COALESCE(sum(ttfb_ms) FILTER (WHERE ok), 0)::float8,
		       count(*) FILTER (WHERE ok AND dns_ms > 0)::bigint,
		       count(*) FILTER (WHERE ok AND connect_ms > 0)::bigint,
		       count(*) FILTER (WHERE ok AND tls_ms > 0)::bigint,
		       count(*) FILTER (WHERE ok AND ttfb_ms > 0)::bigint
		  FROM checks
		 WHERE tenant_id = $1 AND monitor_id = $2 AND ts >= $4 AND ts < $5
		 GROUP BY bucket ORDER BY bucket`,
		tenantID, monitorID, float64(from.Unix()), from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckBucket{}
	for rows.Next() {
		var b CheckBucket
		if err := rows.Scan(&b.Index, &b.SumMs, &b.OK, &b.Total,
			&b.SumDNSMs, &b.SumConnectMs, &b.SumTLSMs, &b.SumTTFBMs,
			&b.NDNS, &b.NConnect, &b.NTLS, &b.NTTFB); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MetricBuckets averages a metric's readings per bucket and keeps the newest
// value of each, which is what a gauge's "latest" total reads.
func (s *Store) MetricBuckets(ctx context.Context, tenantID, projectID int64, name string, labels map[string]string, from, to time.Time, stepSeconds int) ([]GaugeBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+bucketExpr(5, stepSeconds)+` AS bucket,
		       avg(value)::float8,
		       (array_agg(value ORDER BY ts DESC))[1]::float8
		  FROM metrics
		 WHERE tenant_id = $1 AND project_id = $2 AND name = $3 AND labels @> $4::jsonb
		   AND ts >= $6 AND ts < $7
		 GROUP BY bucket ORDER BY bucket`,
		tenantID, projectID, name, string(jsonb(labels)), float64(from.Unix()), from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GaugeBucket{}
	for rows.Next() {
		var b GaugeBucket
		if err := rows.Scan(&b.Index, &b.Avg, &b.Last); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MetricLast is the newest reading strictly before `before`; false when the
// metric has none. Zero is silence: a gauge with no reading has no value, and
// 0 would be one.
func (s *Store) MetricLast(ctx context.Context, tenantID, projectID int64, name string, labels map[string]string, before time.Time) (float64, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT value FROM metrics
		 WHERE tenant_id = $1 AND project_id = $2 AND name = $3 AND labels @> $4::jsonb AND ts < $5
		 ORDER BY ts DESC LIMIT 1`,
		tenantID, projectID, name, string(jsonb(labels)), before)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	if rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return 0, false, err
		}
		return v, true, nil
	}
	return 0, false, rows.Err()
}

// FunnelBuckets folds a counter metric into buckets of stepSeconds over
// [from, to). The fold runs in the database: the delta against the previous
// reading, a drop counted as a reset worth its own value, and the very first
// reading counted as nothing because there is nothing to measure it against.
// The last reading strictly before `from` rides along as that seed and is then
// dropped from the answer, so the range's first delta is a difference rather
// than a whole running total.
func (s *Store) FunnelBuckets(ctx context.Context, tenantID, projectID int64, name string, labels map[string]string, from, to time.Time, stepSeconds int) ([]SumBucket, error) {
	rows, err := s.pool.Query(ctx, `
		WITH span AS (
			(SELECT ts, value FROM metrics
			  WHERE tenant_id = $1 AND project_id = $2 AND name = $3 AND labels @> $4::jsonb
			    AND ts >= $6 AND ts < $7)
			UNION ALL
			(SELECT ts, value FROM metrics
			  WHERE tenant_id = $1 AND project_id = $2 AND name = $3 AND labels @> $4::jsonb
			    AND ts < $6
			  ORDER BY ts DESC LIMIT 1)
		), stepped AS (
			SELECT ts, value, lag(value) OVER (ORDER BY ts) AS prev FROM span
		)
		SELECT `+bucketExpr(5, stepSeconds)+` AS bucket,
		       sum(CASE WHEN prev IS NULL THEN 0
		                WHEN value < prev THEN value
		                ELSE value - prev END)::float8
		  FROM stepped
		 WHERE ts >= $6 AND ts < $7
		 GROUP BY bucket ORDER BY bucket`,
		tenantID, projectID, name, string(jsonb(labels)), float64(from.Unix()), from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SumBucket{}
	for rows.Next() {
		var b SumBucket
		if err := rows.Scan(&b.Index, &b.Sum); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// maxGroupRows caps a grouped answer. A dimension with thousands of values
// must not turn one card into a megabyte.
const maxGroupRows = 200

// CounterGroups folds a counter into one sum per label combination over
// [from, to) — FunnelBuckets' fold with the label tuple where the time bucket
// was: the delta against the previous reading, a drop counted as a reset
// worth its own value, the first reading counted as nothing, and the last
// reading strictly before `from` riding along as the seed and then dropped.
// The group keys are customer text: they ride as parameters, and only the
// tuple's width is ever spliced into the SQL.
func (s *Store) CounterGroups(ctx context.Context, tenantID, projectID int64, name string,
	labels map[string]string, group []string, from, to time.Time) ([]LabelSum, error) {
	if len(group) == 0 || len(group) > 2 {
		return nil, fmt.Errorf("group takes one or two label keys")
	}
	sel, keys, g2not := "labels->>$5 AS g1", "g1", ""
	if len(group) == 2 {
		sel, keys, g2not = "labels->>$5 AS g1, labels->>$6 AS g2", "g1, g2", " AND g2 IS NOT NULL"
	}
	fromP := fmt.Sprintf("$%d", 5+len(group))
	toP := fmt.Sprintf("$%d", 6+len(group))
	limitP := fmt.Sprintf("$%d", 7+len(group))
	args := []any{tenantID, projectID, name, string(jsonb(labels)), group[0]}
	if len(group) == 2 {
		args = append(args, group[1])
	}
	args = append(args, from, to, maxGroupRows)
	rows, err := s.pool.Query(ctx, `
		WITH span AS (
			(SELECT ts, value, `+sel+` FROM metrics
			  WHERE tenant_id = $1 AND project_id = $2 AND name = $3 AND labels @> $4::jsonb
			    AND ts >= `+fromP+` AND ts < `+toP+`)
			UNION ALL
			(SELECT DISTINCT ON (`+keys+`) ts, value, `+sel+` FROM metrics
			  WHERE tenant_id = $1 AND project_id = $2 AND name = $3 AND labels @> $4::jsonb
			    AND ts < `+fromP+`
			  ORDER BY `+keys+`, ts DESC)
		), stepped AS (
			SELECT `+keys+`, ts, value,
			       lag(value) OVER (PARTITION BY `+keys+` ORDER BY ts) AS prev FROM span
		)
		SELECT `+keys+`, sum(CASE WHEN prev IS NULL THEN 0
		                WHEN value < prev THEN value
		                ELSE value - prev END)::float8 AS total
		  FROM stepped
		 WHERE ts >= `+fromP+` AND ts < `+toP+` AND g1 IS NOT NULL`+g2not+`
		 GROUP BY `+keys+` ORDER BY total DESC LIMIT `+limitP, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LabelSum{}
	for rows.Next() {
		var sum LabelSum
		var g1, g2 string
		dest := []any{&g1}
		if len(group) == 2 {
			dest = append(dest, &g2)
		}
		dest = append(dest, &sum.Sum)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		sum.Labels = map[string]string{group[0]: g1}
		if len(group) == 2 {
			sum.Labels[group[1]] = g2
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}
