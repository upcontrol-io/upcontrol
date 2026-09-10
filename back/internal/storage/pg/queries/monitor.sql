-- name: ListMonitorsByTenant :many
SELECT m.id, m.public_id, m.kind, m.name, m.target, m.keyword,
       m.interval_sec, m.availability_target, m.paused, m.paused_by, m.ping_token, m.created_at,
       tf.status, tf.ssl_expires_at, tf.domain_expires_at, tf.last_check_at
  FROM monitor m
  LEFT JOIN target_facts tf ON tf.target_id = m.target_id
 WHERE m.tenant_id = $1
 ORDER BY m.created_at;

-- name: ListMonitorsByProject :many
SELECT m.id, m.public_id, m.kind, m.name, m.target, m.keyword,
       m.interval_sec, m.availability_target, m.paused, m.paused_by, m.ping_token, m.created_at,
       tf.status, tf.ssl_expires_at, tf.domain_expires_at, tf.last_check_at
  FROM monitor m
  LEFT JOIN target_facts tf ON tf.target_id = m.target_id
 WHERE m.project_id = $1
 ORDER BY m.created_at;

-- name: CreateMonitor :one
INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, keyword, interval_sec, ping_token)
VALUES (sqlc.arg(public_id), sqlc.arg(tenant_id), sqlc.arg(project_id),
        sqlc.arg(kind), sqlc.arg(name), sqlc.arg(target), sqlc.arg(keyword),
        sqlc.arg(interval_sec), sqlc.narg(ping_token))
RETURNING id, public_id, kind, name, target, keyword, interval_sec, ping_token, created_at;

-- name: GetMonitorByPublicID :one
SELECT m.id, m.public_id, m.tenant_id, m.project_id, m.kind, m.name, m.target,
       m.keyword, m.interval_sec, m.paused, m.paused_by, m.ping_token, m.created_at,
       tf.status, tf.ssl_expires_at, tf.domain_expires_at
  FROM monitor m
  LEFT JOIN target_facts tf ON tf.target_id = m.target_id
 WHERE m.public_id = $1 AND m.tenant_id = $2;

-- name: GetMonitorByPingToken :one
-- The token is the credential: a miss is a 404, never a hint. target_id is
-- the heartbeat's private probe target, where its facts and schedule live.
SELECT m.id, m.target_id, m.tenant_id, m.name, m.paused, m.interval_sec,
       COALESCE(m.grace_sec, m.interval_sec)::int AS grace_sec
  FROM monitor m
 WHERE m.ping_token = $1 AND m.kind = 'heartbeat';

-- name: PatchMonitor :one
UPDATE monitor SET
  name        = COALESCE(sqlc.narg(name), name),
  target      = COALESCE(sqlc.narg(target), target),
  keyword     = COALESCE(sqlc.narg(keyword), keyword),
  interval_sec = COALESCE(sqlc.narg(interval_sec), interval_sec),
  paused      = COALESCE(sqlc.narg(paused), paused),
  -- An explicit pause/unpause from the owner clears the sweeper's marker: the
  -- owner overrides the plan, and the next sweep re-marks if the budget says so.
  paused_by   = CASE WHEN sqlc.narg(paused) IS NULL THEN paused_by ELSE NULL END
 WHERE public_id = sqlc.arg(public_id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING id, public_id, kind, name, target, keyword, interval_sec, paused, paused_by, created_at;

-- name: DeleteMonitor :exec
DELETE FROM monitor WHERE public_id = $1 AND tenant_id = $2;

-- name: GetMonitorInterval :one
-- Used by SubmitResults to reschedule at the monitor's real interval (not a
-- hardcoded 5m) AND to label the raw check row written to Postgres. A
-- missing row means the monitor was deleted mid-flight.
SELECT tenant_id, interval_sec, paused FROM monitor WHERE id = $1;

-- name: CountMonitorsByTenant :one
-- HTTP checks are the one counted axis (new-plan.md §5.2). A heartbeat costs us
-- nothing to run — the customer's job calls us — so counting one against the
-- plan fires a paid wall on an axis the product gives away.
SELECT count(*)::int FROM monitor WHERE tenant_id = $1 AND kind <> 'heartbeat';

-- name: GetPlanHTTPChecks :one
SELECT http_checks FROM plan_entitlement WHERE plan = $1;
