-- +goose Up
-- +goose StatementBegin
-- upcontrol Postgres schema (plan §4.1). Types: bigint for internal keys,
-- uuid v7 for anything shown outside, timestamptz always, text over varchar.
-- Every tenant-scoped table carries tenant_id (invariant 3); the exempt
-- reference tables (plan_entitlement, probe_node, webhook_seen, person, session)
-- are listed in the invariant-3 test, not here.

CREATE TABLE tenant (
  id            bigserial PRIMARY KEY,
  public_id     uuid NOT NULL UNIQUE,
  name          text NOT NULL,
  plan          text NOT NULL DEFAULT 'Free',       -- Free|Indie|Growth|Agency
  billing       text NOT NULL DEFAULT 'annual',     -- monthly|annual
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE person (
  id            bigserial PRIMARY KEY,
  public_id     uuid NOT NULL UNIQUE,
  email         text UNIQUE,
  email_verified_at timestamptz,
  telegram_id   bigint UNIQUE,
  google_sub    text UNIQUE,
  name          text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now(),
  CHECK (email IS NOT NULL OR telegram_id IS NOT NULL)
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
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL
);
CREATE INDEX ON session (expires_at);

CREATE TABLE project (
  id            bigserial PRIMARY KEY,
  public_id     uuid NOT NULL UNIQUE,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  domain        text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now()
);

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
  status        text NOT NULL DEFAULT 'ok',
  last_signal_at timestamptz,
  paused        bool NOT NULL DEFAULT false
);

CREATE TABLE webhook_seen (
  provider      text NOT NULL,
  event_id      text NOT NULL,
  seen_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, event_id)
);

CREATE TABLE ai_usage (
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  month         date NOT NULL,
  used          int NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, month)
);

CREATE TABLE ai_explain_cache (
  tenant_id     bigint NOT NULL,
  input_hash    bytea NOT NULL,
  text          text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, input_hash)
);

CREATE TABLE status_page (
  id            bigserial PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  slug          text NOT NULL UNIQUE,                 -- token until ownership proven
  domain        text UNIQUE,
  domain_verified_at timestamptz,
  title         text NOT NULL DEFAULT '',
  components    jsonb NOT NULL DEFAULT '[]'
);

CREATE TABLE plan_entitlement (
  plan          text PRIMARY KEY,
  http_checks   int NOT NULL,
  regions       int NOT NULL,
  window_lines  bigint NOT NULL,
  window_hours  int NOT NULL,
  retain_mult   numeric NOT NULL,      -- R
  ai_explains   int,                   -- NULL = unlimited
  incident_days int NOT NULL
);

INSERT INTO plan_entitlement VALUES
  ('Free',    3,1,    25000,  24, 2.0,   5,   30),
  ('Indie',  10,2,   150000,  48, 1.0, NULL, 365),
  ('Growth', 30,2,  3000000, 168, 1.0, NULL, 730),
  ('Agency',100,2, 45000000, 720, 1.0, NULL, 730);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS plan_entitlement;
DROP TABLE IF EXISTS status_page;
DROP TABLE IF EXISTS ai_explain_cache;
DROP TABLE IF EXISTS ai_usage;
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
DROP TABLE IF EXISTS project;
DROP TABLE IF EXISTS session;
DROP TABLE IF EXISTS tenant_member;
DROP TABLE IF EXISTS person;
DROP TABLE IF EXISTS tenant;
-- +goose StatementEnd
