-- +goose Up
-- +goose StatementBegin
-- What a channel is notified about (docs/plans/channel-notify-settings.md).
-- Sparse jsonb: an absent key means the default (websiteDown on, everything
-- else off), so every existing row keeps today's behaviour with no backfill.
-- This narrows "a channel is a destination and nothing else" (user decision,
-- Aug 14, 2026): the settings pick which CLASSES of alert land here — they are
-- not, and must not become, a per-monitor routing matrix.
ALTER TABLE alert_channel ADD COLUMN notify jsonb NOT NULL DEFAULT '{}'::jsonb;
-- +goose StatementEnd

-- +goose StatementBegin
-- The error-log scanner's memory of what it already alerted: without it a
-- persisting error would page again on every 60s scan. kind is which category
-- fired ('error'|'repeat') — the two have different cooldowns.
CREATE TABLE error_alert_state (
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  fingerprint   bigint NOT NULL,
  kind          text NOT NULL,
  last_alerted  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, fingerprint, kind)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE error_alert_state;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE alert_channel DROP COLUMN notify;
-- +goose StatementEnd
