-- name: LeaseDueMonitors :many
-- Find monitors due for a check that are not currently leased. The caller
-- (Lease handler) then atomically leases the returned IDs via SetLease.
-- Heartbeats are excluded: they have no target, the ping route records their
-- passes and the worker's miss sweep their failures.
SELECT ms.monitor_id, m.public_id, m.kind, m.target, m.keyword,
       m.interval_sec, m.availability_target
  FROM monitor_schedule ms
  JOIN monitor m ON m.id = ms.monitor_id
 WHERE ms.next_due_at <= now()
   AND ms.leased_by IS NULL
   AND m.paused = false
   AND m.kind <> 'heartbeat'
 ORDER BY ms.next_due_at
 LIMIT $1;

-- name: SetLease :execrows
-- Atomically lease a batch of monitors to one probe node. Returns the number of
-- rows actually leased (some may have been grabbed by another node between the
-- SELECT and this UPDATE — that's fine, they'll be in the next lease).
UPDATE monitor_schedule
   SET leased_by = $1, lease_until = $2
 WHERE monitor_id = ANY($3::bigint[])
   AND leased_by IS NULL;

-- name: ClearLeaseAndSchedule :exec
-- After results are submitted: clear the lease and set the next due time to
-- now + interval. This is what keeps the monitor checking at its cadence.
UPDATE monitor_schedule
   SET leased_by = NULL, lease_until = NULL, next_due_at = now() + make_interval(secs => $1::double precision)
 WHERE monitor_id = $2;

-- name: GetMonitorFacts :one
SELECT status, consecutive_failures
  FROM monitor_facts WHERE monitor_id = $1;

-- name: UpsertMonitorFacts :exec
-- Insert or update the monitor's facts after a check result is processed.
INSERT INTO monitor_facts (monitor_id, status, consecutive_failures, last_check_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (monitor_id) DO UPDATE
   SET status = EXCLUDED.status,
       consecutive_failures = EXCLUDED.consecutive_failures,
       last_check_at = now();

-- name: UpdateMonitorFactsExpiry :exec
-- Update SSL/domain expiry (collected opportunistically during a website check).
UPDATE monitor_facts
   SET ssl_expires_at = COALESCE(sqlc.narg(ssl_expires_at), ssl_expires_at),
       domain_expires_at = COALESCE(sqlc.narg(domain_expires_at), domain_expires_at)
 WHERE monitor_id = $1;

-- name: UpsertProbeNode :exec
-- Register or update the probe node's last-seen timestamp.
INSERT INTO probe_node (id, region, last_seen_at)
VALUES ($1, $2, now())
ON CONFLICT (id) DO UPDATE SET last_seen_at = now(), region = EXCLUDED.region;

-- name: MarkProbeBlind :exec
UPDATE probe_node SET blind_since = now() WHERE id = $1 AND blind_since IS NULL;

-- name: ClearLeasesForNode :execrows
-- When a probe goes blind, release its leases so other probes can pick them up.
UPDATE monitor_schedule SET leased_by = NULL, lease_until = NULL WHERE leased_by = $1;

-- name: EnsureMonitorSchedule :exec
-- Called when a monitor is created: seed the schedule row so the scheduler picks
-- it up on the next Lease.
INSERT INTO monitor_schedule (monitor_id, region, next_due_at)
VALUES ($1, $2, now())
ON CONFLICT (monitor_id) DO NOTHING;

-- name: ListMissedHeartbeats :many
-- Heartbeats whose window closed. next_due_at is "missed after": a ping sets
-- it to now + interval + grace, a recorded miss to now + interval.
SELECT m.id, m.tenant_id, m.name, m.interval_sec,
       COALESCE(mf.status, 'nodata')::text AS status,
       COALESCE(mf.consecutive_failures, 0)::int AS consecutive_failures
  FROM monitor_schedule ms
  JOIN monitor m ON m.id = ms.monitor_id
  LEFT JOIN monitor_facts mf ON mf.monitor_id = m.id
 WHERE m.kind = 'heartbeat' AND m.paused = false AND ms.next_due_at <= now()
 ORDER BY ms.next_due_at
 LIMIT 500;

-- name: SetHeartbeatDue :exec
-- Push the miss deadline out by secs; also clears a lease left from before
-- heartbeats stopped being handed to the probe.
UPDATE monitor_schedule
   SET next_due_at = now() + make_interval(secs => sqlc.arg(secs)::double precision),
       leased_by = NULL, lease_until = NULL
 WHERE monitor_id = sqlc.arg(monitor_id);
