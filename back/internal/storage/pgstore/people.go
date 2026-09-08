// The people reads: the four answers the SDK's feeds computed on the client,
// moved onto the events table once an event carries who did it. One law holds
// in every query here: count(*) is events, count(DISTINCT actor) over rows
// with actor <> '' is people — an event with nobody behind it is a real event
// and must never inflate a people count. Logs and metrics keep their own
// files; these four read events only.

package pgstore

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// peopleCount is the one expression every people answer is made of. The
// actor <> ” in each WHERE keeps nobody-behind-it rows out before the count,
// so the DISTINCT sees people and only people; spelling the count once is
// what keeps the two readings apart everywhere.
const peopleCount = "count(DISTINCT actor) AS people"

// FunnelSteps counts the distinct people at each of the caller's step names
// over [from, to), one row per step IN THE ORDER GIVEN, zeros for steps
// nothing matched — a funnel's shape is itself the reading. A person at a
// step is a person with any event of that step's name: the SDK's funnel never
// enforced an order either, and silently changing what a number means is
// worse than keeping a loose one. The step list rides one array parameter;
// its width is the answer's width, so it is capped like every grouped read.
func (s *Store) FunnelSteps(ctx context.Context, tenantID, projectID int64, steps []string, from, to time.Time) ([]LabelSum, error) {
	if len(steps) == 0 || len(steps) > maxGroupRows {
		return nil, fmt.Errorf("a funnel takes one to %d steps", maxGroupRows)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT name, `+peopleCount+`
		  FROM events
		 WHERE tenant_id = $1 AND project_id = $2 AND name = ANY($3)
		   AND actor <> '' AND ts >= $4 AND ts < $5
		 GROUP BY name`,
		tenantID, projectID, steps, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var step string
		var people int64
		if err := rows.Scan(&step, &people); err != nil {
			return nil, err
		}
		counts[step] = people
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]LabelSum, len(steps))
	for i, step := range steps {
		out[i] = LabelSum{Labels: map[string]string{"step": step}, Sum: float64(counts[step])}
	}
	return out, nil
}

// BreakdownValues counts the distinct people per value of one label over one
// event name, most people first, capped like every grouped read. Rows the
// label is absent from are dropped, matching CounterGroups: a row with no
// value answers no value.
func (s *Store) BreakdownValues(ctx context.Context, tenantID, projectID int64, name, labelKey string, from, to time.Time) ([]LabelSum, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT labels->>$3 AS g, `+peopleCount+`
		  FROM events
		 WHERE tenant_id = $1 AND project_id = $2 AND name = $4
		   AND actor <> '' AND labels->>$3 IS NOT NULL AND ts >= $5 AND ts < $6
		 GROUP BY g ORDER BY people DESC LIMIT $7`,
		tenantID, projectID, labelKey, name, from, to, maxGroupRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LabelSum{}
	for rows.Next() {
		var value string
		var people int64
		if err := rows.Scan(&value, &people); err != nil {
			return nil, err
		}
		out = append(out, LabelSum{
			Labels: map[string]string{labelKey: value},
			Sum:    float64(people),
		})
	}
	return out, rows.Err()
}

// RetentionCohorts grids people by the week they were first seen: an actor's
// cohort is the Monday of their first event EVER — not their first inside the
// window — and each cell counts that cohort's people active at a week offset.
// Weeks are Monday-aligned in pure epoch arithmetic, so no date_trunc ever
// runs in a session timezone: 259200 s is the gap from the Unix epoch (a
// Thursday) back to its week's Monday, so floor((epoch + 259200) / 604800)
// is any timestamp's week index and week w's Monday 00:00 UTC is
// w·604800 − 259200. Only cohorts whose Monday falls inside [from, to)
// answer: a cohort that began earlier would come back as a fraction of
// itself, and the front already draws on that rule.
func (s *Store) RetentionCohorts(ctx context.Context, tenantID, projectID int64, from, to time.Time) ([]LabelSum, error) {
	rows, err := s.pool.Query(ctx, `
		WITH firsts AS (
			SELECT actor, floor((extract(epoch from min(ts)) + 259200) / 604800)::bigint AS cohort
			  FROM events
			 WHERE tenant_id = $1 AND project_id = $2 AND actor <> ''
			 GROUP BY actor
		), active AS (
			SELECT actor, floor((extract(epoch from ts) + 259200) / 604800)::bigint AS wk
			  FROM events
			 WHERE tenant_id = $1 AND project_id = $2 AND actor <> '' AND ts >= $3 AND ts < $4
		)
		SELECT f.cohort * 604800 - 259200 AS monday, active.wk - f.cohort AS week, `+peopleCount+`
		  FROM active JOIN firsts f USING (actor)
		 WHERE f.cohort * 604800 - 259200 >= $5 AND f.cohort * 604800 - 259200 < $6
		 GROUP BY monday, week ORDER BY monday, week LIMIT $7`,
		tenantID, projectID, from, to, from.Unix(), to.Unix(), maxGroupRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LabelSum{}
	for rows.Next() {
		var monday, week, people int64
		if err := rows.Scan(&monday, &week, &people); err != nil {
			return nil, err
		}
		out = append(out, LabelSum{
			Labels: map[string]string{
				"cohort": time.Unix(monday, 0).UTC().Format("2006-01-02"),
				"week":   strconv.FormatInt(week, 10),
			},
			Sum: float64(people),
		})
	}
	return out, rows.Err()
}

// ExperimentArms counts the distinct people per variant for each of an
// experiment's two event names, the exposed one and the converted one. The
// two names ride one array parameter; a variant is the value of one label
// key, and a row without it is dropped — an event that names no arm answers
// no arm.
func (s *Store) ExperimentArms(ctx context.Context, tenantID, projectID int64, variantKey, exposedName, convertedName string, from, to time.Time) ([]LabelSum, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, labels->>$3 AS variant, `+peopleCount+`
		  FROM events
		 WHERE tenant_id = $1 AND project_id = $2 AND name = ANY($4)
		   AND actor <> '' AND labels->>$3 IS NOT NULL AND ts >= $5 AND ts < $6
		 GROUP BY name, variant ORDER BY people DESC LIMIT $7`,
		tenantID, projectID, variantKey, []string{exposedName, convertedName}, from, to, maxGroupRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LabelSum{}
	for rows.Next() {
		var name, variant string
		var people int64
		if err := rows.Scan(&name, &variant, &people); err != nil {
			return nil, err
		}
		out = append(out, LabelSum{
			Labels: map[string]string{"event": name, variantKey: variant},
			Sum:    float64(people),
		})
	}
	return out, rows.Err()
}
