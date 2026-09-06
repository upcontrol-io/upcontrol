-- Project queries: the tenant's projects — the plan-axis count and the list
-- the Projects page renders.

-- name: CountProjectsByTenant :one
-- The projects plan axis (docs/plans/projects-axis.md): this count against
-- plan_entitlement.projects (NULL = unlimited) is the create/claim gate.
SELECT count(*) FROM project WHERE tenant_id = $1;

-- name: ListProjectsByTenant :many
-- The Projects page: every project in the tenant, oldest first.
SELECT id, public_id, domain, created_at FROM project WHERE tenant_id = $1 ORDER BY id;

-- name: ListProjectsForPerson :many
-- Every project this person can reach: their own workspace's first, then the
-- ones they were invited to, oldest first within each. owner_email names whose
-- workspace a guest row lives in.
SELECT p.id, p.public_id, p.domain, p.created_at, p.tenant_id,
       COALESCE(t.owner_person_id = sqlc.arg(person_id), false)::bool AS owned,
       (CASE WHEN t.owner_person_id = sqlc.arg(person_id) THEN 'login' ELSE m.role END)::text AS role,
       o.email AS owner_email
  FROM project p
  JOIN tenant t ON t.id = p.tenant_id
  LEFT JOIN person o ON o.id = t.owner_person_id
  LEFT JOIN project_member m ON m.project_id = p.id AND m.person_id = sqlc.arg(person_id) AND m.status = 'active'
 WHERE t.owner_person_id = sqlc.arg(person_id) OR m.person_id IS NOT NULL
 ORDER BY owned DESC, p.id;

-- name: TenantOwner :one
-- The workspace's owner as a person row: the Team list prepends it, since
-- ownership is a column on the workspace and never a membership row.
SELECT p.id, p.public_id, p.email, p.name, p.telegram_id, p.telegram_username
  FROM tenant t JOIN person p ON p.id = t.owner_person_id
 WHERE t.id = $1;

-- name: IsTenantOwner :one
SELECT EXISTS (SELECT 1 FROM tenant WHERE id = sqlc.arg(tenant_id) AND owner_person_id = sqlc.arg(person_id));
