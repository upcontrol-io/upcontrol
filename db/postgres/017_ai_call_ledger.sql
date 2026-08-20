-- +goose Up
-- +goose StatementBegin
-- Per-call token ledger (docs/plans/ai-provider-and-scenarios.md, D9/D11).
--   * ai_usage gains monthly token totals alongside the existing call count;
--     the count stays the plan quota axis (D8), tokens are bookkeeping only.
--   * ai_call is one row per real LLM call. Cache hits and heuristic answers
--     insert nothing — no tokens were spent (D9).

ALTER TABLE ai_usage
  ADD COLUMN prompt_tokens bigint NOT NULL DEFAULT 0,
  ADD COLUMN completion_tokens bigint NOT NULL DEFAULT 0;

CREATE TABLE ai_call (
  id                bigserial PRIMARY KEY,
  tenant_id         bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  scenario          text NOT NULL,                 -- registry key, e.g. explain_logs
  model             text NOT NULL,
  prompt_tokens     bigint NOT NULL,
  completion_tokens bigint NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_call_tenant_created_idx ON ai_call (tenant_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE ai_call;
ALTER TABLE ai_usage DROP COLUMN completion_tokens;
ALTER TABLE ai_usage DROP COLUMN prompt_tokens;
-- +goose StatementEnd
