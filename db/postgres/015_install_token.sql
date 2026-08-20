-- +goose Up
-- +goose StatementBegin
-- One-time install tokens (docs/plans/front-distribution-alignment.md §1):
-- the dashboard's install card generates `npx upcontrol init --token uct_...`
-- so a signed-in user's CLI lands the key of THEIR project instead of minting
-- an anonymous one. The token is the only thing that ever appears on screen;
-- redeeming it issues an additional api_key row and returns the secret once.
-- Stored as sha256 like magic-link codes and claim tokens; single-use via
-- used_at; short TTL because it exists only to cross from browser to terminal.
CREATE TABLE install_token (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  token_hash    bytea NOT NULL UNIQUE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  used_at       timestamptz
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE install_token;
-- +goose StatementEnd
