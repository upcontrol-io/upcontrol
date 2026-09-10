// The plan-capacity sweepers (docs/plans/trial-and-freeze.md): a plan buys
// LIVE capacity, data belongs to the customer. Both jobs are single statements
// joined through the entitlement table, mirroring TrimHistory so a limit moves
// without a redeploy.

package pgstore

import (
	"context"
)

// FreezeSweep enforces the projects axis on what RUNS, not on what exists:
// every tenant keeps its most recently active `plan_entitlement.projects`
// projects live (all of them when the plan is unlimited) and the rest are
// frozen snapshots. Activity is the newest log or event row, falling back to
// created_at — the honest "which project is in use" signal; a frozen project
// generates neither, so the ranking cannot flap. One statement both ways, so
// the same tick that learns of an upgrade (or of the live project's deletion —
// the next by activity is promoted) restores what fits.
func (s *Store) FreezeSweep(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		WITH ranked AS (
		  SELECT p.id,
		         row_number() OVER (PARTITION BY p.tenant_id ORDER BY
		           GREATEST(
		             COALESCE((SELECT max(l.ts) FROM logs l
		                WHERE l.tenant_id = p.tenant_id AND l.project_id = p.id), p.created_at),
		             COALESCE((SELECT max(e.ts) FROM events e
		                WHERE e.tenant_id = p.tenant_id AND e.project_id = p.id), p.created_at)
		           ) DESC, p.id ASC) AS rn,
		         COALESCE(pe.projects, 2147483647) AS lim
		    FROM project p
		    JOIN tenant t ON t.id = p.tenant_id
		    JOIN plan_entitlement pe ON pe.plan = t.plan
		)
		UPDATE project p SET frozen_at = CASE WHEN r.rn > r.lim THEN now() ELSE NULL END
		  FROM ranked r
		 WHERE p.id = r.id
		   AND ((r.rn > r.lim AND p.frozen_at IS NULL)
		     OR (r.rn <= r.lim AND p.frozen_at IS NOT NULL))`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MonitorBudgetSweep pauses HTTP monitors beyond the plan's http_checks and
// unpauses exactly its own pauses when the budget fits again. Oldest-first
// (created_at, id): deterministic, and the monitors the customer configured
// first are the ones that stay up. paused_by='plan' is the sweeper's marker —
// an owner's own pause (or a released project's) is never touched. Frozen
// projects' monitors hold no live slot; heartbeats never did.
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
		     AND pe.http_checks > 0
		)
		UPDATE monitor SET paused = true, paused_by = 'plan'
		 WHERE id IN (SELECT id FROM ranked WHERE rn > lim) AND NOT paused`)
	if err != nil {
		return 0, err
	}
	paused := tag.RowsAffected()

	tag, err = s.pool.Exec(ctx, `
		WITH ranked AS (
		  SELECT m.id, pe.http_checks AS lim,
		         row_number() OVER (PARTITION BY m.tenant_id ORDER BY m.created_at, m.id) AS rn
		    FROM monitor m
		    JOIN project p ON p.id = m.project_id AND p.frozen_at IS NULL
		    JOIN tenant t ON t.id = m.tenant_id
		    JOIN plan_entitlement pe ON pe.plan = t.plan
		   WHERE m.kind <> 'heartbeat'
		     AND pe.http_checks > 0
		)
		UPDATE monitor SET paused = false, paused_by = NULL
		 WHERE id IN (SELECT id FROM ranked WHERE rn <= lim) AND paused AND paused_by = 'plan'`)
	if err != nil {
		return paused, err
	}
	return paused + tag.RowsAffected(), nil
}
