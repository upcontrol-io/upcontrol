-- name: CreateAPIKey :one
-- Issue a new API key. The prefix is indexed and visible; the secret_hash is
-- sha256 of the full key (<scheme><prefix><secret>). The full key is shown to
-- the user exactly once at creation. `kind` picks the scheme the caller hashed:
-- a public key is a browser credential and carries the origins it may be sent
-- from, which is the whole of its scope.
INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash, state, name, kind, origins)
VALUES (sqlc.arg(tenant_id), sqlc.arg(project_id), sqlc.arg(prefix), sqlc.arg(secret_hash), 'active', sqlc.arg(name), sqlc.arg(kind), sqlc.arg(origins))
RETURNING id, prefix, name, state, created_at, kind, origins;

-- name: RotateAPIKey :one
-- Atomic: retire the project's active SECRET keys and issue one new one.
-- Scoped to one project: a workspace's other projects keep their keys.
--
-- Rotation is the everything-now lever, so retiring every secret key at once is
-- the intent, not a bug. Two things are not:
--
--   kind = 'secret' — a public key must survive this. It lives in a browser
--   bundle, its replacement would be a uc_live_ key that cannot go there, and
--   the only symptom would be a website that goes quiet 24 hours later.
--   Rotating a public key means redeploying a site: a different act, on a
--   different clock, and it gets its own door rather than a side effect here.
--
--   LIMIT 1 — `old` returns one row per retired key, and INSERT..SELECT would
--   have minted one new key per old one. With the key SET (2026-09-07) that is
--   no longer hypothetical: two keys in, two out, and :one hands back whichever
--   the planner returned first.
WITH old AS (
    UPDATE api_key
       SET state = 'rotating', rotating_until = now() + INTERVAL '24 hours'
     WHERE api_key.tenant_id = sqlc.arg(tenant_id)
       AND api_key.project_id = sqlc.arg(project_id)
       AND api_key.state = 'active' AND api_key.kind = 'secret'
    RETURNING project_id
)
INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash, state, kind)
SELECT sqlc.arg(tenant_id), old.project_id, sqlc.arg(prefix), sqlc.arg(secret_hash), 'active', 'secret'
  FROM old LIMIT 1
RETURNING id, prefix, created_at;

-- name: ListAPIKeysForProject :many
-- Every key of one project, newest first, revoked ones included: the last use of
-- a withdrawn key is the record of what it reached before anyone noticed.
SELECT id, prefix, name, state, created_at, last_used_at, revoked_at, kind, origins
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

-- name: RevokeProjectAPIKeys :execrows
-- Every key of one project that still works, at once: the door a frozen
-- project keeps, since no by-id door reaches a project no session stands in.
-- Marked, never deleted, like RevokeAPIKey.
UPDATE api_key
   SET state = 'revoked', revoked_at = now()
 WHERE project_id = sqlc.arg(project_id) AND state <> 'revoked';
