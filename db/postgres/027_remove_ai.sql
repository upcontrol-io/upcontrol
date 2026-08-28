-- +goose Up
-- +goose StatementBegin
-- AI Explain is removed from the product (Aug 28, 2026, user decision): no LLM
-- triage of logs or incidents, so the ledger, the cache, the quota axis, the
-- project spec that existed only as explain context and the instance-level
-- provider settings all go with it.
DROP TABLE ai_call;
DROP TABLE ai_explain_cache;
DROP TABLE ai_usage;
ALTER TABLE plan_entitlement DROP COLUMN ai_explains;
ALTER TABLE project DROP COLUMN meta;
DELETE FROM instance_setting WHERE key IN ('ai_api_key','ai_model','ai_base_url');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Shapes only: the dropped rows and the deleted instance_setting values are
-- gone for good, nothing here restores them.
CREATE TABLE ai_usage (
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  month         date NOT NULL,
  used          int NOT NULL DEFAULT 0,
  prompt_tokens bigint NOT NULL DEFAULT 0,
  completion_tokens bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, month)
);

CREATE TABLE ai_explain_cache (
  tenant_id     bigint NOT NULL,
  input_hash    bytea NOT NULL,
  text          text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, input_hash)
);

CREATE TABLE ai_call (
  id                bigserial PRIMARY KEY,
  tenant_id         bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  scenario          text NOT NULL,
  model             text NOT NULL,
  prompt_tokens     bigint NOT NULL,
  completion_tokens bigint NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_call_tenant_created_idx ON ai_call (tenant_id, created_at);

ALTER TABLE plan_entitlement ADD COLUMN ai_explains int;
ALTER TABLE project ADD COLUMN meta jsonb;
-- +goose StatementEnd
