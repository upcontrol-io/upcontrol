-- +goose Up
-- +goose StatementBegin
-- Monetization v2 (owner decisions, 2026-08-31). Two changes to the plan axes:
--
--   1. Telegram groups and channels become paid destinations. telegram_rooms
--      gates the broadcast redeem (a group/channel chat pressing an invite);
--      personal connects stay on every plan. The recipients ladder flattens to
--      3/10/20/30 — the axis now counts destinations (people, groups, channels),
--      so the old 100-seat Agency cell priced a different thing.
--
--   2. Incident history becomes a real window. Reads clamp closed incidents to
--      incident_days (open ones always show); the worker hard-deletes only past
--      the widest plan's window, so an upgrade instantly restores history.
ALTER TABLE plan_entitlement ADD COLUMN telegram_rooms boolean NOT NULL DEFAULT true;
UPDATE plan_entitlement SET telegram_rooms = false WHERE plan = 'Free';
UPDATE plan_entitlement SET telegram_recipients = 20 WHERE plan = 'Growth';
UPDATE plan_entitlement SET telegram_recipients = 30 WHERE plan = 'Agency';
UPDATE plan_entitlement SET incident_days = 10 WHERE plan = 'Free';
UPDATE plan_entitlement SET incident_days = 1460 WHERE plan = 'Agency';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE plan_entitlement DROP COLUMN telegram_rooms;
UPDATE plan_entitlement SET telegram_recipients = 30 WHERE plan = 'Growth';
UPDATE plan_entitlement SET telegram_recipients = 100 WHERE plan = 'Agency';
UPDATE plan_entitlement SET incident_days = 30 WHERE plan = 'Free';
UPDATE plan_entitlement SET incident_days = 730 WHERE plan = 'Agency';
-- +goose StatementEnd
