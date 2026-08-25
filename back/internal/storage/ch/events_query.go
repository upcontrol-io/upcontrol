package ch

import (
	"context"
	"sort"
	"time"
)

// EventsAround returns the events in [from, to] CLOSEST to `at`, returned
// in time order; bounded by limit and window (this runs on the card's read path).
func (c *Conn) EventsAround(ctx context.Context, tenantID int64, from, to, at time.Time, limit int) ([]EventRow, error) {
	rows, err := c.db.Query(ctx, `
		SELECT ts, name, labels, amount_minor, currency
		  FROM events
		 WHERE tenant_id = ? AND ts >= ? AND ts <= ?
		 ORDER BY abs(dateDiff('millisecond', ts, ?)) ASC, ts ASC
		 LIMIT ?`, tenantID, from, to, at, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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
func (c *Conn) LastDeployAt(ctx context.Context, tenantID, projectID int64) (time.Time, error) {
	rows, err := c.db.Query(ctx, `
		SELECT max(ts) FROM events
		 WHERE tenant_id = ? AND project_id = ?
		   AND (name LIKE 'deploy%' OR name LIKE '%deployment%')`,
		uint64(tenantID), uint64(projectID))
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = rows.Close() }()

	var t time.Time
	if rows.Next() {
		if err := rows.Scan(&t); err != nil {
			return time.Time{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, err
	}
	// max() over an empty set yields the DateTime zero (the Unix epoch), not
	// Go's zero time — normalize it so "no deploy" compares equal everywhere.
	if t.Unix() <= 0 {
		return time.Time{}, nil
	}
	return t, nil
}
