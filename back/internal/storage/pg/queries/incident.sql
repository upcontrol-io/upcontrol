-- name: OpenIncident :one
-- Open a new incident. The caller computes the title and fingerprint (a hash of
-- monitor_id + detector, so repeated outages of the same monitor share a
-- fingerprint for grouping). Only one incident per monitor should be open at a
-- time — the caller checks GetOpenIncident first.
INSERT INTO incident (public_id, tenant_id, project_id, monitor_id, detector,
                      fingerprint, title, status, detected_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'down', now())
RETURNING id, public_id;

-- name: CloseIncident :exec
-- Close an open incident. close_reason is one of: recovered|maintenance|
-- monitor_deleted|by_human|absorbed|detector_off.
UPDATE incident
   SET resolved_at = now(), status = 'ok', close_reason = $1
 WHERE monitor_id = $2 AND resolved_at IS NULL;

-- name: GetOpenIncident :one
-- The currently-open incident for a monitor (if any). Used to avoid opening a
-- duplicate when the detector fires Open on an already-down monitor.
SELECT id, public_id, title, status, detected_at, notified_at
  FROM incident
 WHERE monitor_id = $1 AND resolved_at IS NULL;

-- name: AddIncidentUpdate :exec
-- Append a timeline entry. kind is one of: opened|escalated|acked|resolved|note.
INSERT INTO incident_update (incident_id, kind, text)
VALUES ($1, $2, $3);

-- name: TouchIncidentNotified :exec
-- Mark the notified_at timestamp when the first alert goes out for this incident.
UPDATE incident SET notified_at = now()
 WHERE id = $1 AND notified_at IS NULL;

-- name: GetMonitorForIncident :one
-- Fetch the monitor's tenant/project/name for incident context.
SELECT m.id, m.tenant_id, m.project_id, m.name, m.target, m.kind
  FROM monitor m WHERE m.id = $1;

-- name: ListIncidentUpdates :many
-- The incident's timeline, oldest first. These are the marks the lifecycle
-- writes (opened / notified / resolved) — the card renders them instead of the
-- empty array it used to get.
SELECT at, kind, text FROM incident_update
 WHERE incident_id = $1 ORDER BY at;

-- name: AddIncidentSliceLine :exec
-- One line of the frozen slice (plan §5.8, phase 1 at opened_at). Frozen means
-- copied out of the ring: the window displaces lines, and an incident that
-- outlives its own evidence is the failure this table exists to prevent.
INSERT INTO incident_slice (incident_id, seq, ts, level, service, message)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (incident_id, seq) DO NOTHING;

-- name: ListIncidentSlice :many
SELECT seq, ts, level, service, message FROM incident_slice
 WHERE incident_id = $1 ORDER BY seq;

-- name: GetProjectSeqNext :one
SELECT next FROM project_seq WHERE project_id = $1;
