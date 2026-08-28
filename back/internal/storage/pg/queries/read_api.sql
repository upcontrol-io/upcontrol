-- name: GetPlanEntitlement :one
SELECT * FROM plan_entitlement WHERE plan = $1;

-- name: CountMonitors :one
-- Same axis as CountMonitorsByTenant, and it must stay the same predicate: this
-- one is the number the sidebar and the Plan page print, that one is the gate.
SELECT count(*)::int FROM monitor WHERE tenant_id = $1 AND kind <> 'heartbeat';

-- name: GetProjectWindowInfo :one
SELECT cutoff_seq, retain_seq, window_hours, beyond_errors, computed_at
  FROM project_window WHERE project_id = $1;

-- name: ListChannelsByTenant :many
SELECT id, public_id, kind, target, notify, breaker_open_until, created_at,
       muted_until, label, recipient_person_id
  FROM alert_channel WHERE tenant_id = $1 ORDER BY created_at;

-- name: GetTenantPlan :one
SELECT plan FROM tenant WHERE id = $1;

-- name: ListRecipientsByTenant :many
SELECT tm.role, tm.status, p.id, p.public_id, p.email, p.name, p.telegram_id, p.telegram_username
  FROM tenant_member tm
  JOIN person p ON p.id = tm.person_id
 WHERE tm.tenant_id = $1
 ORDER BY tm.status, p.name;

-- name: ListIncidentsByTenant :many
-- tenant_id is selected back so incidentWithEvidence can scope the events read
-- to the same tenant without a second lookup.
SELECT id, tenant_id, public_id, title, status, detected_at, resolved_at, affected_count, close_reason
  FROM incident WHERE tenant_id = $1 ORDER BY detected_at DESC LIMIT $2;

-- name: GetAPIKeyForTenant :one
SELECT id, prefix, state, created_at, last_used_at
  FROM api_key WHERE tenant_id = $1 AND state != 'revoked' ORDER BY created_at DESC LIMIT 1;

-- name: ListKeyUsage :many
SELECT at, source, outcome FROM key_usage_log
  WHERE tenant_id = $1 ORDER BY at DESC LIMIT 10;

-- name: TenantSignals :one
-- What the tenant has actually connected, in one round trip: the source list,
-- the effort ladder and the "no data yet" copy all derive from these counters
-- rather than from a static list that says "Site checks" to an account with no
-- checks at all. Log volume is deliberately NOT here: tenant_line_ledger is not
-- written yet, so it would answer "no lines" to a project that is streaming.
-- The line count comes from ring.QueryBuilder.Summary instead, which is why this
-- row carries the project id and its cutoff.
SELECT
  (SELECT count(*)::int FROM monitor m WHERE m.tenant_id = $1) AS monitor_count,
  (SELECT count(*)::int FROM monitor m JOIN monitor_facts f ON f.monitor_id = m.id
    WHERE m.tenant_id = $1 AND f.status = 'down') AS monitors_down,
  (SELECT max(f.last_check_at)::timestamptz FROM monitor m JOIN monitor_facts f ON f.monitor_id = m.id
    WHERE m.tenant_id = $1) AS last_check_at,
  (SELECT p.id FROM project p WHERE p.tenant_id = $1 ORDER BY p.id LIMIT 1) AS project_id,
  (SELECT coalesce(pw.cutoff_seq, 0) FROM project p
     LEFT JOIN project_window pw ON pw.project_id = p.id
    WHERE p.tenant_id = $1 ORDER BY p.id LIMIT 1)::bigint AS cutoff_seq,
  (SELECT count(*)::int FROM alert_channel c WHERE c.tenant_id = $1) AS channel_count;

-- name: ListSourceConnections :many
-- Sources the tenant connected by hand (deploy hooks, receivers). The built-in
-- two — site checks and app logs — are not rows here: they are facts derived
-- from what has arrived, and there is nothing to disconnect. hook_token is the
-- connection's inbound URL (universal hooks): the front renders it, so it
-- travels with the row rather than through a second endpoint. Drafts are the
-- token's storage for a panel someone opened to look — hidden until the first
-- event promotes them, because looking must not leave a connection card behind.
SELECT id, kind, status, last_signal_at, paused, hook_token, last_event
  FROM source_connection WHERE tenant_id = $1 AND status != 'draft' ORDER BY id;

-- name: SetSourcePaused :exec
UPDATE source_connection SET paused = $1 WHERE id = $2 AND tenant_id = $3;
