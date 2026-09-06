-- name: CreateAPIKey :one
-- Issue a new API key. The prefix is indexed and visible; the secret_hash is
-- sha256 of the full key (uc_live_<prefix><secret>). The full key is shown to
-- the user exactly once at creation.
INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash, state)
VALUES (sqlc.arg(tenant_id), sqlc.arg(project_id), sqlc.arg(prefix), sqlc.arg(secret_hash), 'active')
RETURNING id, prefix, state, created_at;

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
