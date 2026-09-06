-- name: CreateSession :exec
-- A session is keyed by sha256(cookie_value); the cookie value itself is never
-- stored. expires_at is set at creation; the middleware checks it on every /v1/*.
-- project_id is the scope the session opens on: NULL until one is resolved.
INSERT INTO session (token_hash, person_id, tenant_id, project_id, expires_at)
VALUES (sqlc.arg(token_hash), sqlc.arg(person_id), sqlc.arg(tenant_id), sqlc.narg(project_id),
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

-- name: SetSessionScope :exec
-- The same switch across workspaces: a project reached by invite lives in
-- somebody else's workspace, so the session's tenant follows the project.
UPDATE session SET tenant_id = sqlc.arg(tenant_id), project_id = sqlc.narg(project_id)
 WHERE id = sqlc.arg(id);

-- name: DeleteSession :exec
DELETE FROM session WHERE token_hash = $1;

-- name: GetPersonByEmail :one
SELECT id, public_id, email, name FROM person WHERE email = $1;

-- name: CreatePerson :one
-- public_id is a UUID; the caller generates it (v7 when available, v4 for now).
INSERT INTO person (public_id, email, name)
VALUES (sqlc.arg(public_id), sqlc.arg(email), sqlc.arg(name))
RETURNING id, public_id, email, name;

-- name: GetMe :one
-- The /v1/me aggregate: person + workspace + current project, joined off the
-- session. `owned` says whether the reader owns the workspace; member_role is
-- their role in the CURRENT project, and the owner always answers 'login'.
-- The project is the session's own while it still belongs to the workspace and
-- the reader can reach it, else the lowest project there they can reach.
SELECT
  p.id       AS person_id,
  p.public_id AS person_public_id,
  p.email,
  p.name     AS person_name,
  (CASE WHEN t.owner_person_id = p.id THEN 'login' ELSE COALESCE(pm.role, 'notify') END)::text AS member_role,
  t.id       AS tenant_id,
  t.plan,
  COALESCE(t.owner_person_id = p.id, false)::bool AS owned,
  o.email    AS owner_email,
  s.project_id AS session_project_id,
  pr.id       AS project_id,
  pr.public_id AS project_public_id,
  pr.domain   AS project_domain,
  pr.created_at AS project_created_at
FROM session s
JOIN person p ON p.id = s.person_id
JOIN tenant t ON t.id = s.tenant_id
LEFT JOIN person o ON o.id = t.owner_person_id
LEFT JOIN project pr ON pr.id = COALESCE(
  (SELECT x.id FROM project x
    WHERE x.id = s.project_id AND x.tenant_id = t.id
      AND (t.owner_person_id = p.id
           OR EXISTS (SELECT 1 FROM project_member m
                       WHERE m.project_id = x.id AND m.person_id = p.id AND m.status = 'active'))),
  (SELECT min(x.id) FROM project x
    WHERE x.tenant_id = t.id
      AND (t.owner_person_id = p.id
           OR EXISTS (SELECT 1 FROM project_member m
                       WHERE m.project_id = x.id AND m.person_id = p.id AND m.status = 'active'))))
LEFT JOIN project_member pm ON pm.project_id = pr.id AND pm.person_id = p.id AND pm.status = 'active'
WHERE s.token_hash = $1 AND s.expires_at > now()
LIMIT 1;

-- name: GetMeByIdentity :one
-- The same /v1/me aggregate keyed by the identity itself: single-user mode
-- (UC_AUTH=none) has no session row, so the token-hash join above can never
-- answer for it. Columns mirror GetMe exactly — the handler converts between
-- the two generated row types. session_project_id is the NULL literal (no
-- session row exists here) and the project is the lowest reachable one.
SELECT
  p.id       AS person_id,
  p.public_id AS person_public_id,
  p.email,
  p.name     AS person_name,
  (CASE WHEN t.owner_person_id = p.id THEN 'login' ELSE COALESCE(pm.role, 'notify') END)::text AS member_role,
  t.id       AS tenant_id,
  t.plan,
  COALESCE(t.owner_person_id = p.id, false)::bool AS owned,
  o.email    AS owner_email,
  NULL::bigint AS session_project_id,
  pr.id       AS project_id,
  pr.public_id AS project_public_id,
  pr.domain   AS project_domain,
  pr.created_at AS project_created_at
FROM person p
JOIN tenant t ON t.id = sqlc.arg(tenant_id)
LEFT JOIN person o ON o.id = t.owner_person_id
LEFT JOIN project pr ON pr.id = (
  SELECT min(x.id) FROM project x
   WHERE x.tenant_id = t.id
     AND (t.owner_person_id = p.id
          OR EXISTS (SELECT 1 FROM project_member m
                      WHERE m.project_id = x.id AND m.person_id = p.id AND m.status = 'active')))
LEFT JOIN project_member pm ON pm.project_id = pr.id AND pm.person_id = p.id AND pm.status = 'active'
WHERE p.id = sqlc.arg(person_id)
LIMIT 1;

-- name: LastSessionScope :one
-- Where this person was last working: a new session reopens on it instead of
-- on whichever project happens to sort lowest.
SELECT tenant_id, project_id FROM session
 WHERE person_id = $1 AND project_id IS NOT NULL
 ORDER BY last_seen_at DESC LIMIT 1;

-- name: OwnTenant :one
-- The workspace this person owns; no row means they own none yet.
SELECT id FROM tenant WHERE owner_person_id = $1 ORDER BY id LIMIT 1;

-- name: ProjectScope :one
-- Can this person reach this project, and as what: no row means not reachable,
-- which is the same answer whether the project is a stranger's or unknown.
SELECT p.id, p.tenant_id,
       COALESCE(t.owner_person_id = sqlc.arg(person_id), false)::bool AS owned,
       (CASE WHEN t.owner_person_id = sqlc.arg(person_id) THEN 'login' ELSE COALESCE(m.role, '') END)::text AS role
  FROM project p
  JOIN tenant t ON t.id = p.tenant_id
  LEFT JOIN project_member m ON m.project_id = p.id AND m.person_id = sqlc.arg(person_id) AND m.status = 'active'
 WHERE p.id = sqlc.arg(project_id)
   AND (t.owner_person_id = sqlc.arg(person_id) OR m.person_id IS NOT NULL);

-- name: FirstReachableProject :one
-- Their own workspace's lowest project, else the lowest one they were invited
-- to: where a session lands when it has no remembered scope.
SELECT p.id, p.tenant_id
  FROM project p
  JOIN tenant t ON t.id = p.tenant_id
  LEFT JOIN project_member m ON m.project_id = p.id AND m.person_id = sqlc.arg(person_id) AND m.status = 'active'
 WHERE t.owner_person_id = sqlc.arg(person_id) OR m.person_id IS NOT NULL
 ORDER BY (t.owner_person_id = sqlc.arg(person_id)) DESC, p.id
 LIMIT 1;

-- name: ReachableProjectInTenant :one
-- The session's current-project resolver: the session's own pick while it is
-- still reachable, else the lowest reachable project in that workspace, else 0.
SELECT COALESCE(
  (SELECT x.id FROM project x
    WHERE x.id = sqlc.arg(pick) AND x.tenant_id = sqlc.arg(tenant_id)
      AND (EXISTS (SELECT 1 FROM tenant t WHERE t.id = x.tenant_id AND t.owner_person_id = sqlc.arg(person_id))
           OR EXISTS (SELECT 1 FROM project_member m
                       WHERE m.project_id = x.id AND m.person_id = sqlc.arg(person_id) AND m.status = 'active'))),
  (SELECT min(x.id) FROM project x
    WHERE x.tenant_id = sqlc.arg(tenant_id)
      AND (EXISTS (SELECT 1 FROM tenant t WHERE t.id = x.tenant_id AND t.owner_person_id = sqlc.arg(person_id))
           OR EXISTS (SELECT 1 FROM project_member m
                       WHERE m.project_id = x.id AND m.person_id = sqlc.arg(person_id) AND m.status = 'active'))),
  0)::bigint;

-- name: ActivateInvites :many
-- Redeeming an identity accepts every project invite waiting on it, and the
-- rows come back so the caller knows where the session may now land.
UPDATE project_member SET status = 'active'
 WHERE person_id = $1 AND status = 'pending'
RETURNING project_id, tenant_id;
