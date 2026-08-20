-- +goose Up
-- +goose StatementBegin
-- One connection per kind, per project. Pressing "Deploy hooks" on /app/sources
-- inserted a row every time, so a second click left the screen with two
-- identical cards (and a third with three) — each with its own Pause and its own
-- Disconnect, describing the same single webhook endpoint. There is exactly one
-- URL per provider (hookUrl(kind) has no per-row part), so a second row could
-- never mean anything different from the first.
--
-- Dedupe before the index: keep the row that has actually heard from the
-- provider (last_signal_at), and among equals the oldest — deleting the one with
-- signals would throw away the only evidence the feed works.
DELETE FROM source_connection
 WHERE id NOT IN (
   SELECT DISTINCT ON (project_id, kind) id
     FROM source_connection
    ORDER BY project_id, kind, last_signal_at DESC NULLS LAST, id
 );

CREATE UNIQUE INDEX source_connection_project_kind_key
  ON source_connection (project_id, kind);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS source_connection_project_kind_key;
-- +goose StatementEnd
