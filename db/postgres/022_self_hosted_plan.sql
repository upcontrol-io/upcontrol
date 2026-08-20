-- The OSS install's plan (docs/plans/public-first-split.md, Decision 7):
-- assigned by UC_SELF_HOSTED=1 at tenant creation, never purchasable, never on
-- the pricing page. Explicit column list on purpose — 010 and 016 added
-- columns with defaults, and a positional insert would silently take
-- telegram_recipients' DEFAULT 3 instead of the intended 1000.
-- The row also lands on the cloud database, unused: migration sequence
-- integrity beats a conditional seed.

-- +goose Up
-- +goose StatementBegin
INSERT INTO plan_entitlement
  (plan, http_checks, regions, window_lines, window_hours, retain_mult,
   ai_explains, incident_days, min_interval_sec, telegram_recipients)
VALUES
  ('Self-hosted', 1000, 10, 45000000, 720, 1.0, NULL, 730, 60, 1000);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM plan_entitlement WHERE plan = 'Self-hosted';
-- +goose StatementEnd
