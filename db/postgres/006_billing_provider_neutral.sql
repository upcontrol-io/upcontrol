-- +goose Up
-- +goose StatementBegin
-- Recreated, not migrated: the LemonSqueezy table never held a production row, and the
-- provider now in its place (Creem) uses string ids. Written only by the hosted billing
-- sidecar; tenant.plan is derived from this row (measured, not asserted).
DROP TABLE billing_subscription;
CREATE TABLE billing_subscription (
  tenant_id                bigint PRIMARY KEY REFERENCES tenant(id) ON DELETE CASCADE,
  provider                 text NOT NULL,          -- creem
  provider_customer_id     text NOT NULL,
  provider_subscription_id text NOT NULL UNIQUE,
  product_id               text NOT NULL,          -- the provider's product: it names plan and interval
  status                   text NOT NULL,          -- provider status verbatim: active|trialing|past_due|unpaid|paused|scheduled_cancel|canceled|expired
  period_end               timestamptz,            -- end of the paid period: the renewal date, or when a cancelled plan lapses
  canceled_at              timestamptz,
  updated_at               timestamptz NOT NULL DEFAULT now(),
  created_at               timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON billing_subscription (provider_customer_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE billing_subscription;
CREATE TABLE billing_subscription (
  tenant_id          bigint PRIMARY KEY REFERENCES tenant(id) ON DELETE CASCADE,
  ls_customer_id     bigint NOT NULL,
  ls_subscription_id bigint NOT NULL UNIQUE,
  variant_id         bigint NOT NULL,
  status             text NOT NULL,
  renews_at          timestamptz,
  ends_at            timestamptz,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON billing_subscription (ls_customer_id);
-- +goose StatementEnd
