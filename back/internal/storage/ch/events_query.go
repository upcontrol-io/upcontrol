package ch

import (
	"context"
	"sort"
	"time"
)

// EventsAround returns the tenant's events in [from, to] that sit CLOSEST to
// `at`, oldest first.
//
// Closest to the pivot, not first in the window, and the difference is the
// whole point. The caller's window is deliberately wider than the moment it
// cares about — the incident card reaches 30 minutes back so a deploy can be
// joined to the break — and `ORDER BY ts ASC LIMIT n` spends the entire budget
// on the far end of that window. On any project busier than a couple of events
// a minute the limit was exhausted inside the first minute, so the card showed
// half an hour of ordinary traffic and never the break itself. The name of this
// function was already right; the query was not.
//
// Bounded by `limit` and by the window the caller passes, because this runs on
// the incident card's read path: a tenant with a busy webhook must not be able
// to make opening one incident scan a month of events.
//
// The read leaves TenantID/ProjectID zero — the caller already knows the
// tenant, and selecting them back would be two columns nobody reads.
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

// LastDeployAt is the project's most recent deploy event, or the zero time
// when there is none. The suppression window reads it: an error burst within
// 90s of a deploy is the deploy's fault, not a new incident.
//
// The name predicate must stay in sync with eventKind (read_api.go:723):
// the timeline marks a deploy by HasPrefix("deploy") || Contains("deployment"),
// and a deploy shape this query misses is a blip the suppressor cannot see.
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
