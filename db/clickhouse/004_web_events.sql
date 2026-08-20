-- 004_web_events.sql — first-party product analytics (plan: product-analytics
-- §Decision 6): the raw append-only visitor event stream written by the
-- ucapi Recorder (internal/analytics). web_visitor in Postgres is the
-- directory of current state; this table is the history the dashboard
-- queries. visitor_id is the Postgres web_visitor bigserial (0 = anonymous:
-- no uc_vid cookie on the request). country is an ISO code ('' = unknown);
-- ip_hash is the first 8 bytes of sha256(client IP) for grouping — a full IP
-- is never stored. Bots (hand-classified UA) are stored with device='bot' and
-- excluded from dashboard metrics at query time.
-- Never edit 001 — add the next file.

CREATE TABLE IF NOT EXISTS web_events (
  visitor_id UInt64,
  person_id UInt64,
  tenant_id UInt64,
  ts DateTime64(3,'UTC') CODEC(DoubleDelta, ZSTD(1)),
  name LowCardinality(String),
  path String,
  title String,
  referrer String,
  utm_source LowCardinality(String),
  utm_medium LowCardinality(String),
  utm_campaign LowCardinality(String),
  country LowCardinality(String),
  ip_hash FixedString(8),
  device LowCardinality(String),
  os LowCardinality(String),
  browser LowCardinality(String),
  is_app UInt8 MATERIALIZED startsWith(path, '/app'),
  props Map(LowCardinality(String), String)
) ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (ts, visitor_id)
TTL toDateTime(ts) + INTERVAL 730 DAY;
