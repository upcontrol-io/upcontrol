// The plan-capacity sweepers: a plan buys LIVE capacity, the data belongs to
// the customer. Each is a statement joined through the entitlement table,
// mirroring TrimHistory, so a limit moves without a redeploy.

package pgstore

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// FreezeSweep enforces the projects axis on what RUNS, not on what exists:
// a tenant keeps `plan_entitlement.projects` projects live (all of them when
// the plan is unlimited) and the rest are frozen snapshots.
//
// Incumbency leads the ranking: a live project stays live and a frozen one
// stays frozen, so the pick is made once. That is what keeps it stable: a
// frozen project keeps ingesting, and activity alone would hand it the slot
// back on any tick. The next keys decide only between equals - every
// project on the tick the limit first bites, the snapshots when a slot opens
// (an upgrade, or the live project's deletion): the project with more
// unpaused monitors, then the newest log or event (a future-dated row counts
// as now; created_at when there is none), then the oldest id. A check's last
// probe is no signal: a shared target is probed for every subscriber, so it
// says nothing about this project and would make the pick between two
// monitored projects random.
//
// Only tenants over their limit, or holding a frozen project, are ranked. It
// answers the projects it just thawed: what they recorded while frozen was
// never delivered.
func (s *Store) FreezeSweep(ctx context.Context) ([]int64, error) {
	rows, _ := s.pool.Query(ctx, `
		WITH tenants AS (
		  SELECT p.tenant_id AS id, pe.projects AS lim
		    FROM project p
		    JOIN tenant t ON t.id = p.tenant_id
		    JOIN plan_entitlement pe ON pe.plan = t.plan
		   GROUP BY p.tenant_id, pe.projects
		  HAVING count(*) > pe.projects OR bool_or(p.frozen_at IS NOT NULL)
		), ranked AS (
		  SELECT p.id,
		         row_number() OVER (PARTITION BY p.tenant_id ORDER BY
		           (p.frozen_at IS NULL) DESC,
		           (SELECT count(*) FROM monitor m WHERE m.project_id = p.id AND NOT m.paused) DESC,
		           GREATEST(
		             LEAST(COALESCE((SELECT max(l.ts) FROM logs l
		               WHERE l.tenant_id = p.tenant_id AND l.project_id = p.id), p.created_at), now()),
		             LEAST(COALESCE((SELECT max(e.ts) FROM events e
		               WHERE e.tenant_id = p.tenant_id AND e.project_id = p.id), p.created_at), now()),
		             p.created_at) DESC,
		           p.id) AS rn,
		         COALESCE(tn.lim, 2147483647) AS lim
		    FROM project p
		    JOIN tenants tn ON tn.id = p.tenant_id
		), upd AS (
		  UPDATE project p SET frozen_at = CASE WHEN r.rn > r.lim THEN now() END
		    FROM ranked r
		   WHERE p.id = r.id
		     AND ((r.rn > r.lim AND p.frozen_at IS NULL)
		       OR (r.rn <= r.lim AND p.frozen_at IS NOT NULL))
		  RETURNING p.id, p.frozen_at
		)
		SELECT id FROM upd WHERE frozen_at IS NULL`)
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// MonitorBudgetSweep pauses HTTP monitors beyond the plan's http_checks and
// unpauses exactly its own pauses when the budget fits again, in one
// statement. Oldest-first (created_at, id): deterministic, and the monitors
// the customer configured first are the ones that stay up. paused_by='plan'
// is the sweeper's marker; an owner's own pause (NULL) is never touched and
// still holds its slot. Frozen projects' monitors hold no live slot;
// heartbeats never did.
func (s *Store) MonitorBudgetSweep(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		WITH ranked AS (
		  SELECT m.id, pe.http_checks AS lim,
		         row_number() OVER (PARTITION BY m.tenant_id ORDER BY m.created_at, m.id) AS rn
		    FROM monitor m
		    JOIN project p ON p.id = m.project_id AND p.frozen_at IS NULL
		    JOIN tenant t ON t.id = m.tenant_id
		    JOIN plan_entitlement pe ON pe.plan = t.plan
		   WHERE m.kind <> 'heartbeat'
		)
		UPDATE monitor m SET paused = r.rn > r.lim,
		       paused_by = CASE WHEN r.rn > r.lim THEN 'plan' END
		  FROM ranked r
		 WHERE m.id = r.id
		   AND ((r.rn > r.lim AND NOT m.paused)
		     OR (r.rn <= r.lim AND m.paused AND m.paused_by = 'plan'))`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DomainGraceDays is how long a custom status-page domain keeps answering
// after the plan stops carrying custom domains; domainAllowed reads the same
// window.
const DomainGraceDays = 3

// DomainGraceSweep runs the custom-domain grace clock: it stamps
// domain_lapsed_at on a page whose domain the tenant's plan no longer
// carries, clears the stamp when the plan carries it again (or the domain is
// gone), and unbinds the domain once the stamp is DomainGraceDays old. The
// page itself stays on its own address.
func (s *Store) DomainGraceSweep(ctx context.Context) error {
	for _, stmt := range []string{
		`UPDATE status_page sp SET domain_lapsed_at = NULL
		   FROM tenant t JOIN plan_entitlement pe ON pe.plan = t.plan
		  WHERE t.id = sp.tenant_id AND sp.domain_lapsed_at IS NOT NULL
		    AND (pe.custom_domain OR sp.domain IS NULL)`,
		`UPDATE status_page sp SET domain_lapsed_at = now()
		   FROM tenant t JOIN plan_entitlement pe ON pe.plan = t.plan
		  WHERE t.id = sp.tenant_id AND sp.domain_lapsed_at IS NULL
		    AND sp.domain IS NOT NULL AND NOT pe.custom_domain`,
		`UPDATE status_page sp SET domain = NULL, domain_verified_at = NULL, domain_lapsed_at = NULL
		   FROM tenant t JOIN plan_entitlement pe ON pe.plan = t.plan
		  WHERE t.id = sp.tenant_id AND NOT pe.custom_domain
		    AND sp.domain_lapsed_at <= now() - make_interval(days => ` + strconv.Itoa(DomainGraceDays) + `)`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}
