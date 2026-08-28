// The read half of the store, mirroring the ch.Conn read methods one for one
// so a caller that switches type keeps everything else — including the Go
// arithmetic over the detector's baseline.
package pgstore

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Raw exposes the underlying pool for callers that need arbitrary SQL; logs
// reads stay behind ring.QueryBuilder; this is the seam.
func (s *Store) Raw() *pgxpool.Pool { return s.pool }

// EventsAround returns the events in [from, to] CLOSEST to `at`, returned
// in time order; bounded by limit and window (this runs on the card's read path).
func (s *Store) EventsAround(ctx context.Context, tenantID int64, from, to, at time.Time, limit int) ([]EventRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ts, name, labels, amount_minor, currency
		  FROM events
		 WHERE tenant_id = $1 AND ts >= $2 AND ts <= $3
		 ORDER BY abs(extract(epoch from ($4 - ts))) ASC, ts ASC
		 LIMIT $5`, tenantID, from, to, at, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EventRow
	for rows.Next() {
		var e EventRow
		if err := rows.Scan(&e.TS, &e.Name, &e.Labels, &e.AmountMinor, &e.Currency); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Chosen by distance from the pivot, returned in time order: the caller
	// renders a chronology, and the pivot is only how the budget was spent.
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, nil
}

// LastDeployAt is the project's most recent deploy event (zero time = none);
// the name predicate must stay in sync with eventKind (read_api.go:723).
func (s *Store) LastDeployAt(ctx context.Context, tenantID, projectID int64) (time.Time, error) {
	var t pgtype.Timestamptz
	if err := s.pool.QueryRow(ctx, `
		SELECT max(ts) FROM events
		 WHERE tenant_id = $1 AND project_id = $2
		   AND (name LIKE 'deploy%' OR name LIKE '%deployment%')`,
		tenantID, projectID).Scan(&t); err != nil {
		return time.Time{}, err
	}
	// max() over an empty set is NULL here (ClickHouse answered the Unix
	// epoch) — normalize either so "no deploy" compares equal everywhere.
	if !t.Valid || t.Time.Unix() <= 0 {
		return time.Time{}, nil
	}
	return t.Time, nil
}

// MetricStat is one metric's tile inputs: latest reading, its normal range
// over the trailing 7 days (p10–p90 of the same hour-of-day), and a spark.
type MetricStat struct {
	Name   string
	Latest float64
	P10    float64
	P90    float64
	Days   int64
	Spark  []float64
}

// metricTileUnits names the metrics the Dashboard's product tiles know; a
// metric with no entry is read but never invented into a tile.
var metricTileUnits = map[string]string{
	"signups":             "",
	"checkout_latency_ms": "ms",
	"checkout_latency":    "ms",
	"latency_ms":          "ms",
	"signups_today":       "",
	"checkouts":           "",
	"checkouts_today":     "",
}

// MetricTileUnits exposes the known-names map for the API layer's tile
// builder. Read-only by convention: callers must not mutate.
func MetricTileUnits() map[string]string { return metricTileUnits }

// MetricSummary reads the tenant's metrics for the tiles: latest value of
// every named metric with 7+ days of history, its range, and a 12-point spark.
func (s *Store) MetricSummary(ctx context.Context, tenantID int64) ([]MetricStat, error) {
	since := time.Now().UTC().Add(-7 * 24 * time.Hour)

	// Latest + spark per name. Latest is max(ts) row's value; spark is the
	// hourly mean over the last 12 hours. The COALESCEs keep the LEFT JOIN's
	// misses at ClickHouse's column defaults (0, empty array), not NULLs.
	rows, err := s.pool.Query(ctx, `
		WITH latest AS (
			SELECT name, (array_agg(value ORDER BY ts DESC))[1] AS latest_value, max(ts) AS latest_ts,
			       (extract(epoch from (max(ts) - min(ts))) / 86400)::bigint + 1 AS days,
			       count(*) AS readings
			  FROM metrics
			 WHERE tenant_id = $1 AND ts >= now() - INTERVAL '30 days'
			 GROUP BY name
		), ranges AS (
			SELECT name,
			       percentile_cont(0.10) WITHIN GROUP (ORDER BY value) AS p10,
			       percentile_cont(0.90) WITHIN GROUP (ORDER BY value) AS p90
			  FROM metrics
			 WHERE tenant_id = $2 AND ts >= $3
			 GROUP BY name
		), sparks AS (
			SELECT name, array_agg(h ORDER BY b) AS spark FROM (
				SELECT name, date_trunc('hour', ts) AS b, avg(value)::float8 AS h
				  FROM metrics
				 WHERE tenant_id = $4 AND ts >= now() - INTERVAL '12 hours'
				 GROUP BY name, b
			) hourly GROUP BY name
		)
		SELECT l.name, l.latest_value, l.days,
		       COALESCE(r.p10, 0), COALESCE(r.p90, 0),
		       COALESCE(s.spark, ARRAY[]::float8[])
		  FROM latest l
		  LEFT JOIN ranges r ON r.name = l.name
		  LEFT JOIN sparks s ON s.name = l.name
		 WHERE l.days >= 7
		 ORDER BY l.name`,
		tenantID, tenantID, since, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MetricStat
	for rows.Next() {
		var st MetricStat
		if err := rows.Scan(&st.Name, &st.Latest, &st.Days, &st.P10, &st.P90, &st.Spark); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ErrorWindow sums the 1-minute rollup over [from, to): error+fatal lines
// and all lines. COALESCE holds the empty window at 0, where an aggregate
// over no rows is NULL.
func (s *Store) ErrorWindow(ctx context.Context, tenantID, projectID int64, from, to time.Time) (errs, total uint64, err error) {
	var e, t int64
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(lines) FILTER (WHERE level IN ('error','fatal')), 0)::bigint AS errs,
		       COALESCE(sum(lines), 0)::bigint AS total
		  FROM series_1m
		 WHERE tenant_id = $1 AND project_id = $2 AND minute >= $3 AND minute < $4`,
		tenantID, projectID, from, to).Scan(&e, &t); err != nil {
		return 0, 0, err
	}
	return uint64(e), uint64(t), nil
}

// baselineRates is the per-5-minute error rate the baseline medians: buckets
// with no traffic are excluded (HAVING), because they have no rate at all.
const baselineRates = `
	SELECT to_timestamp(floor(extract(epoch from minute) / 300) * 300) AS b,
	       (sum(lines) FILTER (WHERE level IN ('error','fatal')))::float8
	         / sum(lines)::float8 AS rate
	  FROM series_1m
	 WHERE tenant_id = $1 AND project_id = $2 AND minute >= $3 AND minute < $4
	 GROUP BY b HAVING sum(lines) > 0`

// ErrorRateBaseline computes the median and MAD of the per-5-minute error
// rate over [from, to); an empty baseline is the (0, 0, nil) contract, not NaN.
// ClickHouse's quantile answered NaN for an empty set — percentile_cont
// answers NULL, so it is COALESCEd back to NaN and the same check reads it.
func (s *Store) ErrorRateBaseline(ctx context.Context, tenantID, projectID int64, from, to time.Time) (median, mad float64, err error) {
	mRows, err := s.pool.Query(ctx, `
		SELECT COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY rate), 'NaN'::float8)
		  FROM (`+baselineRates+`) rates`,
		tenantID, projectID, from, to)
	if err != nil {
		return 0, 0, err
	}
	defer mRows.Close()

	for mRows.Next() {
		if err := mRows.Scan(&median); err != nil {
			return 0, 0, err
		}
	}
	if err := mRows.Err(); err != nil {
		return 0, 0, err
	}
	if math.IsNaN(median) {
		return 0, 0, nil
	}

	dRows, err := s.pool.Query(ctx, `
		SELECT COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY abs(rate - $5)), 'NaN'::float8)
		  FROM (`+baselineRates+`) rates`,
		tenantID, projectID, from, to, median)
	if err != nil {
		return 0, 0, err
	}
	defer dRows.Close()

	for dRows.Next() {
		if err := dRows.Scan(&mad); err != nil {
			return 0, 0, err
		}
	}
	if err := dRows.Err(); err != nil {
		return 0, 0, err
	}
	return median, mad, nil
}
