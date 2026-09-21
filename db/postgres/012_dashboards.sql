-- +goose Up
-- +goose StatementBegin
-- Several named boards per project. The table said "one per project" in its
-- primary key; a board now has an identity of its own and the project becomes
-- an ordinary column beside it. Nothing about an existing row changes except
-- that it gains a name: it is still the project's oldest board, which is the
-- one /v1/dashboard has always answered and the alias `main` still names.
ALTER TABLE dashboard DROP CONSTRAINT dashboard_pkey;
-- Dropping the key leaves the column's NOT NULL in place; it is spelled out
-- so the generated schema says so too, and a board without a project is not a
-- row this table can hold.
ALTER TABLE dashboard ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE dashboard ADD COLUMN id bigserial;
-- Spelled out for the generated schema's sake: a bigserial is NOT NULL and so
-- is a primary key, but sqlc reads the DDL rather than postgres and would
-- hand every by-id query a nullable id.
ALTER TABLE dashboard ALTER COLUMN id SET NOT NULL;
ALTER TABLE dashboard ADD PRIMARY KEY (id);
-- The public id every other entity here carries: the internal key never
-- leaves the process.
ALTER TABLE dashboard ADD COLUMN public_id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid();
ALTER TABLE dashboard ADD COLUMN name text NOT NULL DEFAULT 'Main'
  CHECK (char_length(name) BETWEEN 1 AND 40);
-- created_at is the order the list, the alias and the freeze all read in, so
-- a board that existed before this migration needs one that predates whatever
-- is created next: the day it was last saved is the closest fact stored.
ALTER TABLE dashboard ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
UPDATE dashboard SET created_at = updated_at;
-- Names are what a person and the CLI address a board by, matched without
-- case: two boards differing only in case would make `--board sales` a coin
-- toss.
CREATE UNIQUE INDEX dashboard_project_name_idx ON dashboard (project_id, lower(name));
CREATE INDEX dashboard_project_order_idx ON dashboard (project_id, created_at, id);

-- Boards per PROJECT, unlike checks, which are the workspace's. NULL =
-- unlimited (Self-hosted), the contract every axis in this table carries.
-- Past it a board is frozen rather than deleted, counted in creation order on
-- every read, so a downgrade holds boards back and an upgrade returns them
-- without a job ever touching a row.
ALTER TABLE plan_entitlement ADD COLUMN dashboards int;
UPDATE plan_entitlement SET dashboards = CASE plan
  WHEN 'Free' THEN 1 WHEN 'Indie' THEN 1 WHEN 'Growth' THEN 3 WHEN 'Agency' THEN 5 END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE plan_entitlement DROP COLUMN IF EXISTS dashboards;
-- The old key is one row per project, so everything a project holds past its
-- oldest board has to go before that key can come back. The oldest is the one
-- the alias named, which is the board every caller of the old endpoint saw.
DELETE FROM dashboard d
 WHERE d.id > (SELECT min(x.id) FROM dashboard x WHERE x.project_id = d.project_id);
DROP INDEX IF EXISTS dashboard_project_order_idx;
DROP INDEX IF EXISTS dashboard_project_name_idx;
ALTER TABLE dashboard DROP CONSTRAINT dashboard_pkey;
ALTER TABLE dashboard DROP COLUMN IF EXISTS created_at;
ALTER TABLE dashboard DROP COLUMN IF EXISTS name;
ALTER TABLE dashboard DROP COLUMN IF EXISTS public_id;
ALTER TABLE dashboard DROP COLUMN IF EXISTS id;
ALTER TABLE dashboard ADD PRIMARY KEY (project_id);
-- +goose StatementEnd
