// The web door's Postgres half: a page view is an ordinary events row, and
// everything else a page did lands in web_heat, aggregated per path, device,
// day and element cell. The heatmap link tokens and the day's visitor salt
// live here too. Nothing in this file reaches the log ring.

package pgstore

import (
	"context"
	"errors"
	"time"

	cryptorand "crypto/rand"

	"github.com/jackc/pgx/v5"
)

// HeatAdd is one cell a beacon adds into: the count rides N. The caller merges
// duplicate keys first — Postgres refuses to update one row twice in a
// statement.
type HeatAdd struct {
	Kind     string
	Selector string
	FX       int16
	FY       int16
	N        int64
}

// HeatCell is one aggregated cell a heatmap read answers, in the Heatmap
// contract's wire shape.
type HeatCell struct {
	Selector string `json:"selector"`
	X        int16  `json:"x"`
	Y        int16  `json:"y"`
	N        int64  `json:"n"`
}

// InsertPageview stores one page view as a named event; the actor is the
// web door's cookieless visitor hash.
func (s *Store) InsertPageview(ctx context.Context, tenantID, projectID int64, ts time.Time, labels map[string]string, actor string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO events (tenant_id, project_id, ts, name, labels, actor) VALUES ($1,$2,$3,'uc.pageview',$4,$5)`,
		tenantID, projectID, ts, jsonb(labels), actor)
	return err
}

// AddHeat adds a beacon's cells into web_heat as one statement. The caller
// guarantees no duplicate (kind, selector, fx, fy) keys in cells. The rows go
// in key order, so two beacons sharing cells lock them in the same order; in
// the caller's map order they deadlock.
func (s *Store) AddHeat(ctx context.Context, tenantID, projectID int64, day time.Time, path, device string, cells []HeatAdd) error {
	if len(cells) == 0 {
		return nil
	}
	kinds := make([]string, len(cells))
	sels := make([]string, len(cells))
	fx := make([]int16, len(cells))
	fy := make([]int16, len(cells))
	ns := make([]int64, len(cells))
	for i, c := range cells {
		kinds[i], sels[i], fx[i], fy[i], ns[i] = c.Kind, c.Selector, c.FX, c.FY, c.N
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO web_heat (tenant_id, project_id, day, path, device, kind, selector, fx, fy, n)
		SELECT $1,$2,$3,$4,$5,k,s,x,y,n
		  FROM unnest($6::text[], $7::text[], $8::smallint[], $9::smallint[], $10::bigint[]) AS t(k,s,x,y,n)
		 ORDER BY k, s, x, y
		ON CONFLICT (tenant_id, project_id, path, device, day, kind, selector, fx, fy)
		DO UPDATE SET n = web_heat.n + EXCLUDED.n`,
		tenantID, projectID, day, path, device, kinds, sels, fx, fy, ns)
	return err
}

// DailySalt returns the day's visitor salt, minting it on first sight and
// dropping days that are over: a salt lives exactly one day, which is what
// makes a visitor impossible to follow from one day to the next.
func (s *Store) DailySalt(ctx context.Context, day time.Time) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := cryptorand.Read(salt); err != nil {
		return nil, err
	}
	// The no-op update makes RETURNING answer the stored salt on a conflict.
	var out []byte
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO web_salt (day, salt) VALUES ($1,$2) ON CONFLICT (day) DO UPDATE SET salt = web_salt.salt RETURNING salt`,
		day, salt).Scan(&out); err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM web_salt WHERE day < $1`, day); err != nil {
		return nil, err
	}
	return out, nil
}

// PublicOrigins lists the origins of the project's live public keys, newest
// key first, flattened and de-duplicated in order.
func (s *Store) PublicOrigins(ctx context.Context, tenantID, projectID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT origins FROM api_key
		  WHERE tenant_id=$1 AND project_id=$2 AND kind='public' AND state IN ('active','rotating')
		  ORDER BY created_at DESC, id DESC`, tenantID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := []string{}
	for rows.Next() {
		var origins []string
		if err := rows.Scan(&origins); err != nil {
			return nil, err
		}
		for _, o := range origins {
			if o != "" && !seen[o] {
				seen[o] = true
				out = append(out, o)
			}
		}
	}
	return out, rows.Err()
}

// MintHeatmapLink stores a link token's hash, one hour of life. Expired rows
// go first; the plaintext never leaves the response.
func (s *Store) MintHeatmapLink(ctx context.Context, tokenHash []byte, tenantID, projectID int64, expires time.Time) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM heatmap_link WHERE expires_at < now()`); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO heatmap_link (token_hash, tenant_id, project_id, expires_at) VALUES ($1,$2,$3,$4)`,
		tokenHash, tenantID, projectID, expires)
	return err
}

// ResolveHeatmapLink reads a link token back; ok=false is an unknown or
// expired token, not an error.
func (s *Store) ResolveHeatmapLink(ctx context.Context, tokenHash []byte) (tenantID, projectID int64, ok bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT tenant_id, project_id FROM heatmap_link WHERE token_hash=$1 AND expires_at > now()`,
		tokenHash).Scan(&tenantID, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return tenantID, projectID, true, nil
}

// PageviewCount counts the page views of one path on one device since `from`.
func (s *Store) PageviewCount(ctx context.Context, tenantID, projectID int64, path, device string, from time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM events
		  WHERE tenant_id=$1 AND project_id=$2 AND name='uc.pageview' AND actor <> ''
		    AND labels->>'path'=$3 AND labels->>'device'=$4 AND ts >= $5`,
		tenantID, projectID, path, device, from).Scan(&n)
	return n, err
}

// HeatCells reads one kind's cells since fromDay, busiest first, capped: a
// map with thousands of cells is not a readable overlay.
func (s *Store) HeatCells(ctx context.Context, tenantID, projectID int64, path, device, kind string, fromDay time.Time, limit int) ([]HeatCell, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT selector, fx, fy, sum(n) FROM web_heat
		  WHERE tenant_id=$1 AND project_id=$2 AND path=$3 AND device=$4 AND kind=$5 AND day >= $6
		  GROUP BY 1,2,3 ORDER BY 4 DESC LIMIT $7`,
		tenantID, projectID, path, device, kind, fromDay, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HeatCell{}
	for rows.Next() {
		var c HeatCell
		if err := rows.Scan(&c.Selector, &c.X, &c.Y, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ScrollReach reads the scroll fold as the overlay draws it: out[i] is the
// page views whose deepest reach was at least i twentieths, cumulative from
// the deepest.
func (s *Store) ScrollReach(ctx context.Context, tenantID, projectID int64, path, device string, fromDay time.Time) ([21]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT fy, sum(n) FROM web_heat
		  WHERE tenant_id=$1 AND project_id=$2 AND path=$3 AND device=$4 AND kind='scroll' AND day >= $5
		  GROUP BY 1`, tenantID, projectID, path, device, fromDay)
	if err != nil {
		return [21]int64{}, err
	}
	defer rows.Close()
	var per [21]int64
	for rows.Next() {
		var fy int
		var n int64
		if err := rows.Scan(&fy, &n); err != nil {
			return [21]int64{}, err
		}
		if fy >= 0 && fy < 21 {
			per[fy] += n
		}
	}
	if err := rows.Err(); err != nil {
		return [21]int64{}, err
	}
	var out [21]int64
	var run int64
	for i := 20; i >= 0; i-- {
		run += per[i]
		out[i] = run
	}
	return out, nil
}
