-- Telegram identity for the alerts user workflows (Phase 1 of
-- docs/plans/alerts-workflows-1-5.md). Three columns:
--   * person.telegram_username: the bot has always received from.username and
--     never stored it; a personal chat label reads "Name @handle";
--   * alert_channel.label: the human-readable name of a chat (the person's
--     name, or the group's title), so /app prints it instead of a raw chat id;
--   * telegram_invite.person_id: an invite can be bound to one person, so the
--     link works once and only for them.
-- The backfill names existing personal chats from person.name (the best fact
-- on file today); group titles arrive the next time a group redeems a link.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE person ADD COLUMN telegram_username text;
ALTER TABLE alert_channel ADD COLUMN label text;
ALTER TABLE telegram_invite ADD COLUMN person_id bigint REFERENCES person(id) ON DELETE CASCADE;
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE alert_channel ac
   SET label = p.name
  FROM person p
 WHERE ac.recipient_person_id = p.id
   AND ac.kind = 'telegram'
   AND p.name <> '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE telegram_invite DROP COLUMN person_id;
ALTER TABLE alert_channel DROP COLUMN label;
ALTER TABLE person DROP COLUMN telegram_username;
-- +goose StatementEnd
