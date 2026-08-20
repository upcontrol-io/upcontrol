-- 002_metric_rollups.sql — the two missing metric rollups. The tables
-- (metrics_5m, metrics_1h) were declared in 001 with no writer; these views
-- fill them the same way series_1m_mv and checks_1m_mv already work.
-- Never edit 001 — add the next file.

CREATE MATERIALIZED VIEW IF NOT EXISTS metrics_5m_mv TO metrics_5m AS
SELECT tenant_id, project_id, toStartOfInterval(ts, INTERVAL 5 MINUTE) AS bucket,
       name, avgState(value) AS value_state
FROM metrics GROUP BY tenant_id, project_id, bucket, name;

CREATE MATERIALIZED VIEW IF NOT EXISTS metrics_1h_mv TO metrics_1h AS
SELECT tenant_id, project_id, toStartOfInterval(ts, INTERVAL 1 HOUR) AS bucket,
       name, avgState(value) AS value_state
FROM metrics GROUP BY tenant_id, project_id, bucket, name;
