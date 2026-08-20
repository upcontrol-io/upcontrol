-- +goose Up
-- +goose StatementBegin
-- Telegram identity + recipients (openspec/changes/telegram-bot-auth-and-recipients).
-- Decisions (D1/D2/D4/D9, closed Aug 2026):
--   * numbers 3/10/30/100 — same scale as http_checks (owner-approved default);
--   * the owner counts inside the limit: Free = the owner's own chat + 2 teammates;
--   * a recipient is a person (alert_channel.recipient_person_id), a broadcast
--     group is a channel with recipient_person_id NULL.

-- One-time invite tokens: the deep link `t.me/<bot>?start=inv_<token>` is the
-- only way to link a Telegram account to a person (the old `prj-N` link was
-- guessable). Stored as sha256 like magic-link codes and claim tokens; the raw
-- token appears exactly once in the POST response and never in a log.
CREATE TABLE telegram_invite (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  role          text NOT NULL,                    -- notify|login|owner
  invited_by    bigint NOT NULL REFERENCES person(id) ON DELETE CASCADE,
  token_hash    bytea NOT NULL UNIQUE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  redeemed_at   timestamptz
);

-- A personal telegram destination points at the person it alerts; a broadcast
-- group keeps it NULL (D4/D5). muted_until is the /mute window.
ALTER TABLE alert_channel ADD COLUMN recipient_person_id bigint REFERENCES person(id);
ALTER TABLE alert_channel ADD COLUMN muted_until timestamptz;

ALTER TABLE plan_entitlement ADD COLUMN telegram_recipients int NOT NULL DEFAULT 3;
UPDATE plan_entitlement SET telegram_recipients = 10 WHERE plan = 'Indie';
UPDATE plan_entitlement SET telegram_recipients = 30 WHERE plan = 'Growth';
UPDATE plan_entitlement SET telegram_recipients = 100 WHERE plan = 'Agency';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE plan_entitlement DROP COLUMN telegram_recipients;
ALTER TABLE alert_channel DROP COLUMN muted_until;
ALTER TABLE alert_channel DROP COLUMN recipient_person_id;
DROP TABLE telegram_invite;
-- +goose StatementEnd
