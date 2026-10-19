-- +goose Up
-- +goose StatementBegin
-- Product alerts: a rule on one event of a project (its count, or the money it
-- carried in one currency) that tells the project's alert channels when the
-- last window moved past a threshold against the same hours a week ago, or
-- went silent. The worker's product-alerts job evaluates every live rule and
-- keeps the episode here: one message when it starts, one when it ends.
CREATE TABLE product_alert (
  id              bigserial PRIMARY KEY,
  public_id       uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
  tenant_id       bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id      bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  name            text NOT NULL DEFAULT '',
  event           text NOT NULL,
  labels          jsonb NOT NULL DEFAULT '{}',
  measure         text NOT NULL DEFAULT '' CHECK (measure IN ('', 'amount')),
  currency        text NOT NULL DEFAULT '',
  kind            text NOT NULL CHECK (kind IN ('change', 'silence')),
  direction       text NOT NULL DEFAULT 'any' CHECK (direction IN ('down', 'up', 'any')),
  percent         int NOT NULL DEFAULT 50,
  window_hours    int NOT NULL CHECK (window_hours IN (1, 3, 6, 12, 24)),
  widget_id       text NOT NULL DEFAULT '',
  paused          boolean NOT NULL DEFAULT false,
  -- The episode: firing since last_fired_at; learning when the week before
  -- holds too little to compare with, so the rule keeps quiet.
  state           text NOT NULL DEFAULT 'learning' CHECK (state IN ('ok', 'firing', 'learning')),
  last_fired_at   timestamptz,
  last_checked_at timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX product_alert_by_tenant ON product_alert (tenant_id, created_at);
CREATE INDEX product_alert_by_project ON product_alert (project_id, created_at);

-- The rule count is a workspace axis, NULL = unlimited: Free keeps two, every
-- paid plan and Self-hosted have no ceiling (owner decision, 2026-10-19).
ALTER TABLE plan_entitlement ADD COLUMN product_alerts int;
UPDATE plan_entitlement SET product_alerts = CASE plan WHEN 'Free' THEN 2 END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE plan_entitlement DROP COLUMN IF EXISTS product_alerts;
DROP TABLE IF EXISTS product_alert;
-- +goose StatementEnd
