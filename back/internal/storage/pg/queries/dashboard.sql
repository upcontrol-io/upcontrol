-- Dashboard queries: the one stored board per project, read and replaced whole.

-- name: GetDashboard :one
-- The tenant rides in the predicate even though project_id is the key: a
-- project id from another tenant must read as "no board", never as theirs.
SELECT layout, written_by FROM dashboard WHERE tenant_id = $1 AND project_id = $2;

-- name: PutDashboard :exec
-- Replace the board wholesale. The project id is already the caller's own
-- (currentProject resolves it inside the tenant), so the row simply follows
-- the project: a board left behind by an earlier tenant of a released and
-- re-claimed project is overwritten, tenant and all, rather than silently
-- kept while the save answers 200. A real write also resolves any pending
-- proposal: the proposal was about the board that just changed.
INSERT INTO dashboard (tenant_id, project_id, layout, written_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id) DO UPDATE
   SET tenant_id = EXCLUDED.tenant_id, layout = EXCLUDED.layout,
       written_by = EXCLUDED.written_by,
       proposed = NULL, proposed_at = NULL, updated_at = now();

-- name: ProposeDashboard :exec
-- The key's answer to a curated board: keep the offered layout beside it,
-- never over it. The tenant rides in the predicate.
UPDATE dashboard SET proposed = $3, proposed_at = now()
 WHERE tenant_id = $1 AND project_id = $2;

-- name: GetDashboardProposal :one
-- The pending proposal, NULL when there is none. The tenant rides in the
-- predicate.
SELECT proposed FROM dashboard WHERE tenant_id = $1 AND project_id = $2;

-- name: ClearDashboardProposal :exec
-- Drop the proposal without applying it. The tenant rides in the predicate.
UPDATE dashboard SET proposed = NULL, proposed_at = NULL
 WHERE tenant_id = $1 AND project_id = $2;

-- name: AppendDashboardLayout :exec
-- The append path's write: the layout alone. Provenance is NOT touched, because
-- appending to a curated board keeps it curated, and the pending proposal is NOT
-- cleared, because only the reader resolves an offer the agent made them. The
-- tenant rides in the predicate.
UPDATE dashboard SET layout = $3, updated_at = now()
 WHERE tenant_id = $1 AND project_id = $2;
