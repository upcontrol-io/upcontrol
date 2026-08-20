-- upcontrol ClickHouse schema (plan §4.2). Pure SQL — no goose annotations.
-- ClickHouse's initdb runs this on first boot; `ucapi migrate` re-applies it
-- idempotently on subsequent boots (CREATE TABLE IF NOT EXISTS).
-- logs is the ring-displaced table; events are never displaced (the absence
-- detector lives on them); checks feed the availability detector and the public
-- status page. Rollup tables (series_1m, checks_1m, baselines) are the
-- aggregates the detectors and the dashboards read — invariant 2.

CREATE TABLE IF NOT EXISTS logs (
  tenant_id UInt64, project_id UInt64,
  ts DateTime64(3,'UTC') CODEC(DoubleDelta, ZSTD(1)),
  seq UInt64 CODEC(Delta, ZSTD(1)),
  source LowCardinality(String), service LowCardinality(String),
  host LowCardinality(String), level LowCardinality(String),
  message String CODEC(ZSTD(3)),
  fingerprint UInt64,
  attrs Map(LowCardinality(String), String),
  bucket UInt8 MATERIALIZED sipHash64(tenant_id) % 8
) ENGINE = MergeTree
PARTITION BY (bucket, toYYYYMMDD(ts))
ORDER BY (tenant_id, project_id, seq)
TTL toDateTime(ts) + INTERVAL 31 DAY;

ALTER TABLE logs ADD INDEX IF NOT EXISTS idx_ts    ts          TYPE minmax               GRANULARITY 4;
ALTER TABLE logs ADD INDEX IF NOT EXISTS idx_level level       TYPE set(16)              GRANULARITY 4;
ALTER TABLE logs ADD INDEX IF NOT EXISTS idx_fp    fingerprint TYPE bloom_filter(0.01)   GRANULARITY 4;
ALTER TABLE logs ADD INDEX IF NOT EXISTS idx_msg   message     TYPE tokenbf_v1(32768,3,0) GRANULARITY 4;

CREATE TABLE IF NOT EXISTS events (
  tenant_id UInt64, project_id UInt64,
  ts DateTime64(3,'UTC'), name LowCardinality(String),
  labels Map(LowCardinality(String), String),
  amount_minor Int64 DEFAULT 0, currency LowCardinality(String) DEFAULT ''
) ENGINE = MergeTree PARTITION BY toYYYYMM(ts) ORDER BY (tenant_id, name, ts);

CREATE TABLE IF NOT EXISTS checks (
  tenant_id UInt64, monitor_id UInt64, ts DateTime64(3,'UTC'),
  region LowCardinality(String), ok UInt8, status_code UInt16,
  error_class LowCardinality(String),
  dns_ms UInt32, connect_ms UInt32, tls_ms UInt32, ttfb_ms UInt32, total_ms UInt32,
  body_hash UInt64
) ENGINE = MergeTree PARTITION BY toYYYYMMDD(ts) ORDER BY (tenant_id, monitor_id, ts)
TTL toDateTime(ts) + INTERVAL 7 DAY;

CREATE TABLE IF NOT EXISTS series_1m (
  tenant_id UInt64, project_id UInt64, minute DateTime,
  source LowCardinality(String), level LowCardinality(String),
  lines UInt64, bytes UInt64
) ENGINE = SummingMergeTree ORDER BY (tenant_id, project_id, minute, source, level)
TTL minute + INTERVAL 90 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS series_1m_mv TO series_1m AS
SELECT tenant_id, project_id, toStartOfMinute(ts) AS minute,
       source, level, count() AS lines, sum(length(message)) AS bytes
FROM logs GROUP BY tenant_id, project_id, minute, source, level;

CREATE TABLE IF NOT EXISTS checks_1m (
  tenant_id UInt64, monitor_id UInt64, minute DateTime,
  region LowCardinality(String),
  total_ms_state AggregateFunction(quantile(0.5), UInt32),
  total_ms_p95   AggregateFunction(quantile(0.95), UInt32),
  total_ms_p99   AggregateFunction(quantile(0.99), UInt32),
  ok_count_state SimpleAggregateFunction(sum, UInt64),
  total_count_state SimpleAggregateFunction(sum, UInt64)
) ENGINE = AggregatingMergeTree PARTITION BY toYYYYMM(minute)
ORDER BY (tenant_id, monitor_id, minute, region)
TTL minute + INTERVAL 365 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS checks_1m_mv TO checks_1m AS
SELECT tenant_id, monitor_id, toStartOfMinute(ts) AS minute, region,
       quantileState(0.5)(total_ms)  AS total_ms_state,
       quantileState(0.95)(total_ms) AS total_ms_p95,
       quantileState(0.99)(total_ms) AS total_ms_p99,
       sum(ok)  AS ok_count_state,
       count()  AS total_count_state
FROM checks GROUP BY tenant_id, monitor_id, minute, region;

CREATE TABLE IF NOT EXISTS series_1h (
  tenant_id UInt64, project_id UInt64, hour DateTime,
  source LowCardinality(String), level LowCardinality(String),
  lines UInt64, bytes UInt64
) ENGINE = SummingMergeTree ORDER BY (tenant_id, project_id, hour, source, level)
TTL hour + INTERVAL 730 DAY;

CREATE TABLE IF NOT EXISTS checks_1h (
  tenant_id UInt64, monitor_id UInt64, hour DateTime,
  region LowCardinality(String),
  total_ms_state AggregateFunction(quantile(0.5), UInt32),
  total_ms_p95   AggregateFunction(quantile(0.95), UInt32),
  ok_count_state SimpleAggregateFunction(sum, UInt64),
  total_count_state SimpleAggregateFunction(sum, UInt64)
) ENGINE = AggregatingMergeTree PARTITION BY toYYYYMM(hour)
ORDER BY (tenant_id, monitor_id, hour, region)
TTL hour + INTERVAL 730 DAY;

CREATE TABLE IF NOT EXISTS baselines (
  tenant_id UInt64, series_key String, hour_of_week UInt8,
  median Int16, mad Int16, scale Float32, samples UInt32, updated_at DateTime
) ENGINE = ReplacingMergeTree(updated_at) ORDER BY (tenant_id, series_key, hour_of_week);

CREATE TABLE IF NOT EXISTS metrics (
  tenant_id UInt64, project_id UInt64, ts DateTime64(3,'UTC'),
  name LowCardinality(String),
  labels Map(LowCardinality(String), String),
  value Float64
) ENGINE = MergeTree PARTITION BY toYYYYMMDD(ts)
ORDER BY (tenant_id, project_id, name, ts)
TTL toDateTime(ts) + INTERVAL 31 DAY;

CREATE TABLE IF NOT EXISTS metrics_5m (
  tenant_id UInt64, project_id UInt64, bucket DateTime,
  name LowCardinality(String),
  value_state AggregateFunction(avg, Float64)
) ENGINE = AggregatingMergeTree PARTITION BY toYYYYMM(bucket)
ORDER BY (tenant_id, project_id, name, bucket)
TTL bucket + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS metrics_1h (
  tenant_id UInt64, project_id UInt64, bucket DateTime,
  name LowCardinality(String),
  value_state AggregateFunction(avg, Float64)
) ENGINE = AggregatingMergeTree PARTITION BY toYYYYMM(bucket)
ORDER BY (tenant_id, project_id, name, bucket)
TTL bucket + INTERVAL 730 DAY;
