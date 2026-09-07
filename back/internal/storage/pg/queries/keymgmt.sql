-- name: CreateAPIKey :one
-- Issue a new API key. The prefix is indexed and visible; the secret_hash is
-- sha256 of the full key (uc_live_<prefix><secret>). The full key is shown to
-- the user exactly once at creation.
INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash, state, name)
VALUES (sqlc.arg(tenant_id), sqlc.arg(project_id), sqlc.arg(prefix), sqlc.arg(secret_hash), 'active', sqlc.arg(name))
RETURNING id, prefix, name, state, created_at;

-- name: RotateAPIKey :one
-- Atomic: set the project's old key to rotating, insert the new one, return it.
-- Scoped to one project: a workspace's other projects keep their keys.
WITH old AS (
    UPDATE api_key
       SET state = 'rotating', rotating_until = now() + INTERVAL '24 hours'
     WHERE api_key.tenant_id = sqlc.arg(tenant_id)
       AND api_key.project_id = sqlc.arg(project_id) AND api_key.state = 'active'
    RETURNING project_id
)
INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash, state)
SELECT sqlc.arg(tenant_id), old.project_id, sqlc.arg(prefix), sqlc.arg(secret_hash), 'active' FROM old
RETURNING id, prefix, created_at;

-- name: ListAPIKeysForProject :many
-- Every key of one project, newest first, revoked ones included: the last use of
-- a withdrawn key is the record of what it reached before anyone noticed.
SELECT id, prefix, name, state, created_at, last_used_at, revoked_at
  FROM api_key WHERE project_id = sqlc.arg(project_id)
 ORDER BY created_at DESC;

-- name: CountLiveAPIKeys :one
-- The cap counts credentials that still work, not history: a revoked key is a
-- record, and keeping it should never stop anyone issuing a replacement.
SELECT count(*) FROM api_key
 WHERE project_id = sqlc.arg(project_id) AND state <> 'revoked';

-- name: RevokeAPIKey :execrows
-- Scoped to the project so an id from another workspace matches nothing. No
-- overlap window: withdrawing a key is what you do when it leaked. The row is
-- marked, never deleted.
UPDATE api_key
   SET state = 'revoked', revoked_at = now()
 WHERE id = sqlc.arg(id) AND project_id = sqlc.arg(project_id) AND state <> 'revoked';
