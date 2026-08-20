-- +goose Up
-- +goose StatementBegin
-- Universal inbound hooks (Aug 14, 2026, user decision — docs/plans/
-- universal-hooks.md): every source connection carries its own hook token, and
-- POST /hooks/{token} attributes the event to the connection's tenant and
-- project. The token in the URL is the credential (128 bits, per connection,
-- revoked by disconnecting), which is what lets ANY provider that can POST
-- JSON use the endpoint — the three named providers had one global secret
-- each and wrote every event as tenant 0.
ALTER TABLE source_connection ADD COLUMN hook_token text;

-- Existing rows get a backfill token so their URL exists the moment the front
-- asks for it. md5(random) is fine for a backfill: new tokens come from
-- crypto/rand in Go, and a token's job is to be unguessable-enough to name a
-- write-only event sink, not to be a session credential.
UPDATE source_connection
   SET hook_token = md5(random()::text || clock_timestamp()::text || id::text)
 WHERE hook_token IS NULL;

CREATE UNIQUE INDEX source_connection_hook_token_key
    ON source_connection (hook_token);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX source_connection_hook_token_key;
ALTER TABLE source_connection DROP COLUMN hook_token;
-- +goose StatementEnd
