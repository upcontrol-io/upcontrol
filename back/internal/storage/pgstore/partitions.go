// Day partitions of the logs table: created ahead of the clock, dropped once
// they fall behind the floor. These partitions ARE the retention model — the
// floor is the widest plan window plus a day, so no plan loses days it sold.

package pgstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A logs partition is named after the UTC day it holds: logs_20260830.
const (
	partitionPrefix = "logs_"
	partitionLayout = "20060102"
)

// RollLogPartitions covers every UTC day from `keep` ago to `ahead` days out,
// then drops the partitions whose day ended more than `keep` before now. A
// partition whose name it cannot read is left alone: an operator made it, and
// dropping it would take data nobody agreed to lose. The returned names are
// what actually changed, so the caller logs work rather than intent.
//
// It reaches backwards as well as forwards because a row whose ts has no
// partition fails the whole COPY it rides in, and the batcher never retries a
// batch: one line stamped just before midnight would take a thousand good ones
// with it. The days behind are the ones `keep` promises to hold anyway.
func (s *Store) RollLogPartitions(ctx context.Context, now time.Time, ahead int, keep time.Duration) (created, dropped []string, err error) {
	existing, err := s.logPartitions(ctx)
	if err != nil {
		return nil, nil, err
	}

	utc := now.UTC()
	floor := utc.Add(-keep)
	last := midnightUTC(utc).AddDate(0, 0, ahead)
	for day := midnightUTC(floor); !day.After(last); day = day.AddDate(0, 0, 1) {
		name := partitionPrefix + day.Format(partitionLayout)
		if slices.Contains(existing, name) {
			continue
		}
		// Name and bounds are formatted from a date, never from caller input,
		// so this DDL cannot carry anything but a day.
		if _, err := s.pool.Exec(ctx, fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF logs FOR VALUES FROM ('%s') TO ('%s')",
			name, day.Format(time.RFC3339), day.AddDate(0, 0, 1).Format(time.RFC3339),
		)); err != nil {
			return created, dropped, fmt.Errorf("create %s: %w", name, err)
		}
		created = append(created, name)
	}

	// The floor's own day ends after the floor, so nothing created above is
	// dropped here.
	for _, name := range existing {
		day, ok := partitionDay(name)
		if !ok || !day.AddDate(0, 0, 1).Before(floor) {
			continue
		}
		if _, err := s.pool.Exec(ctx, "DROP TABLE "+name); err != nil {
			return created, dropped, fmt.Errorf("drop %s: %w", name, err)
		}
		dropped = append(dropped, name)
	}
	return created, dropped, nil
}

// logPartitions lists the current partitions of logs by name, sorted, and
// closes the read before the caller runs any DDL of its own.
func (s *Store) logPartitions(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.relname FROM pg_inherits i
		   JOIN pg_class c ON c.oid = i.inhrelid
		  WHERE i.inhparent = 'logs'::regclass
		  ORDER BY c.relname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func midnightUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// partitionDay reads the UTC day out of a logs_YYYYMMDD name. Anything else,
// including an operator's own logs_today, reports false and is never dropped.
func partitionDay(name string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(name, partitionPrefix)
	if !ok {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation(partitionLayout, rest, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}
