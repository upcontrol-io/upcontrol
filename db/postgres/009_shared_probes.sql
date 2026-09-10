-- +goose Up
-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION _uc_target_url(text);
-- +goose StatementEnd

-- +goose Down
-- Forward-only: the per-monitor tables are gone and checks was repartitioned
-- in place; no Down can rebuild them from what replaced them.
