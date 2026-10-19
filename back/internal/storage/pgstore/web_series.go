// The board's web reads: engaged time and reach out of web_engage, and the
// per-page heat sums out of web_heat. Nothing here reads the log ring.

package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// EngageBucket is one bucket of web_engage sums, indexed from the range's
// start. Sums, so a wider span is the sum of its buckets and a ratio over it
// is a division rather than an average of averages. R holds r10, r30, r120
// and r300 in that order.
type EngageBucket struct {
	Index int64
	Views int64
	MS    int64
	R     [4]int64
}

// EngageBuckets sums web_engage into buckets of stepSeconds over [from, to),
// narrowed to one path, device, segment and source when each is non-empty.
func (s *Store) EngageBuckets(ctx context.Context, tenantID, projectID int64, path, device, segment, source string, from, to time.Time, stepSeconds int) ([]EngageBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT floor((extract(epoch from hour)::float8 - $3) / $4)::bigint AS bucket,
		       sum(views)::bigint, sum(ms)::bigint, sum(r10)::bigint, sum(r30)::bigint, sum(r120)::bigint, sum(r300)::bigint
		  FROM web_engage
		 WHERE tenant_id = $1 AND project_id = $2 AND hour >= $5 AND hour < $6
		   AND ($7 = '' OR path = $7) AND ($8 = '' OR device = $8)
		   AND ($9 = '' OR segment = $9) AND ($10 = '' OR source = $10)
		 GROUP BY bucket ORDER BY bucket`,
		tenantID, projectID, float64(from.Unix()), float64(stepSeconds), from, to, path, device, segment, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EngageBucket{}
	for rows.Next() {
		var b EngageBucket
		if err := rows.Scan(&b.Index, &b.Views, &b.MS, &b.R[0], &b.R[1], &b.R[2], &b.R[3]); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// EngagePaths ranks the pages by average engaged seconds per view over
// [from, to), only pages with a view, busiest first, capped at maxGroupRows.
func (s *Store) EngagePaths(ctx context.Context, tenantID, projectID int64, device, segment, source string, from, to time.Time) ([]LabelSum, error) {
	return pathSums(s.pool.Query(ctx, `
		SELECT path, sum(ms)::float8 / sum(views)::float8 / 1000
		  FROM web_engage
		 WHERE tenant_id = $1 AND project_id = $2 AND hour >= $3 AND hour < $4 AND ($5 = '' OR device = $5)
		   AND ($7 = '' OR segment = $7) AND ($8 = '' OR source = $8)
		 GROUP BY path HAVING sum(views) > 0
		 ORDER BY 2 DESC, path LIMIT $6`,
		tenantID, projectID, from, to, device, maxGroupRows, segment, source))
}

// HeatPaths ranks the pages by one heat kind's summed n since fromDay. The
// caller snaps fromDay to a Monday when the window reaches the weekly rows.
func (s *Store) HeatPaths(ctx context.Context, tenantID, projectID int64, kind, device, segment, source string, fromDay time.Time) ([]LabelSum, error) {
	return pathSums(s.pool.Query(ctx, `
		SELECT path, sum(n)::float8
		  FROM web_heat
		 WHERE tenant_id = $1 AND project_id = $2 AND kind = $3 AND day >= $4 AND ($5 = '' OR device = $5)
		   AND ($7 = '' OR segment = $7) AND ($8 = '' OR source = $8)
		 GROUP BY path
		 ORDER BY 2 DESC, path LIMIT $6`,
		tenantID, projectID, kind, fromDay, device, maxGroupRows, segment, source))
}

// pathSums reads (path, value) rows into labelled sums keyed "path".
func pathSums(rows pgx.Rows, err error) ([]LabelSum, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LabelSum{}
	for rows.Next() {
		var path string
		var v float64
		if err := rows.Scan(&path, &v); err != nil {
			return nil, err
		}
		out = append(out, LabelSum{Labels: map[string]string{"path": path}, Sum: v})
	}
	return out, rows.Err()
}
