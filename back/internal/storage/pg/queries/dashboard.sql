-- Dashboard queries: a project's named boards, each read and replaced whole.
-- Every one of them carries the tenant in its predicate even though the id is
-- the key: an id from another tenant, or from a sibling project of the same
-- tenant, must read as "no board" rather than as somebody's board.

-- name: ListBoards :many
-- The project's boards in creation order, which is also the order the freeze
-- counts in: rank 1 is the alias, and every rank past the plan's `dashboards`
-- is frozen. The widget count travels instead of the document — a list of five
-- boards must not carry five layouts.
SELECT id, public_id, name, written_by,
       (proposed IS NOT NULL)::bool AS proposed,
       -- A nil widget slice marshals as `null`, and jsonb_array_length raises
       -- on a scalar: a board stored from a document that named no widgets
       -- would take the whole list down with it.
       COALESCE(jsonb_array_length(
         CASE WHEN jsonb_typeof(layout -> 'widgets') = 'array' THEN layout -> 'widgets' END), 0)::int AS widgets,
       row_number() OVER (ORDER BY created_at, id) AS rank
  FROM dashboard WHERE tenant_id = $1 AND project_id = $2
 ORDER BY created_at, id;

-- name: ResolveBoard :one
-- One board by public id, or by the alias (the oldest), with everything the
-- doors need in one round trip: the document, the offer waiting on it and the
-- rank the freeze counts in. An id that resolves inside the caller's tenant
-- AND project is the only id that resolves at all.
SELECT id, public_id, name, written_by, layout, proposed, rank FROM (
  SELECT id, public_id, name, written_by, layout, proposed,
         row_number() OVER (ORDER BY created_at, id) AS rank
    FROM dashboard
   WHERE tenant_id = sqlc.arg(tenant_id) AND project_id = sqlc.arg(project_id)
) b
 WHERE (sqlc.arg(alias)::bool AND rank = 1)
    OR (NOT sqlc.arg(alias)::bool AND public_id = sqlc.arg(public_id));

-- name: CountBoards :one
SELECT count(*) FROM dashboard WHERE tenant_id = $1 AND project_id = $2;

-- name: CreateBoard :one
-- A new board. A name the project already holds (case-insensitively) conflicts
-- and the insert answers no row at all, which the door turns into 409
-- name_taken: a create must never quietly become a write onto somebody's board.
INSERT INTO dashboard (tenant_id, project_id, name, layout, written_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id, lower(name)) DO NOTHING
RETURNING id, public_id, name;

-- name: UpsertBoard :one
-- The alias's first write: it lays the row down, and a row of the same name
-- left behind by an earlier tenant of a released and re-claimed project is
-- taken over, tenant and all, rather than silently kept while the save answers
-- 200. That self-heal is the old PutDashboard's, kept whole. A row of the SAME
-- tenant is a Main somebody laid down after the caller read the project as
-- empty: it answers no row, and the caller writes onto that board instead.
INSERT INTO dashboard (tenant_id, project_id, name, layout, written_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id, lower(name)) DO UPDATE
   SET tenant_id = EXCLUDED.tenant_id, layout = EXCLUDED.layout,
       written_by = EXCLUDED.written_by,
       proposed = NULL, proposed_at = NULL, updated_at = now()
 WHERE dashboard.tenant_id <> EXCLUDED.tenant_id
RETURNING id, public_id, name;

-- name: PutBoardLayout :execrows
-- Replace one board. By id, and :execrows rather than :exec: a write that
-- matched no row must not answer 200 for a board the caller never reached. A
-- real write also resolves any pending proposal — the proposal was about the
-- board that just changed.
UPDATE dashboard
   SET layout = $3, written_by = $4, proposed = NULL, proposed_at = NULL, updated_at = now()
 WHERE id = $1 AND tenant_id = $2;

-- name: AppendBoardLayout :execrows
-- The append path's write: the layout alone. Provenance is NOT touched, because
-- appending to a curated board keeps it curated, and the pending proposal is NOT
-- cleared, because only the reader resolves an offer the agent made them.
UPDATE dashboard SET layout = $3, updated_at = now()
 WHERE id = $1 AND tenant_id = $2;

-- name: ProposeBoard :execrows
-- The key's answer to a curated board: keep the offered layout beside it,
-- never over it.
UPDATE dashboard SET proposed = $3, proposed_at = now()
 WHERE id = $1 AND tenant_id = $2;

-- name: ClearBoardProposal :execrows
-- Drop the proposal without applying it.
UPDATE dashboard SET proposed = NULL, proposed_at = NULL
 WHERE id = $1 AND tenant_id = $2;

-- name: RenameBoard :execrows
-- A name is not part of the layout, so a rename never touches provenance. A
-- name the project already holds trips the unique index; the door reads that
-- as 409 name_taken.
UPDATE dashboard SET name = $3, updated_at = now()
 WHERE id = $1 AND tenant_id = $2;

-- name: DeleteBoard :execrows
DELETE FROM dashboard WHERE id = $1 AND tenant_id = $2;
