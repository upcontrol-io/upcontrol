-- name: GetCachedExplain :one
-- Rows stop being served after one hour: the volatile server context is sent
-- to the model but excluded from the hash (Decision 7), so without a bound a
-- cached answer could keep asserting an open incident that has since
-- resolved. An hour is far past any incident flap (minutes), which is what
-- the exclusion protects; an expired row is simply a miss — the fresh answer
-- overwrites it. Nothing deletes rows; the table is bounded by overwrite.
SELECT text FROM ai_explain_cache
WHERE tenant_id = $1 AND input_hash = $2 AND created_at > now() - interval '1 hour';

-- name: CacheExplain :exec
-- created_at is refreshed on overwrite too: without it an upsert would leave
-- the row stamped with its first write and serve nothing for another hour
-- after the fresh answer landed.
INSERT INTO ai_explain_cache (tenant_id, input_hash, text)
VALUES ($1, $2, $3)
ON CONFLICT (tenant_id, input_hash) DO UPDATE
SET text = EXCLUDED.text, created_at = now();

-- name: IncrementAIUsage :one
-- `used` counts calls (the plan quota axis); the token columns accumulate
-- monthly totals for cost analysis. Cache hits never reach this query.
-- The upsert is the quota gate itself: the guards refuse the increment once
-- the limit is spent — on the existing row and on a fresh insert alike — so
-- two explains racing for the last slot cannot both take it; no row returned
-- means over quota. quota_limit NULL (a plan's ai_explains NULL, an
-- unlimited plan) skips both guards; the parameter's nullness is the one
-- unlimited sentinel, so a plan row storing 0 AI explains refuses everything
-- — never the reverse (001_init.sql documents NULL = unlimited).
INSERT INTO ai_usage (tenant_id, month, used, prompt_tokens, completion_tokens)
SELECT $1, $2, 1, $3, $4
WHERE sqlc.narg(quota_limit)::int IS NULL OR sqlc.narg(quota_limit)::int > 0
ON CONFLICT (tenant_id, month) DO UPDATE SET
  used = ai_usage.used + 1,
  prompt_tokens = ai_usage.prompt_tokens + $3,
  completion_tokens = ai_usage.completion_tokens + $4
WHERE sqlc.narg(quota_limit)::int IS NULL OR ai_usage.used < sqlc.narg(quota_limit)::int
RETURNING used;

-- name: AccumulateAITokens :exec
-- Token-only accumulation for an unanswered call that still burned tokens:
-- the quota guard refusing the last slot after the model answered, or a
-- stream failing after spend started (a max_tokens cut). The tokens are real
-- spend and belong in the monthly totals, but `used` never moves — no answer
-- was delivered against the quota. Called from recordUnansweredCall in
-- internal/ai for both reasons (answer_dropped = quota_refused |
-- provider_error).
INSERT INTO ai_usage (tenant_id, month, used, prompt_tokens, completion_tokens)
VALUES ($1, $2, 0, $3, $4)
ON CONFLICT (tenant_id, month) DO UPDATE SET
  prompt_tokens = ai_usage.prompt_tokens + $3,
  completion_tokens = ai_usage.completion_tokens + $4;

-- name: InsertAICall :exec
-- One row per provider call that answered or burned tokens: answered calls
-- plus the unanswered-but-paid ones (quota-refused after the model answered,
-- a stream cut at max_tokens). Cache hits and calls that never reached a
-- chunk (connection refused, a non-2xx before any chunk) insert nothing —
-- there is no spend to record.
INSERT INTO ai_call (tenant_id, scenario, model, prompt_tokens, completion_tokens)
VALUES ($1, $2, $3, $4, $5);

-- name: GetAIUsage :one
SELECT used FROM ai_usage WHERE tenant_id = $1 AND month = $2;
