-- +goose Up
-- +goose StatementBegin
-- upcontrol Postgres schema, relational half + telemetry, in one migration.
-- This replaces migrations 001-028 wholesale: nobody has data, so every ALTER
-- that patched an earlier statement is folded into the CREATE it patched and
-- every data-only repair is gone (there is no data to repair).
-- Types: bigint for internal keys, uuid v7 for anything shown outside,
-- timestamptz always, text over varchar. Every tenant-scoped table carries
-- tenant_id (invariant 3); the exempt reference tables (plan_entitlement,
-- probe_node, webhook_seen, person, session) are listed in the invariant-3
-- test, not here.

CREATE TABLE tenant (
  id            bigserial PRIMARY KEY,
  public_id     uuid NOT NULL UNIQUE,
  name          text NOT NULL,
  plan          text NOT NULL DEFAULT 'Free',       -- Free|Indie|Growth|Agency|Self-hosted
  billing       text NOT NULL DEFAULT 'annual',     -- monthly|annual
  claim_token_hash bytea,                           -- sha256 of the anonymous-init claim token
  claimed_at    timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);
-- Claim tokens are one-time: claimed tenants keep the NULL, and NULLs must
-- not collide with each other, hence the partial index.
CREATE UNIQUE INDEX tenant_claim_token_key ON tenant (claim_token_hash)
  WHERE claim_token_hash IS NOT NULL;

CREATE TABLE person (
  id            bigserial PRIMARY KEY,
  public_id     uuid NOT NULL UNIQUE,
  email         text UNIQUE,
  email_verified_at timestamptz,
  telegram_id   bigint UNIQUE,
  google_sub    text UNIQUE,
  name          text NOT NULL DEFAULT '',
  telegram_username text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CHECK (email IS NOT NULL OR telegram_id IS NOT NULL)
);

CREATE TABLE project (
  id            bigserial PRIMARY KEY,
  public_id     uuid NOT NULL UNIQUE,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  domain        text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tenant_member (
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  person_id     bigint NOT NULL REFERENCES person(id) ON DELETE CASCADE,
  role          text NOT NULL,                       -- notify|login
  status        text NOT NULL DEFAULT 'pending',     -- pending|active
  PRIMARY KEY (tenant_id, person_id)
);

CREATE TABLE session (
  id            bigserial PRIMARY KEY,
  token_hash    bytea NOT NULL UNIQUE,               -- sha256 of cookie value
  person_id     bigint NOT NULL REFERENCES person(id) ON DELETE CASCADE,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id    bigint REFERENCES project(id) ON DELETE SET NULL, -- NULL = tenant's lowest id at read time
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL
);
CREATE INDEX ON session (expires_at);

CREATE TABLE api_key (
  id            bigserial PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  prefix        text NOT NULL,                       -- uc_live_8f2ac41d, shown
  secret_hash   bytea NOT NULL,
  state         text NOT NULL DEFAULT 'active',      -- active|rotating|revoked
  rotating_until timestamptz,                        -- 24h overlap window
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_used_at  timestamptz
);
CREATE UNIQUE INDEX ON api_key (prefix);

CREATE TABLE key_usage_log (
  id            bigserial PRIMARY KEY,
  tenant_id     bigint NOT NULL,
  key_id        bigint NOT NULL REFERENCES api_key(id) ON DELETE CASCADE,
  at            timestamptz NOT NULL DEFAULT now(),
  source        text NOT NULL,                       -- sdk|curl|otlp|...
  outcome       text NOT NULL                        -- accepted|rejected
);

CREATE TABLE project_seq (
  project_id    bigint PRIMARY KEY REFERENCES project(id) ON DELETE CASCADE,
  next          bigint NOT NULL DEFAULT 1
);

CREATE TABLE tenant_line_ledger (
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  bucket_5m     timestamptz NOT NULL,
  rows          bigint NOT NULL,
  min_seq       bigint NOT NULL,
  max_seq       bigint NOT NULL,
  PRIMARY KEY (project_id, bucket_5m)
);

CREATE TABLE project_window (
  project_id    bigint PRIMARY KEY REFERENCES project(id) ON DELETE CASCADE,
  cutoff_seq    bigint NOT NULL,
  retain_seq    bigint NOT NULL,
  window_hours  numeric NOT NULL,        -- actual depth, for the screen
  beyond_errors bigint,                  -- NULL = stay silent (zero-is-silence)
  computed_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ingest_batch (
  batch_key     text PRIMARY KEY,
  body_hash     bytea NOT NULL,
  accepted      int NOT NULL DEFAULT 0,  -- replay must return the same count
  accepted_at   timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL
);
CREATE INDEX ON ingest_batch (expires_at);

CREATE TABLE monitor (
  id            bigserial PRIMARY KEY,
  public_id     uuid NOT NULL UNIQUE,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  kind          text NOT NULL,                       -- website|heartbeat
  name          text NOT NULL,
  target        text NOT NULL,
  keyword       text,                                -- body assertion
  interval_sec  int NOT NULL,                        -- 60|300|1800|3600
  availability_target numeric NOT NULL DEFAULT 99.9,
  paused        bool NOT NULL DEFAULT false,
  ping_token    text UNIQUE,                         -- heartbeat only
  grace_sec     int,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE monitor_schedule (
  monitor_id    bigint PRIMARY KEY REFERENCES monitor(id) ON DELETE CASCADE,
  region        text NOT NULL,
  next_due_at   timestamptz NOT NULL,
  leased_by     text,
  lease_until   timestamptz
);
CREATE INDEX ON monitor_schedule (next_due_at) WHERE leased_by IS NULL;

CREATE TABLE monitor_facts (
  monitor_id    bigint PRIMARY KEY REFERENCES monitor(id) ON DELETE CASCADE,
  status        text NOT NULL DEFAULT 'nodata',      -- ok|check|down|nodata
  ssl_expires_at timestamptz,
  domain_expires_at timestamptz,
  last_check_at timestamptz,
  consecutive_failures int NOT NULL DEFAULT 0
);

CREATE TABLE probe_node (
  id            text PRIMARY KEY,
  region        text NOT NULL,
  last_seen_at  timestamptz NOT NULL,
  blind_since   timestamptz
);

CREATE TABLE incident (
  id            bigserial PRIMARY KEY,
  public_id     uuid NOT NULL UNIQUE,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  monitor_id    bigint REFERENCES monitor(id) ON DELETE SET NULL,
  detector      text NOT NULL,
  fingerprint   bigint NOT NULL,
  title         text NOT NULL,
  status        text NOT NULL,                        -- down|check|ok
  detected_at   timestamptz NOT NULL,
  notified_at   timestamptz,
  acked_at      timestamptz,
  acked_by      bigint REFERENCES person(id),
  resolved_at   timestamptz,
  close_reason  text,                                 -- recovered|maintenance|monitor_deleted|by_human|absorbed|detector_off
  affected_count int NOT NULL DEFAULT 0,
  deploy_id     bigint,                               -- joined once at open
  slice_phase   smallint NOT NULL DEFAULT 0,          -- 0 none, 1 left, 2 full
  slice_done_at timestamptz
);
CREATE INDEX ON incident (tenant_id, detected_at DESC);
CREATE INDEX ON incident (fingerprint, resolved_at);

CREATE TABLE incident_slice (
  incident_id   bigint NOT NULL REFERENCES incident(id) ON DELETE CASCADE,
  seq           bigint NOT NULL,
  ts            timestamptz NOT NULL,
  level         text NOT NULL,
  service       text NOT NULL DEFAULT '',
  message       text NOT NULL,
  PRIMARY KEY (incident_id, seq)
);

CREATE TABLE incident_update (
  id            bigserial PRIMARY KEY,
  incident_id   bigint NOT NULL REFERENCES incident(id) ON DELETE CASCADE,
  at            timestamptz NOT NULL DEFAULT now(),
  kind          text NOT NULL,                        -- opened|escalated|acked|resolved|note
  text          text NOT NULL
);

CREATE TABLE alert_channel (
  id            bigserial PRIMARY KEY,
  public_id     uuid NOT NULL UNIQUE,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  kind          text NOT NULL,                        -- telegram|email|discord|slack
  target        text NOT NULL,
  label         text,                                 -- display name for personal destinations
  recipient_person_id bigint REFERENCES person(id),   -- NULL = broadcast group
  muted_until   timestamptz,
  notify        jsonb NOT NULL DEFAULT '{}'::jsonb,   -- sparse: absent key = class default
  secret_enc    bytea,
  breaker_open_until timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE delivery_queue (
  id            bigserial PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  incident_id   bigint REFERENCES incident(id) ON DELETE CASCADE,
  channel_id    bigint NOT NULL REFERENCES alert_channel(id) ON DELETE CASCADE,
  idem_key      text NOT NULL UNIQUE,                 -- digest(recipient, incident, update_seq)
  class         text NOT NULL,                        -- page|ticket|digest|test
  payload       jsonb NOT NULL,
  attempts      int NOT NULL DEFAULT 0,
  next_try_at   timestamptz NOT NULL DEFAULT now(),
  state         text NOT NULL DEFAULT 'pending',      -- pending|sent|dead
  dead_reason   text,
  leased_by     text,
  lease_until   timestamptz
);
CREATE INDEX ON delivery_queue (next_try_at) WHERE state = 'pending' AND leased_by IS NULL;

CREATE TABLE delivery_attempt (
  id            bigserial PRIMARY KEY,
  queue_id      bigint NOT NULL REFERENCES delivery_queue(id) ON DELETE CASCADE,
  at            timestamptz NOT NULL DEFAULT now(),
  outcome       text NOT NULL,                        -- ok|retriable|fatal
  detail        text NOT NULL DEFAULT ''
);

CREATE TABLE source_connection (
  id            bigserial PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  kind          text NOT NULL,                        -- stripe|github|vercel|agent|site
  external_id   text,
  token_enc     bytea,
  hook_token    text,                                 -- the URL credential, 128 bits per connection
  last_event    text,                                 -- the hook panel's receipt, overwritten per event
  status        text NOT NULL DEFAULT 'ok',
  last_signal_at timestamptz,
  paused        bool NOT NULL DEFAULT false
);
-- One connection per kind, per project: there is exactly one hook URL per
-- provider, so a second row could never mean anything different from the first.
CREATE UNIQUE INDEX source_connection_project_kind_key
  ON source_connection (project_id, kind);
CREATE UNIQUE INDEX source_connection_hook_token_key
  ON source_connection (hook_token);

CREATE TABLE webhook_seen (
  provider      text NOT NULL,
  event_id      text NOT NULL,
  seen_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, event_id)
);

-- One outstanding magic-link code per email: sha256(code), single-use, capped
-- attempts, TTL. A new request overwrites the row — the latest code wins.
CREATE TABLE magic_link_code (
  email         text PRIMARY KEY,
  code_hash     bytea NOT NULL,
  attempts      int NOT NULL DEFAULT 0,
  expires_at    timestamptz NOT NULL,
  redeemed_at   timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);

-- Sliding-window throttle for the anonymous magic-link endpoint, keyed by IP:
-- the endpoint sends mail, so it is rate-limited per IP as well as per email.
CREATE TABLE magic_link_ip (
  ip            text PRIMARY KEY,
  first_at      timestamptz NOT NULL DEFAULT now(),
  count         int NOT NULL DEFAULT 1
);

CREATE TABLE status_page (
  id            bigserial PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  slug          text NOT NULL UNIQUE,                 -- token until ownership proven
  domain        text UNIQUE,
  domain_verified_at timestamptz,
  title         text NOT NULL DEFAULT '',
  components    jsonb NOT NULL DEFAULT '[]',
  config        jsonb NOT NULL DEFAULT '{}'::jsonb    -- what is shown, vs components = the published list
);

CREATE TABLE plan_entitlement (
  plan          text PRIMARY KEY,
  http_checks   int NOT NULL,
  regions       int NOT NULL,
  window_lines  bigint NOT NULL,
  window_hours  int NOT NULL,
  retain_mult   numeric NOT NULL,      -- R
  incident_days int NOT NULL,
  min_interval_sec int NOT NULL DEFAULT 60,
  telegram_recipients int NOT NULL DEFAULT 3,
  projects      int                    -- NULL = unlimited (Self-hosted)
);

INSERT INTO plan_entitlement
  (plan, http_checks, regions, window_lines, window_hours, retain_mult,
   incident_days, min_interval_sec, telegram_recipients, projects)
VALUES
  ('Free',          3, 1,    25000,  24, 2.0,  30, 300,   3,    1),
  ('Indie',        10, 2,   150000,  48, 1.0, 365,  60,  10,    2),
  ('Growth',       30, 2,  3000000, 168, 1.0, 730,  60,  30,    5),
  ('Agency',      100, 2, 45000000, 720, 1.0, 730,  60, 100,   10),
  ('Self-hosted',1000,10, 45000000, 720, 1.0, 730,  60,1000, NULL);

-- The error-log scanner's memory of what it already alerted: without it a
-- persisting error would page again on every 60s scan. kind is which category
-- fired ('error'|'repeat') — the two have different cooldowns.
CREATE TABLE error_alert_state (
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  fingerprint   bigint NOT NULL,
  kind          text NOT NULL,
  last_alerted  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, fingerprint, kind)
);

-- One-time install tokens: the dashboard's install card generates
-- `npx upcontrol init --token uct_...` so a signed-in user's CLI lands the key
-- of THEIR project instead of minting an anonymous one. Stored as sha256 like
-- magic-link codes; single-use via used_at, short TTL.
CREATE TABLE install_token (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  token_hash    bytea NOT NULL UNIQUE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  used_at       timestamptz
);

-- One-time Telegram invite tokens: the deep link t.me/<bot>?start=inv_<token>
-- is the only way to link a Telegram account to a person (the old prj-N link
-- was guessable). The raw token appears exactly once in the POST response.
CREATE TABLE telegram_invite (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  role          text NOT NULL,                    -- notify|login|owner
  invited_by    bigint NOT NULL REFERENCES person(id) ON DELETE CASCADE,
  person_id     bigint REFERENCES person(id) ON DELETE CASCADE, -- set at redeem
  token_hash    bytea NOT NULL UNIQUE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  redeemed_at   timestamptz
);

-- LemonSqueezy billing state, one row per tenant. The webhook is the only
-- writer: tenant.plan / tenant.billing are derived from it on every
-- subscription event, so the entitlement gates read the same facts the money
-- side wrote (measured, not asserted — a plan a webhook never confirmed is Free).
CREATE TABLE billing_subscription (
  tenant_id          bigint PRIMARY KEY REFERENCES tenant(id) ON DELETE CASCADE,
  ls_customer_id     bigint NOT NULL,
  ls_subscription_id bigint NOT NULL UNIQUE,
  variant_id         bigint NOT NULL,
  status             text NOT NULL,              -- LS status verbatim: on_trial|active|paused|past_due|unpaid|cancelled|expired
  renews_at          timestamptz,
  ends_at            timestamptz,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON billing_subscription (ls_customer_id);

-- The Postgres half of product analytics: a directory of current visitor
-- state — first-touch attribution, identity, counters. web_events below is the
-- raw stream this summarizes. token_hash is sha256(uc_vid cookie): the raw
-- cookie never reaches the database.
CREATE TABLE web_visitor (
  id                 bigserial PRIMARY KEY,
  token_hash         bytea NOT NULL UNIQUE,
  first_seen_at      timestamptz NOT NULL DEFAULT now(),
  last_seen_at       timestamptz NOT NULL DEFAULT now(),
  first_referrer     text NOT NULL DEFAULT '',
  first_utm_source   text NOT NULL DEFAULT '',
  first_utm_medium   text NOT NULL DEFAULT '',
  first_utm_campaign text NOT NULL DEFAULT '',
  first_country      text NOT NULL DEFAULT '',
  first_device       text NOT NULL DEFAULT '',
  first_path         text NOT NULL DEFAULT '',
  last_country       text NOT NULL DEFAULT '',
  last_device        text NOT NULL DEFAULT '',
  email              text NOT NULL DEFAULT '',
  person_id          bigint REFERENCES person(id) ON DELETE SET NULL,
  tenant_id          bigint,
  signed_in_at       timestamptz,
  account_created_at timestamptz,
  events_count       bigint NOT NULL DEFAULT 0,
  is_bot             boolean NOT NULL DEFAULT false
);
-- Partial indexes: anonymous rows that neither lookup path ever returns do
-- not need index entries.
CREATE INDEX web_visitor_person_idx ON web_visitor (person_id) WHERE person_id IS NOT NULL;
CREATE INDEX web_visitor_email_idx ON web_visitor (email) WHERE email <> '';

CREATE TABLE instance_setting (
  key           text PRIMARY KEY,
  value_enc     bytea NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Telemetry, ported from db/clickhouse: the ring-displaced log table, the
-- never-displaced event table, check history, metrics, web analytics, and the
-- per-minute counts the detector's baseline reads. The ClickHouse rollups
-- (checks_1m, series_1h, checks_1h, metrics_5m, metrics_1h, baselines) are
-- deliberately NOT ported: they are re-derived on demand or maintained at
-- ingest — porting materialized views as triggers is the complexity this
-- collapse exists to remove. No TTLs either: retention becomes partition drops.

-- logs is the ring table. Range-partitioned on ts so retention is a partition
-- drop; the PK carries every column the ring orders by, and the partition key
-- must be part of it. uint64 hashes (fingerprint, body_hash) are stored as
-- signed bigint: the Go writer casts with int64(v), and a value above
-- MaxInt64 wraps — the hash is an opaque identity, never compared for order.
CREATE TABLE logs (
  tenant_id     bigint,
  project_id    bigint,
  ts            timestamptz,
  seq           bigint,
  source        text,
  service       text,
  host          text,
  level         text,
  level_raw     text,                                -- the client's own spelling, capped at 32 bytes
  message       text,
  fingerprint   bigint,
  attrs         jsonb,
  PRIMARY KEY (tenant_id, project_id, seq, ts)
) PARTITION BY RANGE (ts);
CREATE INDEX ON logs (tenant_id, project_id, seq);        -- the ring window
CREATE INDEX ON logs (tenant_id, project_id, ts);         -- range reads
CREATE INDEX ON logs (tenant_id, project_id, fingerprint); -- ErrorGroups
CREATE INDEX ON logs USING gin (attrs);                   -- bounded by the ingest attribute cap
-- Today and tomorrow only: a fresh install can write immediately, and the
-- job that rolls partitions forward from here lands with the swap-in. The
-- bounds are install-time dates, so the DDL is built in a DO block.
DO $$
DECLARE
  d timestamptz := date_trunc('day', now());
BEGIN
  EXECUTE format('CREATE TABLE logs_today PARTITION OF logs FOR VALUES FROM (%L) TO (%L)',
                 d, d + interval '1 day');
  EXECUTE format('CREATE TABLE logs_tomorrow PARTITION OF logs FOR VALUES FROM (%L) TO (%L)',
                 d + interval '1 day', d + interval '2 days');
END $$;

-- events are never displaced by the ring; the absence detector lives on them.
CREATE TABLE events (
  tenant_id     bigint,
  project_id    bigint,
  ts            timestamptz,
  name          text,
  labels        jsonb,
  amount_minor  bigint,
  currency      text
);
CREATE INDEX ON events (tenant_id, project_id, ts);
CREATE INDEX ON events (tenant_id, name);             -- LastDeployAt filters on name with a LIKE

-- checks: the availability detector's history and the public status page.
CREATE TABLE checks (
  tenant_id     bigint,
  monitor_id    bigint,
  ts            timestamptz,
  region        text,
  ok            boolean,
  status_code   int,
  error_class   text,
  dns_ms        int,
  connect_ms    int,
  tls_ms        int,
  ttfb_ms       int,
  total_ms      int,
  body_hash     bigint
);
CREATE INDEX ON checks (tenant_id, monitor_id, ts);

CREATE TABLE metrics (
  tenant_id     bigint,
  project_id    bigint,
  ts            timestamptz,
  name          text,
  labels        jsonb,
  value         double precision
);
CREATE INDEX ON metrics (tenant_id, project_id, name, ts);

-- web_events: the raw visitor event stream (web_visitor above is the
-- directory). ip_hash is the first 8 bytes of sha256(client IP) — a full IP is
-- never stored. Bots are stored with device='bot' and excluded at query time.
CREATE TABLE web_events (
  visitor_id    bigint,
  person_id     bigint,
  tenant_id     bigint,
  ts            timestamptz,
  name          text,
  path          text,
  title         text,
  referrer      text,
  utm_source    text,
  utm_medium    text,
  utm_campaign  text,
  country       text,
  ip_hash       bytea,
  device        text,
  os            text,
  browser       text,
  props         jsonb
);
CREATE INDEX ON web_events (ts);
CREATE INDEX ON web_events (tenant_id, name, ts);     -- the admin dashboard filters exactly this way

-- series_1m replaces the series_1m_mv materialized view: maintained by an
-- UPSERT at ingest (pgstore.BumpSeries), so the composite primary key is what
-- makes ON CONFLICT work. The detector's median/MAD reads this.
CREATE TABLE series_1m (
  tenant_id     bigint,
  project_id    bigint,
  minute        timestamptz,
  source        text,
  level         text,
  lines         bigint,
  bytes         bigint,
  PRIMARY KEY (tenant_id, project_id, minute, source, level)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS series_1m;
DROP TABLE IF EXISTS web_events;
DROP TABLE IF EXISTS metrics;
DROP TABLE IF EXISTS checks;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS logs;
DROP TABLE IF EXISTS instance_setting;
DROP TABLE IF EXISTS web_visitor;
DROP TABLE IF EXISTS billing_subscription;
DROP TABLE IF EXISTS telegram_invite;
DROP TABLE IF EXISTS install_token;
DROP TABLE IF EXISTS error_alert_state;
DROP TABLE IF EXISTS plan_entitlement;
DROP TABLE IF EXISTS status_page;
DROP TABLE IF EXISTS magic_link_ip;
DROP TABLE IF EXISTS magic_link_code;
DROP TABLE IF EXISTS webhook_seen;
DROP TABLE IF EXISTS source_connection;
DROP TABLE IF EXISTS delivery_attempt;
DROP TABLE IF EXISTS delivery_queue;
DROP TABLE IF EXISTS alert_channel;
DROP TABLE IF EXISTS incident_update;
DROP TABLE IF EXISTS incident_slice;
DROP TABLE IF EXISTS incident;
DROP TABLE IF EXISTS probe_node;
DROP TABLE IF EXISTS monitor_facts;
DROP TABLE IF EXISTS monitor_schedule;
DROP TABLE IF EXISTS monitor;
DROP TABLE IF EXISTS ingest_batch;
DROP TABLE IF EXISTS project_window;
DROP TABLE IF EXISTS tenant_line_ledger;
DROP TABLE IF EXISTS project_seq;
DROP TABLE IF EXISTS key_usage_log;
DROP TABLE IF EXISTS api_key;
DROP TABLE IF EXISTS session;
DROP TABLE IF EXISTS tenant_member;
DROP TABLE IF EXISTS project;
DROP TABLE IF EXISTS person;
DROP TABLE IF EXISTS tenant;
-- +goose StatementEnd
