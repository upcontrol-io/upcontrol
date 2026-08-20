-- Finite monthly AI-explain quotas on the paid plans (owner decision, recorded
-- in docs/plans/ai-explain-hardening.md's Ask User: Indie/Growth/Agency →
-- 50/200/400).
--
-- Until now every paid plan carried ai_explains = NULL, which GET /v1/plan
-- reads as "unlimited" and omits from the response entirely. That left the
-- per-replica 6/min throttle as the only bound on provider spend for three of
-- the four plans: an authenticated tenant could run explains all month and the
-- bill is ours. A number here is also what lets the plan panel draw the axis at
-- all — a NULL renders nothing, so nobody could see what they had used.
--
-- Free stays at 5. The numbers match the row in front/src/pages/Pricing.tsx,
-- which owns them (a gate may not exist without its row, and the row may not
-- promise a mechanism that does not).

-- +goose Up
-- +goose StatementBegin
UPDATE plan_entitlement SET ai_explains =  50 WHERE plan = 'Indie';
UPDATE plan_entitlement SET ai_explains = 200 WHERE plan = 'Growth';
UPDATE plan_entitlement SET ai_explains = 400 WHERE plan = 'Agency';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE plan_entitlement SET ai_explains = NULL WHERE plan IN ('Indie', 'Growth', 'Agency');
-- +goose StatementEnd
