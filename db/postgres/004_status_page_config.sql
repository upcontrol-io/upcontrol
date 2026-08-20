-- +goose Up
-- +goose StatementBegin
-- The status page's own settings: which components are shown, and whether the
-- network strip and the incident history are published. They live in their own
-- column rather than inside `components`, which holds the published list.
ALTER TABLE status_page ADD COLUMN config jsonb NOT NULL DEFAULT '{}'::jsonb;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE status_page DROP COLUMN config;
-- +goose StatementEnd
