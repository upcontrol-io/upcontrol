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

-- Shared probes (docs/plans/permanent-status-pages.md part 1) plus the columns
-- parts 2 and 3 need, in one forward-only migration.
--
-- probe_target is the thing the fleet fetches; monitor becomes a project's
-- subscription onto it. No cached paused/interval on the target: liveness and
-- cadence are derived in the lease query, so the reaper, releaseProject,
-- PATCH, DELETE and every cascade keep writing only monitor rows.
--
-- Heartbeats get a PRIVATE target (key from the monitor's public id) so one
-- family of tables serves both kinds and the fleet's filter moves to
-- probe_target.kind.
--
-- KEY ENCODING (deviation from the group spec, flagged to the owner): the
-- spec joined key parts with chr(0)/\x00. Postgres refuses NUL bytes in text
-- (SQLSTATE 54000, verified against the integration DB), so no \x00 key can
-- ever be stored. The join byte is \x1f (ASCII unit separator) instead:
-- storable, never produced by URL normalization, and stripped from user
-- parts by Go's targetkey so the three-part join stays unambiguous. The Go
-- side (internal/targetkey) mirrors this exactly; flip both if it moves.
--
-- Punycode is NOT done here (rare; convergence is safe: Go mints a fresh
-- target on the next subscribe).
CREATE TABLE probe_target (
  id          bigserial PRIMARY KEY,
  key         text NOT NULL UNIQUE,
  -- 'website' | 'heartbeat'
  kind        text NOT NULL,
  url         text NOT NULL,
  keyword     text,
  first_ok_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);

-- Migration-local URL normalizer, mirroring Go's targetkey.NormalizeURL for
-- the backfill only (same rules minus punycode): https when no scheme,
-- lowercase scheme+host, leading www. stripped, default port stripped,
-- fragment dropped, a bare trailing slash dropped.
CREATE FUNCTION _uc_target_url(u text) RETURNS text
LANGUAGE sql IMMUTABLE STRICT AS $$
WITH s AS (
  SELECT CASE WHEN u ~* '^[a-z][a-z0-9+.-]*://' THEN u ELSE 'https://' || u END AS w
), f AS (
  SELECT lower(split_part(w, '://', 1)) AS sch,
         split_part(split_part(w, '://', 2), '#', 1) AS rest
  FROM s
), a AS (
  SELECT sch,
         split_part(rest, '/', 1) AS auth,
         CASE WHEN position('/' IN rest) > 0 THEN substr(rest, position('/' IN rest)) ELSE '' END AS path
  FROM f
), h AS (
  SELECT sch, path,
         regexp_replace(lower(split_part(auth, ':', 1)), '^www\.', '') AS host,
         CASE WHEN sch = 'https' AND split_part(auth, ':', 2) = '443' THEN ''
              WHEN sch = 'http'  AND split_part(auth, ':', 2) = '80'  THEN ''
              ELSE split_part(auth, ':', 2) END AS port
  FROM a
)
SELECT sch || '://' || host ||
       CASE WHEN port <> '' THEN ':' || port ELSE '' END ||
       CASE WHEN path = '/' THEN '' ELSE path END
FROM h
$$;

-- One row per distinct normalized key over website monitors; the lowest
-- monitor id wins so url/keyword are deterministic when the same key came
-- from two spellings. first_ok_at starts NULL (measured, not asserted).
INSERT INTO probe_target (key, kind, url, keyword, first_ok_at, created_at)
SELECT DISTINCT ON (k) k, 'website', u, kw, NULL, now()
  FROM (SELECT 'website' || chr(31) || _uc_target_url(m.target) || chr(31) || coalesce(m.keyword, '') AS k,
               _uc_target_url(m.target) AS u, m.keyword AS kw, m.id
          FROM monitor m
         WHERE m.kind <> 'heartbeat') s
 ORDER BY k, id;

-- One private heartbeat target per heartbeat monitor. url is a tokenless
-- stable label ("heartbeat:<public id>"), never a ping URL: nothing fetches
-- it (the fleet's lease filters kind <> heartbeat; the ping door joins via
-- monitor.ping_token), and the monitor.target column historically held ping
-- URLs WITH their token - a redundant secret copy nobody reads. The create
-- path (internal/api/monitors.go) stores the same label.
INSERT INTO probe_target (key, kind, url, keyword, first_ok_at, created_at)
SELECT 'heartbeat' || chr(31) || m.public_id::text, 'heartbeat', 'heartbeat:' || m.public_id::text, NULL, NULL, now()
  FROM monitor m
 WHERE m.kind = 'heartbeat';

ALTER TABLE monitor ADD COLUMN target_id bigint;

UPDATE monitor m
   SET target_id = pt.id
  FROM probe_target pt
 WHERE pt.kind = 'heartbeat'
   AND m.kind = 'heartbeat'
   AND pt.key = 'heartbeat' || chr(31) || m.public_id::text;

UPDATE monitor m
   SET target_id = pt.id
  FROM probe_target pt
 WHERE pt.kind = 'website'
   AND m.kind <> 'heartbeat'
   AND pt.key = 'website' || chr(31) || _uc_target_url(m.target) || chr(31) || coalesce(m.keyword, '');

ALTER TABLE monitor ALTER COLUMN target_id SET NOT NULL;
-- One project cannot subscribe twice to one fetch. A project that already
-- watched one URL under two spellings now folded together would fail here;
-- that data does not exist (001's reset made every install fresh).
ALTER TABLE monitor ADD CONSTRAINT monitor_project_target_uniq UNIQUE (project_id, target_id);
ALTER TABLE monitor ADD CONSTRAINT monitor_target_fk FOREIGN KEY (target_id) REFERENCES probe_target(id);

-- target_schedule replaces monitor_schedule, keyed by target. In-flight lease
-- holders are cleared: their results would carry the pre-migration wire shape
-- and the fleet re-leases within seconds anyway.
CREATE TABLE target_schedule (
  target_id   bigint PRIMARY KEY REFERENCES probe_target(id) ON DELETE CASCADE,
  region      text NOT NULL DEFAULT 'default',
  next_due_at timestamptz NOT NULL DEFAULT now(),
  leased_by   text,
  lease_until timestamptz
);
CREATE INDEX target_schedule_due_idx ON target_schedule (next_due_at) WHERE leased_by IS NULL;
-- The expired-lease half of the admission predicate gets its own cheap index.
CREATE INDEX target_schedule_lease_idx ON target_schedule (lease_until);

INSERT INTO target_schedule (target_id, region, next_due_at)
SELECT m.target_id, min(ms.region), min(ms.next_due_at)
  FROM monitor_schedule ms
  JOIN monitor m ON m.id = ms.monitor_id
 GROUP BY m.target_id;

-- A monitor that somehow never got its schedule row must not become a target
-- the fleet silently never checks (the plan's no-silent-loss rule).
INSERT INTO target_schedule (target_id, next_due_at)
SELECT DISTINCT m.target_id, now()
  FROM monitor m
 WHERE NOT EXISTS (SELECT 1 FROM target_schedule ts WHERE ts.target_id = m.target_id);

DROP TABLE monitor_schedule;

-- target_facts replaces monitor_facts, keyed by target, with the
-- could-not-measure streak (part 3) and the refusal backoff (part 2) added.
CREATE TABLE target_facts (
  target_id           bigint PRIMARY KEY REFERENCES probe_target(id) ON DELETE CASCADE,
  -- ok|check|down|nodata|could_not_measure
  status              text NOT NULL DEFAULT 'nodata',
  ssl_expires_at      timestamptz,
  domain_expires_at   timestamptz,
  last_check_at       timestamptz,
  consecutive_failures int NOT NULL DEFAULT 0,
  consecutive_unmeasured int NOT NULL DEFAULT 0,
  consecutive_refusals int NOT NULL DEFAULT 0,
  backoff_until       timestamptz
);

-- The most recent state per target wins (targets shared by several monitors
-- take the freshest monitor's facts).
INSERT INTO target_facts (target_id, status, ssl_expires_at, domain_expires_at,
                          last_check_at, consecutive_failures)
SELECT DISTINCT ON (m.target_id)
       m.target_id, mf.status, mf.ssl_expires_at, mf.domain_expires_at,
       mf.last_check_at, mf.consecutive_failures
  FROM monitor_facts mf
  JOIN monitor m ON m.id = mf.monitor_id
 ORDER BY m.target_id, mf.last_check_at DESC NULLS LAST, m.id;

DROP TABLE monitor_facts;

-- checks grows target_id and interval_sec (the effective cadence the row was
-- taken at) and loses tenant_id/monitor_id; retention becomes partition
-- drops. Rows whose monitor was already deleted name no target and no reader
-- can resolve them, so they go: NOT NULL cannot hold their NULL.
ALTER TABLE checks ADD COLUMN target_id bigint;
UPDATE checks SET target_id = m.target_id
  FROM monitor m
 WHERE checks.monitor_id = m.id;
DELETE FROM checks WHERE target_id IS NULL;

ALTER TABLE checks ADD COLUMN interval_sec int;
-- 300 is the fleet's effective cadence for every existing row today.
UPDATE checks SET interval_sec = 300;

CREATE TABLE checks_new (LIKE checks INCLUDING DEFAULTS) PARTITION BY RANGE (ts);
ALTER TABLE checks_new DROP COLUMN tenant_id;
ALTER TABLE checks_new DROP COLUMN monitor_id;
ALTER TABLE checks_new ALTER COLUMN target_id SET NOT NULL;

-- Daily partitions over the FULL range of existing data, so the insert below
-- has somewhere to land, plus the default partition kept forever as the
-- safety net when the roller lags.
DO $$
DECLARE
  first_day date;
  last_day date := ((now() AT TIME ZONE 'UTC')::date + 3);
  d date;
BEGIN
  SELECT date(min(ts)) INTO first_day FROM checks;
  IF first_day IS NULL THEN
    first_day := (now() AT TIME ZONE 'UTC')::date;
  END IF;
  FOR d IN SELECT generate_series(first_day, last_day, '1 day') LOOP
    EXECUTE format('CREATE TABLE %I PARTITION OF checks_new FOR VALUES FROM (%L) TO (%L)',
                   'checks_' || to_char(d, 'YYYYMMDD'),
                   (d)::timestamp AT TIME ZONE 'UTC',
                   (d + 1)::timestamp AT TIME ZONE 'UTC');
  END LOOP;
END $$;

CREATE TABLE checks_default PARTITION OF checks_new DEFAULT;

INSERT INTO checks_new (ts, region, ok, status_code, error_class,
                        dns_ms, connect_ms, tls_ms, ttfb_ms, total_ms, body_hash,
                        target_id, interval_sec)
SELECT ts, region, ok, status_code, error_class,
       dns_ms, connect_ms, tls_ms, ttfb_ms, total_ms, body_hash,
       target_id, interval_sec
  FROM checks;

DROP TABLE checks;
ALTER TABLE checks_new RENAME TO checks;
CREATE INDEX checks_target_ts_idx ON checks (target_id, ts);
-- Unmeasured rows ("could not measure": HTTP 401/403/429) are stored with
-- ok = false but never count against uptime; every bucket/uptime query over
-- checks MUST carry this exact predicate (see the note in queries/schedule.sql).
CREATE INDEX checks_measurable_idx ON checks (target_id, ts)
  WHERE NOT (error_class = 'status' AND status_code IN (401, 403, 429));

-- status_page grows the part-2 columns: the host page holds its root probe
-- directly, pages can be removed, indexed, verified, audited and gated.
ALTER TABLE status_page
  ADD COLUMN root_target_id    bigint REFERENCES probe_target(id) ON DELETE SET NULL,
  ADD COLUMN is_host_page      boolean NOT NULL DEFAULT false,
  ADD COLUMN removed_at        timestamptz,
  ADD COLUMN indexed_at        timestamptz,
  ADD COLUMN reindex_hold      boolean NOT NULL DEFAULT false,
  ADD COLUMN host_verified_at  timestamptz,
  ADD COLUMN last_seen_at      timestamptz,
  -- 'watch' | 'seed'
  ADD COLUMN minted_source     text,
  ADD COLUMN minted_ip_hash    text,
  ADD COLUMN minted_visitor_hash text,
  ADD COLUMN minted_ua         text,
  ADD COLUMN index_opt_in      boolean NOT NULL DEFAULT false,
  ADD COLUMN created_at         timestamptz NOT NULL DEFAULT now(),
  -- DNS TXT proof of control; NULL once verified
  ADD COLUMN verification_token text,
  -- DNS TXT self-serve removal; NULL when never issued
  ADD COLUMN removal_token     text;

-- created_at: the mint's own timestamp, the axis the daily ceilings count
-- on. Existing pages predate the column; their tenant's birth is the best
-- record of it (every pre-009 page was minted with its tenant, hand-made
-- projects get the tenant's age as a harmless approximation).
UPDATE status_page sp
   SET created_at = t.created_at
  FROM tenant t
 WHERE t.id = sp.tenant_id;

-- Best-effort backfill (approximation, by design): a page whose project
-- watches the canonical host gets that target as its root; exactly-one keeps
-- a host watched under two keywords from silently picking one.
UPDATE status_page sp
   SET root_target_id = pt.id
  FROM project p
  JOIN probe_target pt ON pt.kind = 'website'
   AND pt.url = _uc_target_url('https://' || p.domain)
 WHERE sp.project_id = p.id
   AND p.domain <> ''
   AND EXISTS (SELECT 1 FROM monitor m WHERE m.project_id = p.id AND m.target_id = pt.id)
   AND NOT EXISTS (SELECT 1 FROM probe_target twin
                    WHERE twin.kind = 'website' AND twin.url = pt.url AND twin.id <> pt.id);

-- A page whose slug is the dashed lowercase canonical host is the host page.
UPDATE status_page sp
   SET is_host_page = true
  FROM project p
 WHERE sp.project_id = p.id
   AND p.domain <> ''
   AND sp.slug = trim(both '-' FROM regexp_replace(
        regexp_replace(lower(p.domain), '^www\.', ''), '[^a-z0-9]+', '-', 'g'));

CREATE TABLE blocked_host (
  domain     text PRIMARY KEY,
  reason     text,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Every incident opened after this migration records the effective interval
-- its target was checked at when it fired (NULL for legacy rows).
ALTER TABLE incident ADD COLUMN effective_interval_sec int;

-- The effective-cadence ladder in ONE place: the subscribers' tightest
-- interval when any exist (a live host page never loosens below 900), else
-- the host-page ladder - 300 s for the target's first 24 hours, 3600 s only
-- when every referencing page is unclaimed, unindexed and untouched for
-- 30 days, else 900. LOAD-BEARING: LeaseDueTargets and
-- EffectiveIntervalForTarget both call this function, and checks.interval_sec
-- plus incident.effective_interval_sec record what it returned - the lease
-- cadence and the recorded cadence must never drift, so neither query may
-- ever restate the ladder inline.
CREATE FUNCTION _uc_effective_interval(t probe_target) RETURNS int
LANGUAGE sql STABLE AS $$
SELECT CASE
         WHEN f.eff IS NOT NULL THEN
           CASE WHEN sp.n > 0 THEN LEAST(f.eff, 900) ELSE f.eff END
         WHEN t.created_at > now() - interval '24 hours' THEN 300
         WHEN sp.any_claimed = false
          AND sp.indexed_at IS NULL
          AND COALESCE(sp.last_seen_at, t.created_at) < now() - interval '30 days' THEN 3600
         ELSE 900
       END::int
  FROM (SELECT min(m.interval_sec)::int AS eff
          FROM monitor m
         WHERE m.target_id = t.id AND NOT m.paused) f,
       (SELECT count(*)::int AS n,
               max(pgp.indexed_at) AS indexed_at,
               max(pgp.last_seen_at) AS last_seen_at,
               bool_or(tn.claim_token_hash IS NULL) AS any_claimed
          FROM status_page pgp
          JOIN tenant tn ON tn.id = pgp.tenant_id
         WHERE pgp.root_target_id = t.id AND pgp.removed_at IS NULL) sp
$$;

DROP FUNCTION _uc_target_url(text);

-- Project freeze (docs/plans/trial-and-freeze.md): a plan buys LIVE projects.
-- Downgrade to Free keeps the tenant's most recently active project running and
-- freezes the rest as snapshots — no reads, no checks, no delivery, but nothing
-- deleted and nothing trimmed, so an upgrade restores them as they stopped.
ALTER TABLE project ADD COLUMN frozen_at timestamptz;
CREATE INDEX project_frozen_idx ON project (tenant_id) WHERE frozen_at IS NOT NULL;

-- The budget sweeper pauses over-limit monitors and must later unpause exactly
-- its own pauses, never an owner's: paused_by separates the two writers. NULL
-- (or 'owner') is a human choice, 'plan' is the sweeper's.
ALTER TABLE monitor ADD COLUMN paused_by text;

