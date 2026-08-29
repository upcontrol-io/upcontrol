-- +goose Up
-- +goose StatementBegin
-- Custom domains for status pages. The page's own columns (domain,
-- domain_verified_at) already exist in 001; this adds only the plan axis —
-- which plans may point a page at their own host. No index is added on
-- status_page.domain: the UNIQUE constraint from 001 already provides one,
-- and NULLs do not collide there, which is exactly what the many pages
-- without a domain need.
ALTER TABLE plan_entitlement ADD COLUMN custom_domain boolean NOT NULL DEFAULT false;
UPDATE plan_entitlement SET custom_domain = true WHERE plan <> 'Free';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE plan_entitlement DROP COLUMN custom_domain;
-- +goose StatementEnd
