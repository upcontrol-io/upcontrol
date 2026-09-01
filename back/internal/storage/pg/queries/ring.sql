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

