CREATE TABLE tenant (
  id            bigserial PRIMARY KEY,
  public_id     uuid NOT NULL UNIQUE,
  name          text NOT NULL,
  plan          text NOT NULL DEFAULT 'Free',       -- Free|Indie|Growth|Agency|Self-hosted
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

-- Sequence allocation is live and load bearing; the ring-retention ledger and
-- window tables that once sat next to it were inert end to end and are gone.
CREATE TABLE project_seq (
  project_id    bigint PRIMARY KEY REFERENCES project(id) ON DELETE CASCADE,
  next          bigint NOT NULL DEFAULT 1
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

-- Webhook dedup for every provider that delivers more than once; keyed by the
-- provider's own event id.
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

-- The plan ladder's numbers: every gate and every figure on the Plan screen
-- reads this row, never a constant. telegram_recipients counts destinations
-- (people, groups, channels alike); telegram_rooms is whether groups and
-- channels may connect at all; custom_domain is whether a status page may sit
-- on the customer's own host; incident_days is how long closed incidents stay
-- readable. Self-hosted is the OSS install's one generous row, never sold.
CREATE TABLE plan_entitlement (
  plan          text PRIMARY KEY,
  http_checks   int NOT NULL,
  window_lines  bigint NOT NULL,
  window_hours  int NOT NULL,
  incident_days int NOT NULL,
  min_interval_sec int NOT NULL DEFAULT 60,
  telegram_recipients int NOT NULL DEFAULT 3,
  projects      int,                   -- NULL = unlimited (Self-hosted)
  custom_domain boolean NOT NULL DEFAULT false,
  telegram_rooms boolean NOT NULL DEFAULT true
);

INSERT INTO plan_entitlement
  (plan, http_checks, window_lines, window_hours, incident_days,
   min_interval_sec, telegram_recipients, projects, custom_domain, telegram_rooms)
VALUES
  ('Free',          3,    25000,  24,   10, 300,    3,    1, false, false),
  ('Indie',        10,   150000,  48,  365,  60,   10,    2, true,  true),
  ('Growth',       30,  3000000, 168,  730,  60,   20,    5, true,  true),
  ('Agency',      100, 45000000, 720, 1460,  60,   30,   10, true,  true),
  ('Self-hosted',1000, 45000000, 720,  730,  60, 1000, NULL, true,  true);

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
-- per-minute counts the detector's baseline reads. The six rollups
-- (checks_1m, series_1h, checks_1h, metrics_5m, metrics_1h, baselines) are
-- dropped, not deferred: a survey found them read by nothing in core, cloud
-- or admin, and the one rollup anything read (series_1m) is ported above as
-- the per-minute counts. Porting materialized views as triggers is the
-- complexity this collapse exists to remove. No TTLs either: retention
-- becomes partition drops.

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
-- Partitions are named after the UTC day they hold, which is what lets
-- ucworker's log-partitions job create ahead and drop behind. Today and the
-- next three days, so a fresh install can write immediately and the hourly
-- job has a full day of slack before the first one it must make.
DO $$
DECLARE
  d date := (now() AT TIME ZONE 'UTC')::date;
  i int;
BEGIN
  FOR i IN 0..3 LOOP
    EXECUTE format('CREATE TABLE IF NOT EXISTS %I PARTITION OF logs FOR VALUES FROM (%L) TO (%L)',
                   'logs_' || to_char(d + i, 'YYYYMMDD'),
                   (d + i)::timestamp AT TIME ZONE 'UTC',
                   (d + i + 1)::timestamp AT TIME ZONE 'UTC');
  END LOOP;
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

-- Teams and channels move from the workspace to the project: a workspace has
-- exactly one owner, everyone else is a member of the projects they were
-- invited to, and a channel, an invite and the scanner's memory all belong to
-- one project rather than to the whole account.

-- The owner is a column on the workspace, not a role in a membership table:
-- there is exactly one, it is never granted and never revoked.
ALTER TABLE tenant ADD COLUMN owner_person_id bigint REFERENCES person(id) ON DELETE SET NULL;
-- The sign-in door inserts the person and then the tenant, so the creator is
-- the newest person at or before the tenant's own creation; an older teammate
-- invited later sorts below. Unclaimed anonymous tenants keep NULL.
UPDATE tenant t SET owner_person_id = (
  SELECT tm.person_id FROM tenant_member tm JOIN person p ON p.id = tm.person_id
   WHERE tm.tenant_id = t.id AND tm.role = 'login' AND tm.status = 'active'
   ORDER BY (p.created_at <= t.created_at) DESC, p.created_at DESC, p.id
   LIMIT 1);
CREATE INDEX tenant_owner_idx ON tenant (owner_person_id) WHERE owner_person_id IS NOT NULL;

-- tenant_id rides along for the tenant-scoped-table invariant (invariant 3 in
-- 001_init.sql). The owner needs no row: ownership is tenant.owner_person_id.
CREATE TABLE project_member (
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  person_id     bigint NOT NULL REFERENCES person(id) ON DELETE CASCADE,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  role          text NOT NULL,                       -- notify|login
  status        text NOT NULL DEFAULT 'pending',     -- pending|active
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, person_id)
);
CREATE INDEX project_member_person_idx ON project_member (person_id);

-- A workspace member becomes a member of its oldest project only: the old row
-- said nothing about which project the person was meant to see.
INSERT INTO project_member (project_id, person_id, tenant_id, role, status)
SELECT (SELECT min(id) FROM project WHERE tenant_id = tm.tenant_id), tm.person_id, tm.tenant_id, tm.role, tm.status
  FROM tenant_member tm JOIN tenant t ON t.id = tm.tenant_id
 WHERE tm.person_id IS DISTINCT FROM t.owner_person_id
   AND EXISTS (SELECT 1 FROM project WHERE tenant_id = tm.tenant_id);
DROP TABLE tenant_member;

ALTER TABLE alert_channel ADD COLUMN project_id bigint REFERENCES project(id) ON DELETE CASCADE;
UPDATE alert_channel c SET project_id = (SELECT min(id) FROM project WHERE tenant_id = c.tenant_id);
DELETE FROM alert_channel WHERE project_id IS NULL;
ALTER TABLE alert_channel ALTER COLUMN project_id SET NOT NULL;
CREATE INDEX alert_channel_project_idx ON alert_channel (project_id);

ALTER TABLE telegram_invite ADD COLUMN project_id bigint REFERENCES project(id) ON DELETE CASCADE;
UPDATE telegram_invite i SET project_id = (SELECT min(id) FROM project WHERE tenant_id = i.tenant_id);
DELETE FROM telegram_invite WHERE project_id IS NULL;
ALTER TABLE telegram_invite ALTER COLUMN project_id SET NOT NULL;

-- The scanner's memory becomes per project: two projects of one workspace can
-- carry the same fingerprint, and one alerting must not silence the other.
ALTER TABLE error_alert_state ADD COLUMN project_id bigint REFERENCES project(id) ON DELETE CASCADE;
UPDATE error_alert_state s SET project_id = (SELECT min(id) FROM project WHERE tenant_id = s.tenant_id);
DELETE FROM error_alert_state WHERE project_id IS NULL;
ALTER TABLE error_alert_state ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE error_alert_state DROP CONSTRAINT error_alert_state_pkey;
ALTER TABLE error_alert_state ADD PRIMARY KEY (tenant_id, project_id, fingerprint, kind);

-- A project keeps a SET of keys, not one key it rotates. The table always
-- allowed it — the only unique index is on `prefix` — and the resolver already
-- looks a key up by prefix and honours `revoked`. What was missing is a way to
-- tell two keys apart and a record of when one was withdrawn.

-- Empty is a real name, not a placeholder: every key issued before this
-- migration has one, and the app prints the prefix when the name is blank.
ALTER TABLE api_key ADD COLUMN name text NOT NULL DEFAULT '';

-- When the key was withdrawn, kept beside `state = 'revoked'` rather than
-- instead of it: the state is what the resolver reads on every ingest batch,
-- and this is what a person reads when they are working out what leaked and
-- when. A revoked row is never deleted for the same reason — `last_used_at` on
-- a key someone withdrew is evidence.
ALTER TABLE api_key ADD COLUMN revoked_at timestamptz;

-- The list is read per project on every Connect page load.
CREATE INDEX IF NOT EXISTS api_key_project_state_idx ON api_key (project_id, state);

-- Who made this board, and what a key offered for it. Provenance exists
-- because "does a board exist?" was only ever an approximation of "did a
-- human curate this board?": a key that can replace a curated board is a
-- wipe waiting to leak, while a key that can replace its OWN board is not.
-- 'session' is the default on purpose — every board that exists today was
-- saved from a browser, and that is what the column must say about them.
-- `proposed` holds a replacement the key offered for a curated board: kept,
-- never applied, waiting for one click in the app.
ALTER TABLE dashboard
  ADD COLUMN written_by  text NOT NULL DEFAULT 'session',
  ADD COLUMN proposed    jsonb,
  ADD COLUMN proposed_at timestamptz;

-- Who did it. An event has always carried what happened and with what labels, but never a
-- person, so every people-shaped answer had to be computed on the client and shipped as a
-- finished reading. One column moves all four feeds onto the server and makes them reachable
-- from any language that can POST an event.
--
-- Empty string rather than NULL: "no actor" is a real answer — a server-side event with nobody
-- behind it — and it keeps the two readings honestly apart. count(*) is events;
-- count(DISTINCT actor) FILTER (WHERE actor <> '') is people.
ALTER TABLE events ADD COLUMN actor text NOT NULL DEFAULT '';

-- The funnel and breakdown reads: one event name over a window.
CREATE INDEX events_people_by_name ON events (tenant_id, project_id, name, ts) WHERE actor <> '';
-- The retention read: an actor's whole history, to find the week they were first seen.
CREATE INDEX events_people_by_actor ON events (tenant_id, project_id, actor, ts) WHERE actor <> '';

-- A key that may live in a browser bundle. The existing key is a bare write credential with no
-- scope at all, which is why no SPA could ever use this product: putting it in a bundle ships
-- it to every visitor. A public key is narrower on both axes — it may only write named events,
-- and only from an origin its owner listed.
--
-- `origins` empty on a public key is a refusal, never "any": a public key with no domain is the
-- unscoped key it exists to replace.
ALTER TABLE api_key ADD COLUMN kind text NOT NULL DEFAULT 'secret';
ALTER TABLE api_key ADD COLUMN origins text[] NOT NULL DEFAULT '{}';

