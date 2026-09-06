// The board's reads: the catalog of what a project sends (events, metrics,
// funnels) and the bucketed series a widget draws from checks, events and
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

// CatalogFunnel is one funnel and its steps, in the order the latest readings
// declared them.
type CatalogFunnel struct {
	Name  string
	Steps []string
}

// Bucket is one counted bucket of a series, indexed from the range's start.
type Bucket struct {
	Index int64
	Count int64
}

// CheckBucket carries both answers a check series can give: the summed
// response time of the ok probes (with their count, so an average over any
// span is a division rather than an average of averages) and the total number
// of probes, which is what uptime is a share of.
type CheckBucket struct {
	Index int64
	SumMs float64
	OK    int64
	Total int64
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

// CatalogMetrics lists the metric names except `funnel`, which is a shape of
// its own (CatalogFunnels reads it). The label keys come from a second pass:
// expanding them in the counting query would multiply the reading count by the
// number of keys.
func (s *Store) CatalogMetrics(ctx context.Context, tenantID, projectID int64, since time.Time) ([]CatalogMetric, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.name, m.readings, COALESCE(k.keys, ARRAY[]::text[])
		  FROM (SELECT name, count(*) AS readings FROM metrics
		         WHERE tenant_id = $1 AND project_id = $2 AND ts >= $3 AND name <> 'funnel'
		         GROUP BY name) m
		  LEFT JOIN (SELECT name, array_agg(DISTINCT key) AS keys
		               FROM metrics, LATERAL jsonb_object_keys(labels) AS key
		              WHERE tenant_id = $1 AND project_id = $2 AND ts >= $3 AND name <> 'funnel'
		              GROUP BY name) k ON k.name = m.name
		 ORDER BY m.name`, tenantID, projectID, since)
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

// CatalogFunnels groups the `funnel` metric's readings into funnels and their
// steps. The order is the `i` label of the newest reading of each step; a step
// whose `i` is not a number sorts last rather than failing the read.
func (s *Store) CatalogFunnels(ctx context.Context, tenantID, projectID int64, since time.Time) ([]CatalogFunnel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT funnel, step FROM (
			SELECT labels->>'funnel' AS funnel, labels->>'step' AS step,
			       (array_agg(labels->>'i' ORDER BY ts DESC))[1] AS i
			  FROM metrics
			 WHERE tenant_id = $1 AND project_id = $2 AND ts >= $3 AND name = 'funnel'
			   AND labels->>'funnel' IS NOT NULL AND labels->>'step' IS NOT NULL
			 GROUP BY 1, 2
		) f
		 ORDER BY funnel, (CASE WHEN i ~ '^-?[0-9]+$' THEN i::int END) NULLS LAST, step`,
		tenantID, projectID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogFunnel{}
	at := map[string]int{}
	for rows.Next() {
		var name, step string
		if err := rows.Scan(&name, &step); err != nil {
			return nil, err
		}
		i, ok := at[name]
		if !ok {
			i = len(out)
			at[name] = i
			out = append(out, CatalogFunnel{Name: name, Steps: []string{}})
		}
		out[i].Steps = append(out[i].Steps, step)
	}
	return out, rows.Err()
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
		       count(*)::bigint
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
		if err := rows.Scan(&b.Index, &b.SumMs, &b.OK, &b.Total); err != nil {
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
	for rows.Next() {
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
