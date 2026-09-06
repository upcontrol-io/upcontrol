-- Dashboard queries: the one stored board per project, read and replaced whole.

-- name: GetDashboard :one
-- The tenant rides in the predicate even though project_id is the key: a
-- project id from another tenant must read as "no board", never as theirs.
SELECT layout FROM dashboard WHERE tenant_id = $1 AND project_id = $2;

-- name: PutDashboard :exec
-- Replace the board wholesale. The project id is already the caller's own
-- (currentProject resolves it inside the tenant), so the row simply follows
-- the project: a board left behind by an earlier tenant of a released and
-- re-claimed project is overwritten, tenant and all, rather than silently
-- kept while the save answers 200.
INSERT INTO dashboard (tenant_id, project_id, layout)
VALUES ($1, $2, $3)
ON CONFLICT (project_id) DO UPDATE
   SET tenant_id = EXCLUDED.tenant_id, layout = EXCLUDED.layout, updated_at = now();
