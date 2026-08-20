-- name: ClaimIngestBatch :one
-- Idempotent ingest dedup. The batch_key is sha256(body); a replay carries the
-- same key. ON CONFLICT DO UPDATE (a no-op on the row) lets us RETURNING the
-- stored accepted count and (xmax = 0), which is true ONLY on a real insert —
-- so a replay returns the first accept's count and never double-writes.
INSERT INTO ingest_batch (batch_key, body_hash, accepted, accepted_at, expires_at)
VALUES (sqlc.arg(batch_key), sqlc.arg(body_hash), sqlc.arg(accepted), now(),
        now() + interval '24 hours')
ON CONFLICT (batch_key) DO UPDATE
        SET batch_key = ingest_batch.batch_key
RETURNING accepted, (xmax = 0) AS inserted;

-- name: PurgeExpiredBatches :exec
-- Background sweep (ucworker) drops expired dedup rows so ingest_batch stays
-- bounded. Idempotent; safe to run from either instance under advisory lock.
DELETE FROM ingest_batch WHERE expires_at < now();
