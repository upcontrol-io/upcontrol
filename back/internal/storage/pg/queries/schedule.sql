-- The shared-probe pipeline (migration 009, plan part 1): the fleet leases
-- probe_targets, not monitors. monitor rows are subscriptions; liveness and
-- cadence are DERIVED here, never cached on the target.
--
-- UNMEASURED ROWS (never counted against uptime): a checks row stored with
-- error_class = 'status' AND status_code IN (401, 403, 429) is a
-- "could not measure" reading (bot filter / auth wall / rate limit). It is
-- stored with ok = false, but every uptime or bucket query over checks MUST
-- carry this exact predicate (partial index checks_measurable_idx matches
-- it): WHERE NOT (error_class = 'status' AND status_code IN (401, 403, 429)).
-- Group 2's statusComponents / CheckBuckets read paths copy this fragment.

-- name: LeaseDueTargets :many
-- Targets due for a check, not leased, not heartbeats, not in refusal
-- backoff. A target is due only when it has at least one unpaused subscriber
-- (f.eff) or a live host page referencing it (sp.n). interval_sec is the
-- derived cadence _uc_effective_interval computes (migration 009): the
-- subscribers' tightest interval when any exist (a host page never loosens
-- below 900), else the host-page ladder: 300 s for the target's first 24
-- hours, 900 s after, and 3600 s only when every referencing page is
-- unclaimed, unindexed and untouched for 30 days.
-- prev_leased_by is the stale holder the admission predicate just evicted
-- (NULL when the slot was free): the caller logs it, silence is the one
-- defect this product may not have. Paying subscribers' targets lease first
-- so a free page never delays a customer's minute check.
SELECT t.id, t.kind, t.url, t.keyword,
       _uc_effective_interval(t) AS interval_sec,
       (p.n > 0) AS paying,
       ts.leased_by AS prev_leased_by
  FROM probe_target t
  JOIN target_schedule ts ON ts.target_id = t.id
  LEFT JOIN target_facts tf ON tf.target_id = t.id
  LEFT JOIN LATERAL (
    SELECT min(m.interval_sec)::int AS eff
      FROM monitor m
     WHERE m.target_id = t.id AND NOT m.paused
  ) f ON true
  LEFT JOIN LATERAL (
    SELECT count(*)::int AS n,
           max(pgp.indexed_at) AS indexed_at,
           max(pgp.last_seen_at) AS last_seen_at,
           bool_or(tn.claim_token_hash IS NULL) AS any_claimed
      FROM status_page pgp
      JOIN tenant tn ON tn.id = pgp.tenant_id
     WHERE pgp.root_target_id = t.id AND pgp.removed_at IS NULL
  ) sp ON true
  LEFT JOIN LATERAL (
    SELECT count(*)::int AS n
      FROM monitor m
      JOIN tenant tn ON tn.id = m.tenant_id
     WHERE m.target_id = t.id AND NOT m.paused AND tn.plan <> 'Free'
  ) p ON true
 WHERE ts.next_due_at <= now()
   AND (ts.leased_by IS NULL OR ts.lease_until < now())
   AND t.kind <> 'heartbeat'
   AND (f.eff IS NOT NULL OR sp.n > 0)
   AND (tf.backoff_until IS NULL OR tf.backoff_until < now())
 ORDER BY paying DESC, ts.next_due_at
 LIMIT $1;

-- name: SetLease :exec
-- Atomically lease the batch to one probe node. The admission predicate is
-- the lease query's: a slot whose lease expired is taken back here.
UPDATE target_schedule
   SET leased_by = $1, lease_until = now() + interval '5 minutes'
 WHERE target_id = ANY($2::bigint[])
   AND (leased_by IS NULL OR lease_until < now());

-- name: ClearLeaseAndScheduleTarget :exec
-- After results are submitted: release the lease and set the next due time
-- to now + $1 seconds (the target's effective interval, recomputed by
-- EffectiveIntervalForTarget before this runs).
UPDATE target_schedule
   SET leased_by = NULL, lease_until = NULL,
       next_due_at = now() + make_interval(secs => $1::double precision)
 WHERE target_id = $2;

-- name: GetTargetFacts :one
-- The detector state and the backoff counters, one row per target.
SELECT status, consecutive_failures, consecutive_unmeasured,
       consecutive_refusals, backoff_until
  FROM target_facts WHERE target_id = $1;

-- name: UpsertTargetFacts :exec
-- Persist the detector state after a result. consecutive_refusals and
-- backoff_until are the refusal backoff (part 2): raised on unmeasured
-- results, reset to 0/NULL on any measurable one. The caller computes the
-- doubling in Go; this only stores it.
INSERT INTO target_facts (target_id, status, consecutive_failures,
                          consecutive_unmeasured, consecutive_refusals,
                          backoff_until, last_check_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (target_id) DO UPDATE
   SET status = EXCLUDED.status,
       consecutive_failures = EXCLUDED.consecutive_failures,
       consecutive_unmeasured = EXCLUDED.consecutive_unmeasured,
       consecutive_refusals = EXCLUDED.consecutive_refusals,
       backoff_until = EXCLUDED.backoff_until,
       last_check_at = now();

-- name: UpdateTargetFactsExpiry :exec
-- SSL/domain expiry (collected opportunistically during a website check).
UPDATE target_facts
   SET ssl_expires_at = COALESCE(sqlc.narg(ssl_expires_at), ssl_expires_at),
       domain_expires_at = COALESCE(sqlc.narg(domain_expires_at), domain_expires_at)
 WHERE target_id = $1;

-- name: SetTargetFirstOk :exec
-- The first successful measurement stamps first_ok_at once; the reaper's
-- host-page exemption and the index gate read it.
UPDATE probe_target SET first_ok_at = now()
 WHERE id = $1 AND first_ok_at IS NULL;

-- name: EnsureTargetSchedule :exec
-- Seed the schedule row when a target is created so the next Lease picks it
-- up (target-keyed).
INSERT INTO target_schedule (target_id, region, next_due_at)
VALUES ($1, $2, now())
ON CONFLICT (target_id) DO NOTHING;

-- name: PullTargetDue :exec
-- A subscription starts now: any action that lowers a target's effective
-- interval pulls its next check to the earliest slot, so a new subscriber's
-- first check runs at the next lease, not one interval later.
UPDATE target_schedule
   SET next_due_at = LEAST(next_due_at, now())
 WHERE target_id = $1;

-- name: ClearLeasesForNode :exec
-- When a probe goes blind, release its leases so other probes pick them up.
UPDATE target_schedule SET leased_by = NULL, lease_until = NULL WHERE leased_by = $1;

-- name: UpsertProbeNode :exec
-- Register or update the probe node's last-seen timestamp.
INSERT INTO probe_node (id, region, last_seen_at)
VALUES ($1, $2, now())
ON CONFLICT (id) DO UPDATE SET last_seen_at = now(), region = EXCLUDED.region;

-- name: MarkProbeBlind :exec
UPDATE probe_node SET blind_since = now() WHERE id = $1 AND blind_since IS NULL;

-- name: ListMissedHeartbeats :many
-- Heartbeats whose window closed. next_due_at is "missed after": a ping sets
-- it to now + interval + grace, a recorded miss to now + interval. The
-- heartbeat's private target carries the schedule and detector state now;
-- the monitor row stays the identity for incidents and alerts.
SELECT m.id, m.target_id, m.tenant_id, m.name, m.interval_sec,
       COALESCE(tf.status, 'nodata')::text AS status,
       COALESCE(tf.consecutive_failures, 0)::int AS consecutive_failures
  FROM monitor m
  JOIN target_schedule ts ON ts.target_id = m.target_id
  LEFT JOIN target_facts tf ON tf.target_id = m.target_id
 WHERE m.kind = 'heartbeat' AND m.paused = false AND ts.next_due_at <= now()
 ORDER BY ts.next_due_at
 LIMIT 500;

-- name: SetHeartbeatDue :exec
-- Push the miss deadline out by secs; also clears a lease left from before
-- heartbeats stopped being handed to the probe. Monitor-keyed: the API's
-- create path calls it before the monitor has ever been leased.
UPDATE target_schedule ts
   SET next_due_at = now() + make_interval(secs => sqlc.arg(secs)::double precision),
       leased_by = NULL, lease_until = NULL
  FROM monitor m
 WHERE m.id = sqlc.arg(monitor_id) AND ts.target_id = m.target_id;

-- name: GetOrCreateProbeTarget :one
-- The one door every subscribe goes through: two simultaneous Start watching
-- on one host cannot race. DO UPDATE (a url no-op rewrite) so RETURNING
-- yields the id on the conflict path too.
INSERT INTO probe_target (key, kind, url, keyword)
VALUES ($1, $2, $3, $4)
ON CONFLICT (key) DO UPDATE SET url = EXCLUDED.url
RETURNING id;

-- name: ActiveSubscriberMonitors :many
-- Unpaused subscriptions on a target: while a target is down, every one of
-- them holds an open incident (idempotent Open), so a subscriber who joins
-- an outage gets its incident and alert within one check.
SELECT id FROM monitor WHERE target_id = $1 AND NOT paused;

-- name: AllSubscriberMonitors :many
-- Every subscription, paused or not: recovery closes them all.
SELECT id FROM monitor WHERE target_id = $1;

-- name: EffectiveIntervalForTarget :one
-- The lease query's cadence for one target, recomputed at submit time: the
-- checks row and the incident's effective_interval_sec record the cadence
-- the row was actually taken at. The derivation itself lives in
-- _uc_effective_interval (migration 009), the one ladder both queries call.
SELECT _uc_effective_interval(t) AS interval_sec
  FROM probe_target t
 WHERE t.id = $1;

-- name: SetIncidentEffectiveInterval :exec
-- Stamp the effective interval at opening on a freshly created incident
-- (review decision 14: the interval fold is recorded, not undone).
UPDATE incident SET effective_interval_sec = sqlc.arg(effective_interval_sec)::int WHERE id = $1;
