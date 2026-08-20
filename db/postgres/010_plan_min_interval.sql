-- +goose Up
-- +goose StatementBegin
-- How often a check may run is now a plan axis (Aug 14, 2026, user decision):
-- Free runs every 5 minutes, paid plans down to the minute. It lives here rather
-- than in Go because every other plan number does — one row per plan, read by
-- the create/patch gate and by GET /v1/plan, so the client never hardcodes it.
--
-- The default is 60s: a plan added later gets the floor the fleet is actually
-- sized against (backend-from-new-plan.md §2.3), never an unbounded one.
ALTER TABLE plan_entitlement ADD COLUMN min_interval_sec int NOT NULL DEFAULT 60;

UPDATE plan_entitlement SET min_interval_sec = 300 WHERE plan = 'Free';

-- Checks that already run faster than their plan now allows are left alone: the
-- floor is a gate on what you may ask for, not a reason to silently slow down a
-- monitor somebody is relying on.
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE plan_entitlement DROP COLUMN min_interval_sec;
-- +goose StatementEnd
