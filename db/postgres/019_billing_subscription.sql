-- +goose Up
-- +goose StatementBegin
-- LemonSqueezy billing state, one row per tenant. The webhook
-- (/hooks/lemonsqueezy) is the only writer: tenant.plan / tenant.billing are
-- derived from it on every subscription event, so the entitlement gates read
-- the same facts the money side wrote (docs/rules/data-layer.md: measured,
-- not asserted — a plan a webhook never confirmed is Free).
CREATE TABLE billing_subscription (
  tenant_id          bigint PRIMARY KEY REFERENCES tenant(id) ON DELETE CASCADE,
  ls_customer_id     bigint NOT NULL,
  ls_subscription_id bigint NOT NULL UNIQUE,
  variant_id         bigint NOT NULL,
  status             text NOT NULL,              -- LS status verbatim: on_trial|active|paused|past_due|unpaid|cancelled|expired
  renews_at          timestamptz,
  ends_at            timestamptz,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON billing_subscription (ls_customer_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE billing_subscription;
-- +goose StatementEnd
