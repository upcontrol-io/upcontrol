-- +goose Up
-- +goose StatementBegin
-- One board per project: the layout the dashboard draws, stored whole and
-- overwritten whole on every save (last write wins). tenant_id rides along
-- for the tenant-scoped-table invariant and the read's own guard; the project
-- is the key, so a deleted project takes its board with it.
CREATE TABLE dashboard (
  tenant_id     bigint NOT NULL,
  project_id    bigint PRIMARY KEY REFERENCES project(id) ON DELETE CASCADE,
  layout        jsonb NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS dashboard;
-- +goose StatementEnd
