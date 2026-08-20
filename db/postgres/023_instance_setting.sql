-- Instance-level settings written from the app itself. The first tenant of
-- this table is the AI API key the self-host Settings screen takes (owner
-- decision, 2026-08-20: the heuristic fallback is removed — no key means
-- Explain is off, and the key can be pasted into the UI instead of a secret
-- file). Values are sealed with UC_SECRET_KEY_HEX (AES-256-GCM) before they
-- land here; the hosted cloud never writes this table — its keys arrive as
-- secret files, and the writing endpoint answers 404 off a self-host.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE instance_setting (
  key        text PRIMARY KEY,
  value_enc  bytea NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE instance_setting;
-- +goose StatementEnd
