-- +goose Up
-- +goose StatementBegin
-- A project keeps a SET of keys, not one key it rotates. The table always
-- allowed it — the only unique index is on `prefix` — and the resolver already
-- looks a key up by prefix and honours `revoked`. What was missing is a way to
-- tell two keys apart and a record of when one was withdrawn.

-- Empty is a real name, not a placeholder: every key issued before this
-- migration has one, and the app prints the prefix when the name is blank.
ALTER TABLE api_key ADD COLUMN name text NOT NULL DEFAULT '';

-- When the key was withdrawn, kept beside `state = 'revoked'` rather than
-- instead of it: the state is what the resolver reads on every ingest batch,
-- and this is what a person reads when they are working out what leaked and
-- when. A revoked row is never deleted for the same reason — `last_used_at` on
-- a key someone withdrew is evidence.
ALTER TABLE api_key ADD COLUMN revoked_at timestamptz;

-- The list is read per project on every Connect page load.
CREATE INDEX IF NOT EXISTS api_key_project_state_idx ON api_key (project_id, state);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS api_key_project_state_idx;
ALTER TABLE api_key DROP COLUMN IF EXISTS revoked_at;
ALTER TABLE api_key DROP COLUMN IF EXISTS name;
-- +goose StatementEnd
