-- name: GetOpenIncidentByFingerprint :one
SELECT id, public_id, detected_at FROM incident WHERE tenant_id = $1 AND fingerprint = $2 AND resolved_at IS NULL;

-- name: CloseIncidentByFingerprint :exec
UPDATE incident SET resolved_at = now(), status = 'ok', close_reason = $1 WHERE tenant_id = $2 AND fingerprint = $3 AND resolved_at IS NULL;

-- name: GetErrorAlertState :one
SELECT last_alerted FROM error_alert_state WHERE tenant_id = $1 AND project_id = $2 AND fingerprint = $3 AND kind = $4;

-- name: ListProjectsForDetect :many
SELECT id, tenant_id, domain FROM project ORDER BY id;
