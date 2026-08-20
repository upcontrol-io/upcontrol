-- name: LeaseSeqBlock :one
-- Atomically lease a block of sequence values for a project. The caller owns
-- [start_seq, start_seq + block_size); holes from unused tails on restart are
-- acceptable — seq is an order marker, not a row counter (plan ring/seq §2.3.1).
UPDATE project_seq
   SET next = next + sqlc.arg(block_size)
 WHERE project_id = sqlc.arg(project_id)
 RETURNING (next - sqlc.arg(block_size))::bigint AS start_seq;

-- name: EnsureProjectSeq :exec
-- Create the seq row for a new project if absent (idempotent). Called at
-- project creation so LeaseSeqBlock never no-ops on a missing row.
INSERT INTO project_seq (project_id, next)
VALUES (sqlc.arg(project_id), 1)
ON CONFLICT (project_id) DO NOTHING;

-- name: UpsertLedgerBucket :exec
-- Add a 5-minute bucket's row count and seq extent to the per-project ledger.
-- The ring walks this backward to derive cutoff_seq and retain_seq.
INSERT INTO tenant_line_ledger (project_id, bucket_5m, rows, min_seq, max_seq)
VALUES (sqlc.arg(project_id), sqlc.arg(bucket_5m), sqlc.arg(rows),
        sqlc.arg(min_seq), sqlc.arg(max_seq))
ON CONFLICT (project_id, bucket_5m) DO UPDATE
   SET rows    = tenant_line_ledger.rows    + EXCLUDED.rows,
       min_seq = LEAST(tenant_line_ledger.min_seq, EXCLUDED.min_seq),
       max_seq = GREATEST(tenant_line_ledger.max_seq, EXCLUDED.max_seq);

-- name: LedgerBucketsBackward :many
-- Read ledger buckets from `until` backward, oldest-needed first, so the cutoff
-- walker sums rows until it reaches the window line count. Ordered by bucket
-- descending so the walker consumes newest-first.
SELECT project_id, bucket_5m, rows, min_seq, max_seq
  FROM tenant_line_ledger
 WHERE project_id = sqlc.arg(project_id)
   AND bucket_5m <= sqlc.arg(until)
 ORDER BY bucket_5m DESC;

-- name: GetProjectWindow :one
SELECT * FROM project_window WHERE project_id = $1;

-- name: UpsertProjectWindow :exec
-- Recompute the window each minute: cutoff_seq (visibility), retain_seq
-- (deletion), the actual depth for the screen, and beyond_errors (NULL = the
-- "zero is silence" rule: a field absent from the response).
INSERT INTO project_window (project_id, cutoff_seq, retain_seq, window_hours,
                            beyond_errors, computed_at)
VALUES (sqlc.arg(project_id), sqlc.arg(cutoff_seq), sqlc.arg(retain_seq),
        sqlc.arg(window_hours), sqlc.arg(beyond_errors), now())
ON CONFLICT (project_id) DO UPDATE
   SET cutoff_seq    = EXCLUDED.cutoff_seq,
       retain_seq    = EXCLUDED.retain_seq,
       window_hours  = EXCLUDED.window_hours,
       beyond_errors = EXCLUDED.beyond_errors,
       computed_at   = now();
