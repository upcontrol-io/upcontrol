package ch

import (
	"context"
	"math"
	"time"
)

// ErrorWindow sums the 1-minute rollup over [from, to): error+fatal lines
// and all lines. series_1m is a SummingMergeTree: only read through sum().
func (c *Conn) ErrorWindow(ctx context.Context, tenantID, projectID int64, from, to time.Time) (errs, total uint64, err error) {
	rows, err := c.db.Query(ctx, `
		SELECT sumIf(lines, level IN ('error','fatal')) AS errs, sum(lines) AS total
		  FROM series_1m
		 WHERE tenant_id = ? AND project_id = ? AND minute >= ? AND minute < ?`,
		uint64(tenantID), uint64(projectID), from, to)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		if err := rows.Scan(&errs, &total); err != nil {
			return 0, 0, err
		}
	}
	return errs, total, rows.Err()
}

// ErrorRateBaseline computes the median and MAD of the per-5-minute error
// rate over [from, to); an empty baseline is the (0, 0, nil) contract, not NaN.
func (c *Conn) ErrorRateBaseline(ctx context.Context, tenantID, projectID int64, from, to time.Time) (median, mad float64, err error) {
	mRows, err := c.db.Query(ctx, `
		SELECT quantile(0.5)(rate) FROM (
			SELECT toStartOfFiveMinutes(minute) AS b,
			       sumIf(lines, level IN ('error','fatal')) / sum(lines) AS rate
			  FROM series_1m
			 WHERE tenant_id = ? AND project_id = ? AND minute >= ? AND minute < ?
			 GROUP BY b HAVING sum(lines) > 0
		)`,
		uint64(tenantID), uint64(projectID), from, to)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = mRows.Close() }()

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

	dRows, err := c.db.Query(ctx, `
		SELECT quantile(0.5)(abs(rate - ?)) FROM (
			SELECT toStartOfFiveMinutes(minute) AS b,
			       sumIf(lines, level IN ('error','fatal')) / sum(lines) AS rate
			  FROM series_1m
			 WHERE tenant_id = ? AND project_id = ? AND minute >= ? AND minute < ?
			 GROUP BY b HAVING sum(lines) > 0
		)`,
		median, uint64(tenantID), uint64(projectID), from, to)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = dRows.Close() }()

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
