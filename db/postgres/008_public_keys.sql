-- +goose Up
-- +goose StatementBegin
-- A key that may live in a browser bundle. The existing key is a bare write credential with no
-- scope at all, which is why no SPA could ever use this product: putting it in a bundle ships
-- it to every visitor. A public key is narrower on both axes — it may only write named events,
-- and only from an origin its owner listed.
--
-- `origins` empty on a public key is a refusal, never "any": a public key with no domain is the
-- unscoped key it exists to replace.
ALTER TABLE api_key ADD COLUMN kind text NOT NULL DEFAULT 'secret';
ALTER TABLE api_key ADD COLUMN origins text[] NOT NULL DEFAULT '{}';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE api_key DROP COLUMN IF EXISTS origins;
ALTER TABLE api_key DROP COLUMN IF EXISTS kind;
-- +goose StatementEnd
