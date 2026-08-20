-- +goose Up
-- +goose StatementBegin
-- Anonymous projects (cli/SPEC.md §7.1, docs/plans/one-command-install.md):
-- `npx upcontrol init` provisions a tenant+project+key with NO person attached,
-- so data can flow before any registration. The claim token is the one-time
-- proof of provenance: whoever presents it (signed in) becomes a member of the
-- tenant. Stored as sha256, like magic-link codes — the raw token exists only
-- in the CLI's output. Claiming clears the hash (one-time) and stamps
-- claimed_at; claiming never changes the API key (a rotation would require a
-- release of the customer's deployed app).
ALTER TABLE tenant ADD COLUMN claim_token_hash bytea;
ALTER TABLE tenant ADD COLUMN claimed_at timestamptz;
CREATE UNIQUE INDEX tenant_claim_token_key ON tenant (claim_token_hash)
  WHERE claim_token_hash IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX tenant_claim_token_key;
ALTER TABLE tenant DROP COLUMN claimed_at;
ALTER TABLE tenant DROP COLUMN claim_token_hash;
-- +goose StatementEnd
