-- The error-log scanner's Postgres side (docs/plans/channel-notify-settings.md).
-- The scan itself reads ClickHouse; these queries answer "who subscribed" and
-- remember what was already alerted so a persisting error does not page every
-- 60-second tick.

-- name: ListErrorSubscribedChannels :many
-- Channels that asked to hear about error logs at all. The scanner groups the
-- rows by tenant and reads each channel's window out of `notify` itself.
SELECT id, tenant_id, notify FROM alert_channel
 WHERE (notify->>'errorLogs')::boolean IS TRUE
    OR (notify->>'repeatingErrorLogs')::boolean IS TRUE
 ORDER BY tenant_id;

-- name: ListErrorAlertState :many
SELECT fingerprint, kind, last_alerted FROM error_alert_state
 WHERE tenant_id = $1;

-- name: UpsertErrorAlertState :exec
INSERT INTO error_alert_state (tenant_id, fingerprint, kind, last_alerted)
VALUES ($1, $2, $3, now())
ON CONFLICT (tenant_id, fingerprint, kind) DO UPDATE SET last_alerted = now();
