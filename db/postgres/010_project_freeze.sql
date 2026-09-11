-- +goose Up
-- +goose StatementBegin
-- Project freeze: a plan buys LIVE projects. A tenant over its projects limit
-- keeps its live ones running and freezes the rest as snapshots - no checks
-- and no delivery. Configuration, incidents and the rollup up to the freeze
-- are kept, so an upgrade restores them as they stopped; raw logs and check
-- results age out with the daily partitions like any project's.
ALTER TABLE project ADD COLUMN frozen_at timestamptz;

-- The budget sweeper pauses over-limit monitors and must later unpause exactly
-- its own pauses, never an owner's: paused_by separates the two writers. NULL
-- is the owner's own pause, 'plan' is the sweeper's.
ALTER TABLE monitor ADD COLUMN paused_by text;

-- A custom status-page domain outlives the plan that carried it by a grace
-- window: the sweeper stamps when the plan stops carrying custom domains,
-- clears when it carries them again, and unbinds the domain once the stamp is
-- three days old.
ALTER TABLE status_page ADD COLUMN domain_lapsed_at timestamptz;
-- +goose StatementEnd

-- +goose StatementBegin
-- The effective-cadence ladder, now blind to frozen subscribers: a snapshot's
-- subscription sets no target's pace, exactly as it keeps no target due in
-- LeaseDueTargets. Otherwise the 009 body, unchanged.
CREATE OR REPLACE FUNCTION _uc_effective_interval(t probe_target) RETURNS int
LANGUAGE sql STABLE AS $$
SELECT CASE
         WHEN f.eff IS NOT NULL THEN
           CASE WHEN sp.n > 0 THEN LEAST(f.eff, 900) ELSE f.eff END
         WHEN t.created_at > now() - interval '24 hours' THEN 300
         WHEN sp.any_claimed = false
          AND sp.indexed_at IS NULL
          AND COALESCE(sp.last_seen_at, t.created_at) < now() - interval '30 days' THEN 3600
         ELSE 900
       END::int
  FROM (SELECT min(m.interval_sec)::int AS eff
          FROM monitor m
          JOIN project pr ON pr.id = m.project_id AND pr.frozen_at IS NULL
         WHERE m.target_id = t.id AND NOT m.paused) f,
       (SELECT count(*)::int AS n,
               max(pgp.indexed_at) AS indexed_at,
               max(pgp.last_seen_at) AS last_seen_at,
               bool_or(tn.claim_token_hash IS NULL) AS any_claimed
          FROM status_page pgp
          JOIN tenant tn ON tn.id = pgp.tenant_id
         WHERE pgp.root_target_id = t.id AND pgp.removed_at IS NULL) sp
$$;
-- +goose StatementEnd

-- +goose Down
-- The sweeper's pauses go first: once paused_by is dropped nothing can tell a
-- plan pause from an owner's, and older code never resumes one.
UPDATE monitor SET paused = false WHERE paused_by = 'plan';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION _uc_effective_interval(t probe_target) RETURNS int
LANGUAGE sql STABLE AS $$
SELECT CASE
         WHEN f.eff IS NOT NULL THEN
           CASE WHEN sp.n > 0 THEN LEAST(f.eff, 900) ELSE f.eff END
         WHEN t.created_at > now() - interval '24 hours' THEN 300
         WHEN sp.any_claimed = false
          AND sp.indexed_at IS NULL
          AND COALESCE(sp.last_seen_at, t.created_at) < now() - interval '30 days' THEN 3600
         ELSE 900
       END::int
  FROM (SELECT min(m.interval_sec)::int AS eff
          FROM monitor m
         WHERE m.target_id = t.id AND NOT m.paused) f,
       (SELECT count(*)::int AS n,
               max(pgp.indexed_at) AS indexed_at,
               max(pgp.last_seen_at) AS last_seen_at,
               bool_or(tn.claim_token_hash IS NULL) AS any_claimed
          FROM status_page pgp
          JOIN tenant tn ON tn.id = pgp.tenant_id
         WHERE pgp.root_target_id = t.id AND pgp.removed_at IS NULL) sp
$$;
-- +goose StatementEnd
ALTER TABLE status_page DROP COLUMN domain_lapsed_at;
ALTER TABLE monitor DROP COLUMN paused_by;
ALTER TABLE project DROP COLUMN frozen_at;
