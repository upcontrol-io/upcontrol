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
//
// stepLabels[i] narrows step i to the rows whose labels hold its pairs; a
// missing or empty entry matches every row. A step is counted by its INDEX,
// so two steps of one name with different labels (two pages of
// `uc.pageview`) are counted apart.
func (s *Store) FunnelSteps(ctx context.Context, tenantID, projectID int64, steps []string, stepLabels []map[string]string, from, to time.Time) ([]LabelSum, error) {
	if len(steps) == 0 || len(steps) > maxGroupRows {
		return nil, fmt.Errorf("a funnel takes one to %d steps", maxGroupRows)
	}
	filters := make([]string, len(steps))
	for i := range steps {
		var l map[string]string
		if i < len(stepLabels) {
			l = stepLabels[i]
		}
		filters[i] = string(jsonb(l))
	}
	rows, err := s.pool.Query(ctx, `
		SELECT s.i, count(DISTINCT e.actor) AS people
		  FROM unnest($3::text[], $4::text[]) WITH ORDINALITY AS s(name, labels, i)
		  JOIN events e ON e.name = s.name AND (s.labels = '{}' OR e.labels @> s.labels::jsonb)
		 WHERE e.tenant_id = $1 AND e.project_id = $2
		   AND e.actor <> '' AND e.ts >= $5 AND e.ts < $6
		 GROUP BY s.i`,
		tenantID, projectID, steps, filters, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LabelSum, len(steps))
	for i, step := range steps {
		out[i] = LabelSum{Labels: map[string]string{"step": step}}
	}
	for rows.Next() {
		var i, people int64
		if err := rows.Scan(&i, &people); err != nil {
			return nil, err
		}
		out[i-1].Sum = float64(people)
	}
	return out, rows.Err()
}

// BreakdownValues ranks the values of one label over one event name, most
// first, capped like every grouped read. Rows the label is absent from are
// dropped, matching CounterGroups: a row with no value answers no value.
//
// It is the one read here with an axis: `countEvents` counts ROWS instead of
// distinct actors, because a dimension nobody is behind — a delivery outcome,
// an HTTP status, a queue name — has no actors at all, and every people read
// filters them out. Without this such a card reads 0 and says nothing about
// why. The actor filter goes with the aggregate rather than staying: counting
// events among rows that happen to have a person would answer a third
// question nobody asked. A non-empty `labels` folds only the rows holding its
// pairs.
func (s *Store) BreakdownValues(ctx context.Context, tenantID, projectID int64, name, labelKey string, labels map[string]string, countEvents bool, from, to time.Time) ([]LabelSum, error) {
	fold, actors := peopleCount, "AND actor <> ''"
	if countEvents {
		fold, actors = "count(*) AS events", ""
	}
	return s.breakdown(ctx, fold, actors, []any{tenantID, projectID, labelKey, name, from, to, maxGroupRows}, labelKey, labels)
}

// BreakdownAmounts ranks the values of one label by the money the event's rows
// carried in one currency, most first: the revenue per plan, per country. No
// actor filter, because money is money whoever paid, and a server-side charge
// often has nobody behind it. The currency is compared upper case, as
// AmountBuckets does.
func (s *Store) BreakdownAmounts(ctx context.Context, tenantID, projectID int64, name, labelKey, currency string, labels map[string]string, from, to time.Time) ([]LabelSum, error) {
	return s.breakdown(ctx, "COALESCE(sum(amount_minor), 0)::bigint AS amount", "AND upper(currency) = $8",
		[]any{tenantID, projectID, labelKey, name, from, to, maxGroupRows, currency}, labelKey, labels)
}

// breakdown is the one fold of a label's values over an event name: args bind
// $1..$7 as tenant, project, label key, name, from, to and the row cap, and
// `where` may bind more.
func (s *Store) breakdown(ctx context.Context, fold, where string, args []any, labelKey string, labels map[string]string) ([]LabelSum, error) {
	narrow, args := eventLabels(labels, args)
	// ORDER BY the ordinal, because the aggregate's name changes with what it counts.
	rows, err := s.pool.Query(ctx, `
		SELECT labels->>$3 AS g, `+fold+`
		  FROM events
		 WHERE tenant_id = $1 AND project_id = $2 AND name = $4
		   `+where+` AND labels->>$3 IS NOT NULL AND ts >= $5 AND ts < $6`+narrow+`
		 GROUP BY g ORDER BY 2 DESC LIMIT $7`,
		args...)
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
// itself, and the front already draws on that rule. The server's own `uc.`
// names stay out: a page view's actor is a visitor hash that changes every
// day, so each day's visitors would join as a new cohort and never return.
//
// A name narrows the grid to that one event's people: the cohort is the
// Monday of an actor's first event OF THAT NAME, ever, and a return is the
// same event in a later week. Empty is every event, read by the very statement
// the grid ran before names existed: the filter is spliced in rather than
// written as an OR on an empty parameter, so that statement keeps its plan and
// its partial index. A `uc.` name reads nothing: the exclusion above holds.
func (s *Store) RetentionCohorts(ctx context.Context, tenantID, projectID int64, name string, from, to time.Time) ([]LabelSum, error) {
	args := []any{tenantID, projectID, from, to, from.Unix(), to.Unix(), maxGroupRows}
	only := ""
	if name != "" {
		args = append(args, name)
		only = fmt.Sprintf(" AND name = $%d", len(args))
	}
	// actor NOT LIKE 'w:%': the web door's visitor is a daily hash, so it would
	// read as a new person every day.
	rows, err := s.pool.Query(ctx, `
		WITH firsts AS (
			SELECT actor, floor((extract(epoch from min(ts)) + 259200) / 604800)::bigint AS cohort
			  FROM events
			 WHERE tenant_id = $1 AND project_id = $2 AND actor <> '' AND name NOT LIKE 'uc.%' AND actor NOT LIKE 'w:%'`+only+`
			 GROUP BY actor
		), active AS (
			SELECT actor, floor((extract(epoch from ts) + 259200) / 604800)::bigint AS wk
			  FROM events
			 WHERE tenant_id = $1 AND project_id = $2 AND actor <> '' AND name NOT LIKE 'uc.%' AND actor NOT LIKE 'w:%'`+only+`
			   AND ts >= $3 AND ts < $4
		)
		SELECT f.cohort * 604800 - 259200 AS monday, active.wk - f.cohort AS week, `+peopleCount+`
		  FROM active JOIN firsts f USING (actor)
		 WHERE f.cohort * 604800 - 259200 >= $5 AND f.cohort * 604800 - 259200 < $6
		 GROUP BY monday, week ORDER BY monday, week LIMIT $7`,
		args...)
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

// ExperimentArms counts the distinct people per arm per stat over ONE event
// name: the wire writes an A/B test as a single name whose rows carry the arm
// in one label key and exposed/converted in another, so the read folds that
// name by the two keys the caller names. A row missing either key is dropped
// — an event naming no arm answers no arm, and one naming no stat answers no
// stat.
func (s *Store) ExperimentArms(ctx context.Context, tenantID, projectID int64,
	name, variantKey, statKey string, from, to time.Time) ([]LabelSum, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT labels->>$3 AS variant, labels->>$4 AS stat, `+peopleCount+`
		  FROM events
		 WHERE tenant_id = $1 AND project_id = $2 AND name = $5
		   AND actor <> '' AND labels->>$3 IS NOT NULL AND labels->>$4 IS NOT NULL
		   AND ts >= $6 AND ts < $7
		 GROUP BY variant, stat ORDER BY people DESC LIMIT $8`,
		tenantID, projectID, variantKey, statKey, name, from, to, maxGroupRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LabelSum{}
	for rows.Next() {
		var variant, stat string
		var people int64
		if err := rows.Scan(&variant, &stat, &people); err != nil {
			return nil, err
		}
		out = append(out, LabelSum{
			Labels: map[string]string{variantKey: variant, statKey: stat},
			Sum:    float64(people),
		})
	}
	return out, rows.Err()
}
