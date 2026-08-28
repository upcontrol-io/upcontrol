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

-- Idempotency must return the SAME accepted count on replay (plan §3.8: "a
-- retry of the same batch gets the same accepted"). ingest_batch already
-- dedupes on batch_key;
-- this adds the count it returns so a replay's receipt matches the first.
ALTER TABLE ingest_batch ADD COLUMN accepted int NOT NULL DEFAULT 0;


-- One outstanding magic-link code per email. The code is stored as sha256(code)
-- (constant-time compared), single-use (redeemed_at), with an attempt cap and a
-- TTL (expires_at). A new request overwrites the row — the latest code wins, the
-- prior one is invalidated. Block 3: previously the code was generated and never
-- stored, so any token logged you in (and prod login was impossible).
CREATE TABLE magic_link_code (
    email text PRIMARY KEY,
    code_hash bytea NOT NULL,
    attempts int NOT NULL DEFAULT 0,
    expires_at timestamptz NOT NULL,
    redeemed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Sliding-window throttle for the anonymous magic-link endpoint, keyed by the
-- caller's IP. The endpoint sends mail, so it must be rate-limited per IP as
-- well as per email (the per-email cooldown is read off magic_link_code.created_at).
CREATE TABLE magic_link_ip (
    ip text PRIMARY KEY,
    first_at timestamptz NOT NULL DEFAULT now(),
    count int NOT NULL DEFAULT 1
);

-- The status page's own settings: which components are shown, and whether the
-- network strip and the incident history are published. They live in their own
-- column rather than inside `components`, which holds the published list.
ALTER TABLE status_page ADD COLUMN config jsonb NOT NULL DEFAULT '{}'::jsonb;

-- Until Aug 14, 2026 every account's project was created with a hardcoded
-- "example.com", whatever site its owner had actually asked us to watch — so
-- the Projects tab, the sidebar and the delete-confirmation prompt all named a
-- domain the customer had never typed. Provisioning now passes the checked host;
-- this repairs the rows created before it did.
--
-- Only rows that still hold the placeholder are touched, and only when the
-- account's earliest check points somewhere else — an account that genuinely
-- watches example.com keeps its name. `split_part` peels the scheme and the
-- path off the monitor's target, matching what bareHost() does in Go.
UPDATE project p
   SET domain = sub.host
  FROM (
    SELECT DISTINCT ON (m.project_id)
           m.project_id,
           split_part(split_part(regexp_replace(m.target, '^https?://', ''), '/', 1), ':', 1) AS host
      FROM monitor m
     WHERE m.kind <> 'heartbeat'
     ORDER BY m.project_id, m.id
  ) AS sub
 WHERE p.id = sub.project_id
   AND p.domain = 'example.com'
   AND sub.host <> ''
   AND sub.host <> 'example.com';

-- Status pages created before Aug 14, 2026 are addressed as /status/prj-12: our
-- own row id, handed to a customer's visitors. The slug is now the site's name
-- (harpa.ai -> harpa-ai), so this renames the pages that predate it.
--
-- Rewriting a slug is normally forbidden — it is a link somebody may already
-- have shared — but "prj-12" is an id nobody would have chosen to share, the
-- feature is days old, and leaving it means the format the product promises
-- exists only for accounts created after a certain Thursday.
--
-- Renamed only when the derived slug is free: the column is UNIQUE across
-- tenants, so the second page for a host keeps the id-shaped name rather than
-- failing the migration. `regexp_replace` mirrors slugFromHost() in Go:
-- lowercase, every run of non-alphanumerics to one dash, trimmed, max 40.
--
-- DISTINCT ON is not a tidiness flourish: several projects can derive the SAME
-- slug (every account created before the fix was called "example.com"), and
-- `NOT EXISTS` only sees the pre-statement snapshot, so without it the rows
-- collide with each other mid-UPDATE and the whole migration fails. One row per
-- name, oldest first; the rest keep their id-shaped slug.
UPDATE status_page s
   SET slug = cand.slug
  FROM (
    SELECT DISTINCT ON (slug) id, slug
      FROM (
        SELECT sp.id,
               left(
                 trim(both '-' from regexp_replace(lower(p.domain), '[^a-z0-9]+', '-', 'g')),
                 40
               ) AS slug
          FROM status_page sp
          JOIN project p ON p.id = sp.project_id
         WHERE sp.slug ~ '^prj-[0-9]+$'
      ) AS derived
     WHERE derived.slug <> ''
       AND NOT EXISTS (SELECT 1 FROM status_page t WHERE t.slug = derived.slug)
     ORDER BY slug, id
  ) AS cand
 WHERE s.id = cand.id;

-- Check names created before Aug 14, 2026 were built from the address the
-- visitor typed rather than from the target, so a pasted "datrade.io/" produced
-- "datrade.io//pricing", and every subdomain discovery found (api., app.) was
-- labelled with the root host — a status page listing three components all
-- called "harpa.ai". These names are published, so they are worth repairing.
--
-- Derived exactly as monitorName() now does: the target's own host, plus its
-- path when the path is not the root. Website checks only — a heartbeat's name
-- is its owner's word, not a URL.
UPDATE monitor
   SET name = regexp_replace(
                rtrim(regexp_replace(target, '^https?://', ''), '/'),
                '\?.*$', ''
              )
 WHERE kind <> 'heartbeat'
   AND target ~ '^https?://'
   AND name <> regexp_replace(
                 rtrim(regexp_replace(target, '^https?://', ''), '/'),
                 '\?.*$', ''
               );

-- The Grafana receiver was offered as a connect tile but never existed:
-- internal/source/webhook verifies stripe, github and vercel only, so a POST to
-- /hooks/grafana had nowhere to land. Pressing the tile still wrote a
-- source_connection row, which then sat on the Sources screen as a feed that
-- could not receive anything. The tile is gone (Aug 14, 2026, user decision);
-- these rows go with it, or the screen keeps a card nothing can ever update.
DELETE FROM source_connection WHERE kind = 'grafana';

-- One connection per kind, per project. Pressing "Deploy hooks" on /app/sources
-- inserted a row every time, so a second click left the screen with two
-- identical cards (and a third with three) — each with its own Pause and its own
-- Disconnect, describing the same single webhook endpoint. There is exactly one
-- URL per provider (hookUrl(kind) has no per-row part), so a second row could
-- never mean anything different from the first.
--
-- Dedupe before the index: keep the row that has actually heard from the
-- provider (last_signal_at), and among equals the oldest — deleting the one with
-- signals would throw away the only evidence the feed works.
DELETE FROM source_connection
 WHERE id NOT IN (
   SELECT DISTINCT ON (project_id, kind) id
     FROM source_connection
    ORDER BY project_id, kind, last_signal_at DESC NULLS LAST, id
 );

CREATE UNIQUE INDEX source_connection_project_kind_key
  ON source_connection (project_id, kind);

-- How often a check may run is now a plan axis (Aug 14, 2026, user decision):
-- Free runs every 5 minutes, paid plans down to the minute. It lives here rather
-- than in Go because every other plan number does — one row per plan, read by
-- the create/patch gate and by GET /v1/plan, so the client never hardcodes it.
--
-- The default is 60s: a plan added later gets the floor the fleet is actually
-- sized against (backend-from-new-plan.md §2.3), never an unbounded one.
ALTER TABLE plan_entitlement ADD COLUMN min_interval_sec int NOT NULL DEFAULT 60;

UPDATE plan_entitlement SET min_interval_sec = 300 WHERE plan = 'Free';

-- Checks that already run faster than their plan now allows are left alone: the
-- floor is a gate on what you may ask for, not a reason to silently slow down a
-- monitor somebody is relying on.

-- What a channel is notified about (docs/plans/channel-notify-settings.md).
-- Sparse jsonb: an absent key means the default (websiteDown on, everything
-- else off), so every existing row keeps today's behaviour with no backfill.
-- This narrows "a channel is a destination and nothing else" (user decision,
-- Aug 14, 2026): the settings pick which CLASSES of alert land here — they are
-- not, and must not become, a per-monitor routing matrix.
ALTER TABLE alert_channel ADD COLUMN notify jsonb NOT NULL DEFAULT '{}'::jsonb;

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

-- Universal inbound hooks (Aug 14, 2026, user decision — docs/plans/
-- universal-hooks.md): every source connection carries its own hook token, and
-- POST /hooks/{token} attributes the event to the connection's tenant and
-- project. The token in the URL is the credential (128 bits, per connection,
-- revoked by disconnecting), which is what lets ANY provider that can POST
-- JSON use the endpoint — the three named providers had one global secret
-- each and wrote every event as tenant 0.
ALTER TABLE source_connection ADD COLUMN hook_token text;

-- Existing rows get a backfill token so their URL exists the moment the front
-- asks for it. md5(random) is fine for a backfill: new tokens come from
-- crypto/rand in Go, and a token's job is to be unguessable-enough to name a
-- write-only event sink, not to be a session credential.
UPDATE source_connection
   SET hook_token = md5(random()::text || clock_timestamp()::text || id::text)
 WHERE hook_token IS NULL;

CREATE UNIQUE INDEX source_connection_hook_token_key
    ON source_connection (hook_token);

-- The hook panel's receipt (Aug 14, 2026, user decision): when an event lands
-- on a connection's hook we show WHAT landed — "Received github_push · 2s ago"
-- — because the provider's own "send test webhook" button is the real tester,
-- and our job is to display the proof. One column, overwritten per event: the
-- receipt is the last message, not a log (the log is the events table).
ALTER TABLE source_connection ADD COLUMN last_event text;

-- Anonymous projects (cli/SPEC.md §7.1, docs/plans/one-command-install.md):
-- `npx upcontrol init` provisions a tenant+project+key with NO person attached,
-- so data can flow before any registration. The claim token is the one-time
-- proof of provenance: whoever presents it (signed in) becomes a member of the
-- tenant. Stored as sha256, like magic-link codes — the raw token exists only
-- in the CLI's output. Claiming clears the hash (one-time) and stamps
-- claimed_at; claiming never changes the API key (a rotation would require a
-- release of the customer's deployed app).
ALTER TABLE tenant ADD COLUMN claim_token_hash bytea;
ALTER TABLE tenant ADD COLUMN claimed_at timestamptz;
CREATE UNIQUE INDEX tenant_claim_token_key ON tenant (claim_token_hash)
  WHERE claim_token_hash IS NOT NULL;

-- One-time install tokens (docs/plans/front-distribution-alignment.md §1):
-- the dashboard's install card generates `npx upcontrol init --token uct_...`
-- so a signed-in user's CLI lands the key of THEIR project instead of minting
-- an anonymous one. The token is the only thing that ever appears on screen;
-- redeeming it issues an additional api_key row and returns the secret once.
-- Stored as sha256 like magic-link codes and claim tokens; single-use via
-- used_at; short TTL because it exists only to cross from browser to terminal.
CREATE TABLE install_token (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  token_hash    bytea NOT NULL UNIQUE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  used_at       timestamptz
);

-- Telegram identity + recipients (openspec/changes/telegram-bot-auth-and-recipients).
-- Decisions (D1/D2/D4/D9, closed Aug 2026):
--   * numbers 3/10/30/100 — same scale as http_checks (owner-approved default);
--   * the owner counts inside the limit: Free = the owner's own chat + 2 teammates;
--   * a recipient is a person (alert_channel.recipient_person_id), a broadcast
--     group is a channel with recipient_person_id NULL.

-- One-time invite tokens: the deep link `t.me/<bot>?start=inv_<token>` is the
-- only way to link a Telegram account to a person (the old `prj-N` link was
-- guessable). Stored as sha256 like magic-link codes and claim tokens; the raw
-- token appears exactly once in the POST response and never in a log.
CREATE TABLE telegram_invite (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  role          text NOT NULL,                    -- notify|login|owner
  invited_by    bigint NOT NULL REFERENCES person(id) ON DELETE CASCADE,
  token_hash    bytea NOT NULL UNIQUE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  redeemed_at   timestamptz
);

-- A personal telegram destination points at the person it alerts; a broadcast
-- group keeps it NULL (D4/D5). muted_until is the /mute window.
ALTER TABLE alert_channel ADD COLUMN recipient_person_id bigint REFERENCES person(id);
ALTER TABLE alert_channel ADD COLUMN muted_until timestamptz;

ALTER TABLE plan_entitlement ADD COLUMN telegram_recipients int NOT NULL DEFAULT 3;
UPDATE plan_entitlement SET telegram_recipients = 10 WHERE plan = 'Indie';
UPDATE plan_entitlement SET telegram_recipients = 30 WHERE plan = 'Growth';
UPDATE plan_entitlement SET telegram_recipients = 100 WHERE plan = 'Agency';

-- Per-call token ledger (docs/plans/ai-provider-and-scenarios.md, D9/D11).
--   * ai_usage gains monthly token totals alongside the existing call count;
--     the count stays the plan quota axis (D8), tokens are bookkeeping only.
--   * ai_call is one row per real LLM call. Cache hits and heuristic answers
--     insert nothing — no tokens were spent (D9).

ALTER TABLE ai_usage
  ADD COLUMN prompt_tokens bigint NOT NULL DEFAULT 0,
  ADD COLUMN completion_tokens bigint NOT NULL DEFAULT 0;

CREATE TABLE ai_call (
  id                bigserial PRIMARY KEY,
  tenant_id         bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  scenario          text NOT NULL,                 -- registry key, e.g. explain_logs
  model             text NOT NULL,
  prompt_tokens     bigint NOT NULL,
  completion_tokens bigint NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_call_tenant_created_idx ON ai_call (tenant_id, created_at);

-- Installer-collected project spec (docs/plans/ai-provider-and-scenarios.md,
-- D15b/D16): {name, description, framework, runtime, language} plus storage
-- bookkeeping (source, collectedAt). NULL = never collected. Written only by
-- PUT /v1/project/meta after scrubbing; read by the AI explain context block.

ALTER TABLE project ADD COLUMN meta jsonb;

-- LemonSqueezy billing state, one row per tenant. The webhook
-- (/hooks/lemonsqueezy) is the only writer: tenant.plan / tenant.billing are
-- derived from it on every subscription event, so the entitlement gates read
-- the same facts the money side wrote (docs/rules/data-layer.md: measured,
-- not asserted — a plan a webhook never confirmed is Free).
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

UPDATE plan_entitlement SET ai_explains =  50 WHERE plan = 'Indie';
UPDATE plan_entitlement SET ai_explains = 200 WHERE plan = 'Growth';
UPDATE plan_entitlement SET ai_explains = 400 WHERE plan = 'Agency';

-- web_visitor is the Postgres half of product analytics (plan: product-analytics
-- §Decision 6): a directory of current visitor state — first-touch attribution,
-- identity, counters. ClickHouse web_events is the raw stream; this table is
-- what answers "who is this visitor". First-touch columns are written exactly
-- once (INSERT ... ON CONFLICT DO NOTHING in queries/analytics.sql); identity
-- and last_* columns are touched on every recorder flush. token_hash is
-- sha256(uc_vid cookie): the raw cookie never reaches the database.
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
-- Partial indexes: the two lookup paths that matter are "known visitors"
-- (person linked) and "reachable visitors" (email left); anonymous rows
-- neither query ever returns do not need index entries.
CREATE INDEX web_visitor_person_idx ON web_visitor (person_id) WHERE person_id IS NOT NULL;
CREATE INDEX web_visitor_email_idx ON web_visitor (email) WHERE email <> '';

INSERT INTO plan_entitlement
  (plan, http_checks, regions, window_lines, window_hours, retain_mult,
   ai_explains, incident_days, min_interval_sec, telegram_recipients)
VALUES
  ('Self-hosted', 1000, 10, 45000000, 720, 1.0, NULL, 730, 60, 1000);

CREATE TABLE instance_setting (
  key        text PRIMARY KEY,
  value_enc  bytea NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

UPDATE incident
   SET resolved_at   = now(),
       status        = 'ok',
       close_reason  = 'monitor_deleted'
 WHERE resolved_at IS NULL
   AND monitor_id IS NULL
   AND detector = 'availability';

INSERT INTO incident_update (incident_id, kind, text)
SELECT id, 'resolved', 'Monitor deleted'
  FROM incident
 WHERE close_reason = 'monitor_deleted'
   AND detector = 'availability'
   AND NOT EXISTS (
         SELECT 1 FROM incident_update u
          WHERE u.incident_id = incident.id AND u.kind = 'resolved'
       );

ALTER TABLE person ADD COLUMN telegram_username text;
ALTER TABLE alert_channel ADD COLUMN label text;
ALTER TABLE telegram_invite ADD COLUMN person_id bigint REFERENCES person(id) ON DELETE CASCADE;

UPDATE alert_channel ac
   SET label = p.name
  FROM person p
 WHERE ac.recipient_person_id = p.id
   AND ac.kind = 'telegram'
   AND p.name <> '';

-- How many projects a tenant may have is now a plan axis (Aug 27, 2026, user
-- decision — docs/plans/projects-axis.md): Free 1, Indie 2, Growth 5, Agency 10.
-- Self-hosted stays NULL: the same "NULL = unlimited" contract as ai_explains.
-- A project is a row in `project` within the person's ONE tenant; billing stays
-- tenant-level, so the ladder gates the count, nothing else.
ALTER TABLE plan_entitlement ADD COLUMN projects int;

UPDATE plan_entitlement SET projects =  1 WHERE plan = 'Free';
UPDATE plan_entitlement SET projects =  2 WHERE plan = 'Indie';
UPDATE plan_entitlement SET projects =  5 WHERE plan = 'Growth';
UPDATE plan_entitlement SET projects = 10 WHERE plan = 'Agency';

-- The session remembers which project the person is working in. NULL (or a
-- deleted project, via ON DELETE SET NULL) falls back to the tenant's lowest
-- project id at read time — no backfill, and single-user mode, which has no
-- session row, is unaffected.
ALTER TABLE session ADD COLUMN project_id bigint REFERENCES project(id) ON DELETE SET NULL;

-- AI Explain is removed from the product (Aug 28, 2026, user decision): no LLM
-- triage of logs or incidents, so the ledger, the cache, the quota axis, the
-- project spec that existed only as explain context and the instance-level
-- provider settings all go with it.
DROP TABLE ai_call;
DROP TABLE ai_explain_cache;
DROP TABLE ai_usage;
ALTER TABLE plan_entitlement DROP COLUMN ai_explains;
ALTER TABLE project DROP COLUMN meta;
DELETE FROM instance_setting WHERE key IN ('ai_api_key','ai_model','ai_base_url');

-- Heartbeat monitors created before the ping route existed: mint their token
-- and open a first window, or every one of them is "missed" a minute after
-- this deploy, before anyone has seen the URL.
UPDATE monitor
   SET ping_token = replace(gen_random_uuid()::text, '-', '')
 WHERE kind = 'heartbeat' AND ping_token IS NULL;

UPDATE monitor_schedule ms
   SET next_due_at = now() + make_interval(secs => m.interval_sec + COALESCE(m.grace_sec, m.interval_sec)),
       leased_by = NULL, lease_until = NULL
  FROM monitor m
 WHERE m.id = ms.monitor_id AND m.kind = 'heartbeat';

