-- +goose Up
-- +goose StatementBegin
-- The history axis: how far back the dashboard's series reach, in days. NULL
-- is unlimited (Self-hosted). POST /v1/series is the gate; the raw log ring
-- (window_lines / window_hours) and incident_days are their own windows.
ALTER TABLE plan_entitlement ADD COLUMN history_days int;
UPDATE plan_entitlement SET history_days = CASE plan
  WHEN 'Free' THEN 1 WHEN 'Indie' THEN 7 WHEN 'Growth' THEN 31 WHEN 'Agency' THEN 365 END;

-- series_1h is the board's rollup: hourly line counts per service, level and
-- message fingerprint, upserted at ingest next to series_1m (pgstore.BumpHistory).
-- The 7d, 31d and 365d ranges step in whole hours, so they read this table
-- instead of scanning the ring; sub-day ranges keep reading logs. ucworker's
-- history-trim job drops rows older than the tenant's history_days, so an
-- upgrade starts counting from the day it happens. Fingerprint is the same
-- int64-wrapped hash logs.fingerprint carries.
CREATE TABLE series_1h (
  tenant_id     bigint NOT NULL,
  project_id    bigint NOT NULL,
  hour          timestamptz NOT NULL,
  service       text NOT NULL,
  level         text NOT NULL,
  fingerprint   bigint NOT NULL,
  lines         bigint NOT NULL,
  PRIMARY KEY (tenant_id, project_id, hour, service, level, fingerprint)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS series_1h;
ALTER TABLE plan_entitlement DROP COLUMN IF EXISTS history_days;
-- +goose StatementEnd
