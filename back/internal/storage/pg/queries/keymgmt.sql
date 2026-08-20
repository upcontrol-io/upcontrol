-- name: CreateAPIKey :one
-- Issue a new API key. The prefix is indexed and visible; the secret_hash is
-- sha256 of the full key (uc_live_<prefix><secret>). The full key is shown to
-- the user exactly once at creation.
INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash, state)
VALUES (sqlc.arg(tenant_id), sqlc.arg(project_id), sqlc.arg(prefix), sqlc.arg(secret_hash), 'active')
RETURNING id, prefix, state, created_at;

-- name: SetKeyRotating :exec
-- Mark the current active key as rotating with a 24h overlap window. The old
-- key continues to authenticate until rotating_until passes (back-impl D4).
UPDATE api_key
   SET state = 'rotating', rotating_until = now() + INTERVAL '24 hours'
 WHERE tenant_id = $1 AND state = 'active';

-- name: GetActiveKeyForRotate :one
-- Get the current active key's details (needed to compute the full key for the
-- rotate response — but we can't show the old key's secret, only its prefix).
SELECT id, tenant_id, project_id, prefix, state, rotating_until, created_at
  FROM api_key
 WHERE tenant_id = $1 AND state IN ('active', 'rotating')
 ORDER BY created_at DESC
 LIMIT 1;

-- name: RotateAPIKey :one
-- Atomic: set old key to rotating, insert new key, return new key.
-- Called after GetActiveKeyForRotate to get the project_id.
WITH old AS (
    UPDATE api_key
       SET state = 'rotating', rotating_until = now() + INTERVAL '24 hours'
     WHERE tenant_id = $1 AND state = 'active'
    RETURNING project_id
)
INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash, state)
SELECT $1, old.project_id, $2, $3, 'active' FROM old
RETURNING id, prefix, created_at;
