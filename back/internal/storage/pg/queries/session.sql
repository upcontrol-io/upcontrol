-- name: CreateSession :exec
-- A session is keyed by sha256(cookie_value); the cookie value itself is never
-- stored. expires_at is set at creation; the middleware checks it on every /v1/*.
INSERT INTO session (token_hash, person_id, tenant_id, expires_at)
VALUES (sqlc.arg(token_hash), sqlc.arg(person_id), sqlc.arg(tenant_id),
        now() + make_interval(secs => sqlc.arg(ttl_secs)::double precision));

-- name: GetSessionByToken :one
-- Only non-expired sessions match. last_seen_at is touched separately.
-- project_id rides along: every /v1/* request resolves the current project
-- off the session (docs/plans/projects-axis.md), so it must survive this read.
-- Column order follows the table, so sqlc returns the Session model itself
-- rather than a one-off row struct.
SELECT id, token_hash, person_id, tenant_id, project_id, created_at, last_seen_at, expires_at
  FROM session
 WHERE token_hash = $1
   AND expires_at > now();

-- name: TouchSession :exec
UPDATE session SET last_seen_at = now() WHERE id = $1;

-- name: SetSessionProject :exec
-- The project switcher: points the session at a project the handler has
-- already verified belongs to the session's tenant.
UPDATE session SET project_id = $2 WHERE id = $1;

-- name: DeleteSession :exec
DELETE FROM session WHERE token_hash = $1;

-- name: GetPersonByEmail :one
SELECT id, public_id, email, name FROM person WHERE email = $1;

-- name: CreatePerson :one
-- public_id is a UUID; the caller generates it (v7 when available, v4 for now).
INSERT INTO person (public_id, email, name)
VALUES (sqlc.arg(public_id), sqlc.arg(email), sqlc.arg(name))
RETURNING id, public_id, email, name;

-- name: EnsureTenantMember :exec
-- First member of a tenant becomes the owner (role=login). Idempotent.
INSERT INTO tenant_member (tenant_id, person_id, role, status)
VALUES (sqlc.arg(tenant_id), sqlc.arg(person_id), 'login', 'active')
ON CONFLICT (tenant_id, person_id) DO NOTHING;

-- name: GetMe :one
-- The /v1/me aggregate: person + tenant + current project, joined off the
-- session. The project is the session's own when it still belongs to the
-- tenant, else the tenant's lowest id — a session whose project was deleted
-- (or never set) still answers instead of going NULL while projects exist.
SELECT
  p.id       AS person_id,
  p.public_id AS person_public_id,
  p.email,
  p.name     AS person_name,
  tm.role    AS member_role,
  t.id       AS tenant_id,
  t.plan,
  t.billing,
  s.project_id AS session_project_id,
  pr.id       AS project_id,
  pr.public_id AS project_public_id,
  pr.domain   AS project_domain,
  pr.created_at AS project_created_at
FROM session s
JOIN person p       ON p.id = s.person_id
JOIN tenant_member tm ON tm.person_id = p.id AND tm.tenant_id = s.tenant_id
JOIN tenant t       ON t.id = tm.tenant_id
LEFT JOIN project pr ON pr.id = COALESCE((SELECT id FROM project WHERE id = s.project_id AND tenant_id = t.id), (SELECT min(id) FROM project WHERE tenant_id = t.id))
WHERE s.token_hash = $1 AND s.expires_at > now()
ORDER BY pr.created_at
LIMIT 1;

-- name: GetMeByIdentity :one
-- The same /v1/me aggregate keyed by the identity itself: single-user mode
-- (UC_AUTH=none) has no session row, so the token-hash join above can never
-- answer for it. Columns mirror GetMe exactly — the handler converts between
-- the two generated row types. session_project_id is the NULL literal (no
-- session row exists here) and the project is the tenant's lowest id.
SELECT
  p.id       AS person_id,
  p.public_id AS person_public_id,
  p.email,
  p.name     AS person_name,
  tm.role    AS member_role,
  t.id       AS tenant_id,
  t.plan,
  t.billing,
  NULL::bigint AS session_project_id,
  pr.id       AS project_id,
  pr.public_id AS project_public_id,
  pr.domain   AS project_domain,
  pr.created_at AS project_created_at
FROM person p
JOIN tenant_member tm ON tm.person_id = p.id AND tm.tenant_id = sqlc.arg(tenant_id)
JOIN tenant t       ON t.id = tm.tenant_id
LEFT JOIN project pr ON pr.id = (SELECT min(id) FROM project WHERE tenant_id = t.id)
WHERE p.id = sqlc.arg(person_id)
ORDER BY pr.created_at
LIMIT 1;
