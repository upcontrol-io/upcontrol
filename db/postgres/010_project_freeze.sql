-- +goose Up
-- Project freeze (docs/plans/trial-and-freeze.md): a plan buys LIVE projects.
-- Downgrade to Free keeps the tenant's most recently active project running and
-- freezes the rest as snapshots — no reads, no checks, no delivery, but nothing
-- deleted and nothing trimmed, so an upgrade restores them as they stopped.
ALTER TABLE project ADD COLUMN frozen_at timestamptz;
CREATE INDEX project_frozen_idx ON project (tenant_id) WHERE frozen_at IS NOT NULL;

-- The budget sweeper pauses over-limit monitors and must later unpause exactly
-- its own pauses, never an owner's: paused_by separates the two writers. NULL
-- (or 'owner') is a human choice, 'plan' is the sweeper's.
ALTER TABLE monitor ADD COLUMN paused_by text;

-- +goose Down
ALTER TABLE project DROP COLUMN frozen_at;
DROP INDEX IF EXISTS project_frozen_idx;
ALTER TABLE monitor DROP COLUMN paused_by;
