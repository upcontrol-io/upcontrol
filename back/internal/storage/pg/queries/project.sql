-- Project queries: the installer-collected project spec (ai-provider plan,
-- Decision 15b). Lives beside its table, not in ai.sql — the writer is the
-- install endpoint, which has nothing to do with the AI package.

-- name: SetProjectMeta :exec
-- Tenant-scoped on purpose: this is the one externally-authenticated project
-- write, and the API-key resolver returns tenant+project from the same row,
-- so the guard costs one predicate and nothing else.
UPDATE project SET meta = $3 WHERE id = $1 AND tenant_id = $2;

-- name: GetProjectMeta :one
-- Tenant-scoped like every other project resolver in the app: the explain
-- context reader holds a tenant id, not a project id.
SELECT meta FROM project WHERE tenant_id = $1 ORDER BY id LIMIT 1;

-- name: CountProjectsByTenant :one
-- The projects plan axis (docs/plans/projects-axis.md): this count against
-- plan_entitlement.projects (NULL = unlimited) is the create/claim gate.
SELECT count(*) FROM project WHERE tenant_id = $1;

-- name: ListProjectsByTenant :many
-- The Projects page: every project in the tenant, oldest first.
SELECT id, public_id, domain, created_at FROM project WHERE tenant_id = $1 ORDER BY id;
