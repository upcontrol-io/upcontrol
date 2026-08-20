-- name: GetAPIKeyByPrefix :one
-- Look up an API key by its (visible) prefix for authentication. The caller then
-- verifies sha256(full_key) == secret_hash and checks state. The prefix is the
-- first segment of the key after "uc_live_", indexed uniquely so the lookup is
-- O(1) — we never scan keys.
SELECT id, tenant_id, project_id, secret_hash, state, rotating_until
  FROM api_key
 WHERE prefix = $1;

-- name: TouchAPIKeyLastUsed :exec
-- Stamp last_used_at on a successful auth (cheap, best-effort: key_usage_log is
-- the durable record; this just feeds the "last used" display).
UPDATE api_key SET last_used_at = now() WHERE id = $1;

-- name: LogKeyUsage :exec
-- One row per accepted/rejected use, for the Sources #key usage screen.
INSERT INTO key_usage_log (tenant_id, key_id, source, outcome)
VALUES (sqlc.arg(tenant_id), sqlc.arg(key_id), sqlc.arg(source), sqlc.arg(outcome));
