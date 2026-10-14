// The board's marks: what happened on a project's clock that a chart draws
// beside its line, so a reading that moved carries the change that moved it.
// Each read answers raw rows, the latest marksCap of its kind, oldest first;
// the API layer folds a burst of deploys into one and words every label.

package pgstore

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// MarkRow is one row a mark is made from. Name is the deploy event's name, the
// release's value or the incident's title; SHA is a deploy's labels.sha, ""
// when it carried none; Until is an incident's resolved_at, nil while it is
// open and for the other two kinds.
type MarkRow struct {
	At    time.Time
	Until *time.Time
	Name  string
	SHA   string
}

// marksCap is how many rows of each kind one read keeps: the latest ones. For
// deploys that counts events, before the API layer folds a burst of them into
// one mark, so a pipeline that reports a deploy as three events reaches back a
// third as many deploys.
const marksCap = 100

// releaseLookback is how far before the window a release's first sighting is
// looked for: a release already running when the window opened is not new
// inside it, and only a scan that starts earlier can tell.
const releaseLookback = 30 * 24 * time.Hour

// deployName is LastDeployAt's name predicate (reads.go) plus one rule: no
// whitespace in the name, not just a space. An unmarked log line that merely
// starts with "deploy" is stored as an event named after its whole message
// (normalize.Classify), and a sentence is not a deploy, whether its words are
// split by a space, a tab or a line break.
const deployName = `(name LIKE 'deploy%' OR name LIKE '%deployment%') AND name !~ '\s'`

// DeployMarks reads the project's deploy events inside [from, to).
func (s *Store) DeployMarks(ctx context.Context, tenantID, projectID int64, from, to time.Time) ([]MarkRow, error) {
	return s.markRows(ctx, `
		SELECT ts, NULL::timestamptz, name, COALESCE(labels->>'sha', '')
		  FROM events
		 WHERE tenant_id = $1 AND project_id = $2 AND ts >= $3 AND ts < $4
		   AND `+deployName+`
		 ORDER BY ts DESC LIMIT $5`,
		tenantID, projectID, from, to, marksCap)
}

// ReleaseMarks reads the first sighting of each release that falls inside
// [from, to). A release is app_started's `release` label, else its `version`;
// the SDK reports a package with no version as "0", which names nothing.
func (s *Store) ReleaseMarks(ctx context.Context, tenantID, projectID int64, from, to time.Time) ([]MarkRow, error) {
	// ponytail: the scan reads every app_started row of the span, the lookback
	// included; a release table comes when the slow-read log shows the need.
	return s.markRows(ctx, `
		SELECT first_seen, NULL::timestamptz, rel, '' FROM (
			SELECT COALESCE(NULLIF(labels->>'release', ''), labels->>'version') AS rel, min(ts) AS first_seen
			  FROM events
			 WHERE tenant_id = $1 AND project_id = $2 AND name = 'app_started'
			   AND ts >= $3 AND ts < $4
			 GROUP BY rel
		) releases
		 WHERE first_seen >= $5 AND rel NOT IN ('', '0')
		 ORDER BY first_seen DESC LIMIT $6`,
		tenantID, projectID, from.Add(-releaseLookback), to, from, marksCap)
}

// IncidentMarks reads the project's incidents that overlap [from, to): opened
// before it ends, and still open or resolved after it began.
func (s *Store) IncidentMarks(ctx context.Context, tenantID, projectID int64, from, to time.Time) ([]MarkRow, error) {
	return s.markRows(ctx, `
		SELECT detected_at, resolved_at, title, ''
		  FROM incident
		 WHERE tenant_id = $1 AND project_id = $2
		   AND detected_at < $4 AND (resolved_at IS NULL OR resolved_at >= $3)
		 ORDER BY detected_at DESC LIMIT $5`,
		tenantID, projectID, from, to, marksCap)
}

// markRows runs one of the reads above. Each reads newest first, so the cap
// keeps the latest rows, and answers oldest first, the order a chart draws in.
func (s *Store) markRows(ctx context.Context, sql string, args ...any) ([]MarkRow, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[MarkRow])
	slices.Reverse(out)
	return out, err
}
