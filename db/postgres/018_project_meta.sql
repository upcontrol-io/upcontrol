-- +goose Up
-- +goose StatementBegin
-- Installer-collected project spec (docs/plans/ai-provider-and-scenarios.md,
-- D15b/D16): {name, description, framework, runtime, language} plus storage
-- bookkeeping (source, collectedAt). NULL = never collected. Written only by
-- PUT /v1/project/meta after scrubbing; read by the AI explain context block.

ALTER TABLE project ADD COLUMN meta jsonb;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE project DROP COLUMN meta;
-- +goose StatementEnd
