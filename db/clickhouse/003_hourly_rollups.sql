-- 003_hourly_rollups.sql — the two missing hourly rollups. series_1h and
-- checks_1h were declared in 001 with no writer, exactly like metrics_5m and
-- metrics_1h before 002. These follow the _1m views' pattern, aggregated from
-- the raw tables at hour granularity.
-- Never edit 001 — add the next file.

CREATE MATERIALIZED VIEW IF NOT EXISTS series_1h_mv TO series_1h AS
SELECT tenant_id, project_id, toStartOfHour(ts) AS hour,
       source, level, count() AS lines, sum(length(message)) AS bytes
FROM logs GROUP BY tenant_id, project_id, hour, source, level;

CREATE MATERIALIZED VIEW IF NOT EXISTS checks_1h_mv TO checks_1h AS
SELECT tenant_id, monitor_id, toStartOfHour(ts) AS hour, region,
       quantileState(0.5)(total_ms)  AS total_ms_state,
       quantileState(0.95)(total_ms) AS total_ms_p95,
       sum(ok)  AS ok_count_state,
       count()  AS total_count_state
FROM checks GROUP BY tenant_id, monitor_id, hour, region;
