-- The error-log scanner's queries.
-- The scan itself reads the logs table; these queries answer "who subscribed"
-- and remember what was already alerted so a persisting error does not page
-- every 60-second tick.

-- name: ListErrorSubscribedChannels :many
-- Channels that asked to hear about error logs at all. The scanner groups the
-- rows by project and reads each channel's window out of `notify` itself.
-- Frozen projects keep ingesting (their data must stay continuous through a
-- freeze) but never page: a snapshot alerts nobody.
SELECT c.id, c.tenant_id, c.project_id, c.notify FROM alert_channel c
 WHERE ((c.notify->>'errorLogs')::boolean IS TRUE
    OR (c.notify->>'repeatingErrorLogs')::boolean IS TRUE)
   AND NOT EXISTS (SELECT 1 FROM project p
                    WHERE p.id = c.project_id AND p.frozen_at IS NOT NULL)
 ORDER BY c.tenant_id, c.project_id;

-- name: ListErrorAlertState :many
SELECT fingerprint, kind, last_alerted FROM error_alert_state
 WHERE tenant_id = $1 AND project_id = $2;

-- name: UpsertErrorAlertState :exec
INSERT INTO error_alert_state (tenant_id, project_id, fingerprint, kind, last_alerted)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (tenant_id, project_id, fingerprint, kind) DO UPDATE SET last_alerted = now();
