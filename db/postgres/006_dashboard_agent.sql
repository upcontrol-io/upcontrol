-- +goose Up
-- +goose StatementBegin
-- Who made this board, and what a key offered for it. Provenance exists
-- because "does a board exist?" was only ever an approximation of "did a
-- human curate this board?": a key that can replace a curated board is a
-- wipe waiting to leak, while a key that can replace its OWN board is not.
-- 'session' is the default on purpose — every board that exists today was
-- saved from a browser, and that is what the column must say about them.
-- `proposed` holds a replacement the key offered for a curated board: kept,
-- never applied, waiting for one click in the app.
ALTER TABLE dashboard
  ADD COLUMN written_by  text NOT NULL DEFAULT 'session',
  ADD COLUMN proposed    jsonb,
  ADD COLUMN proposed_at timestamptz;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE dashboard
  DROP COLUMN IF EXISTS proposed_at,
  DROP COLUMN IF EXISTS proposed,
  DROP COLUMN IF EXISTS written_by;
-- +goose StatementEnd
