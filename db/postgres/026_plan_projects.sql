-- +goose Up
-- +goose StatementBegin
-- How many projects a tenant may have is now a plan axis (Aug 27, 2026, user
-- decision — docs/plans/projects-axis.md): Free 1, Indie 2, Growth 5, Agency 10.
-- Self-hosted stays NULL: the same "NULL = unlimited" contract as ai_explains.
-- A project is a row in `project` within the person's ONE tenant; billing stays
-- tenant-level, so the ladder gates the count, nothing else.
ALTER TABLE plan_entitlement ADD COLUMN projects int;

UPDATE plan_entitlement SET projects =  1 WHERE plan = 'Free';
UPDATE plan_entitlement SET projects =  2 WHERE plan = 'Indie';
UPDATE plan_entitlement SET projects =  5 WHERE plan = 'Growth';
UPDATE plan_entitlement SET projects = 10 WHERE plan = 'Agency';

-- The session remembers which project the person is working in. NULL (or a
-- deleted project, via ON DELETE SET NULL) falls back to the tenant's lowest
-- project id at read time — no backfill, and single-user mode, which has no
-- session row, is unaffected.
ALTER TABLE session ADD COLUMN project_id bigint REFERENCES project(id) ON DELETE SET NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE session DROP COLUMN project_id;
ALTER TABLE plan_entitlement DROP COLUMN projects;
-- +goose StatementEnd
