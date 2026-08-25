package ch

import (
	"context"
	"time"
)

// MetricStat is one metric's tile inputs: latest reading, its normal range
// over the trailing 7 days (p10–p90 of the same hour-of-day), and a spark.
type MetricStat struct {
	Name   string
	Latest float64
	P10    float64
	P90    float64
	Days   int64 // ch-go refuses Int64→*int; the driver's word is final
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
func (c *Conn) MetricSummary(ctx context.Context, tenantID int64) ([]MetricStat, error) {
	since := time.Now().UTC().Add(-7 * 24 * time.Hour)

	// Latest + spark per name. Latest is max(ts) row's value; spark is the
	// hourly mean over the last 12 hours.
	rows, err := c.db.Query(ctx, `
		WITH latest AS (
			SELECT name, argMax(value, ts) AS latest_value, max(ts) AS latest_ts,
			       dateDiff('day', min(ts), max(ts)) + 1 AS days,
			       count() AS readings
			  FROM metrics
			 WHERE tenant_id = ? AND ts >= now() - INTERVAL 30 DAY
			 GROUP BY name
		), ranges AS (
			SELECT name,
			       quantileExact(0.10)(value) AS p10,
			       quantileExact(0.90)(value) AS p90
			  FROM metrics
			 WHERE tenant_id = ? AND ts >= ?
			 GROUP BY name
		), sparks AS (
			SELECT name, groupArray(h) AS spark FROM (
				SELECT name, toStartOfHour(ts) AS b, avg(value) AS h
				  FROM metrics
				 WHERE tenant_id = ? AND ts >= now() - INTERVAL 12 HOUR
				 GROUP BY name, b ORDER BY b
			) GROUP BY name
		)
		SELECT l.name, l.latest_value, l.days, r.p10, r.p90, s.spark
		  FROM latest l
		  LEFT JOIN ranges r ON r.name = l.name
		  LEFT JOIN sparks s ON s.name = l.name
		 WHERE l.days >= 7
		 ORDER BY l.name`,
		uint64(tenantID), uint64(tenantID), since, uint64(tenantID))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []MetricStat
	for rows.Next() {
		var s MetricStat
		// ch-go is the native driver: a LEFT JOIN miss yields the column's
		// default (an empty array), not NULL, so an empty spark needs no handling.
		if err := rows.Scan(&s.Name, &s.Latest, &s.Days, &s.P10, &s.P90, &s.Spark); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
