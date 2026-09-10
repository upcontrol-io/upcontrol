-- name: GetPlanEntitlement :one
SELECT * FROM plan_entitlement WHERE plan = $1;

-- name: CountMonitors :one
-- Same axis as CountMonitorsByTenant, and it must stay the same predicate: this
-- one is the number the sidebar and the Plan page print, that one is the gate.
SELECT count(*)::int FROM monitor WHERE tenant_id = $1 AND kind <> 'heartbeat';

-- name: ListChannelsByTenant :many
-- Every channel in the workspace, whichever project owns it: the export is
-- tenant-wide, the Alerts screen reads ListChannelsByProject instead.
SELECT id, public_id, kind, target, notify, breaker_open_until, created_at,
       muted_until, label, recipient_person_id, project_id
  FROM alert_channel WHERE tenant_id = $1 ORDER BY created_at;

-- name: ListChannelsByProject :many
SELECT id, public_id, kind, target, notify, breaker_open_until, created_at,
       muted_until, label, recipient_person_id, project_id
  FROM alert_channel WHERE project_id = $1 ORDER BY created_at;

-- name: GetTenantPlan :one
SELECT plan FROM tenant WHERE id = $1;

-- name: ListRecipientsByProject :many
-- The project's team. The workspace owner is not a row here: handlers prepend
-- it from TenantOwner, because ownership is a column on the workspace.
SELECT m.role, m.status, p.id, p.public_id, p.email, p.name, p.telegram_id, p.telegram_username
  FROM project_member m
  JOIN person p ON p.id = m.person_id
 WHERE m.project_id = $1
 ORDER BY m.status, p.name;

-- name: ListRecipientsByTenant :many
-- Every person in the workspace, for the takeout only: the owner (a column on
-- the tenant, never a membership row) plus the members of its projects. The
-- screens read ListRecipientsByProject — this one is the export's, so the same
-- person in two projects is one row.
SELECT role, status, email FROM (
  SELECT 'login'::text AS role, 'active'::text AS status, p.email
    FROM tenant t JOIN person p ON p.id = t.owner_person_id
   WHERE t.id = sqlc.arg(tenant_id)
  UNION
  SELECT m.role, m.status, p.email
    FROM project_member m JOIN person p ON p.id = m.person_id
   WHERE m.tenant_id = sqlc.arg(tenant_id)
) x ORDER BY email;

-- name: ListIncidentsByTenant :many
-- tenant_id is selected back so incidentWithEvidence can scope the events read
-- to the same tenant without a second lookup. since_days is the plan's incident
-- window (plan_entitlement.incident_days): closed incidents past it are hidden,
-- an OPEN incident always shows, and 0 means no clamp (the export takeout).
-- Hidden, not deleted — an upgrade instantly restores the history; the worker's
-- purge deletes only past the widest plan's window.
SELECT id, tenant_id, project_id, public_id, title, status, detected_at, resolved_at, affected_count, close_reason
  FROM incident
 WHERE tenant_id = sqlc.arg(tenant_id)
   AND (resolved_at IS NULL
        OR sqlc.arg(since_days)::int <= 0
        OR detected_at >= now() - make_interval(days => sqlc.arg(since_days)::int))
 ORDER BY detected_at DESC LIMIT sqlc.arg(row_limit);

-- name: ListIncidentsByProject :many
-- The same window as ListIncidentsByTenant, narrowed to one project: the
-- Incidents screen reads this, the export reads the tenant-wide one.
SELECT id, tenant_id, project_id, public_id, title, status, detected_at, resolved_at, affected_count, close_reason
  FROM incident
 WHERE tenant_id = sqlc.arg(tenant_id)
   AND project_id = sqlc.arg(project_id)
   AND (resolved_at IS NULL
        OR sqlc.arg(since_days)::int <= 0
        OR detected_at >= now() - make_interval(days => sqlc.arg(since_days)::int))
 ORDER BY detected_at DESC LIMIT sqlc.arg(row_limit);

-- name: GetAPIKeyForProject :one
SELECT id, prefix, name, state, created_at, last_used_at, revoked_at, kind, origins
  FROM api_key WHERE project_id = $1 AND state != 'revoked' ORDER BY created_at DESC LIMIT 1;

-- name: ListKeyUsage :many
-- Scoped through the key, since key_usage_log carries only the tenant.
SELECT l.at, l.source, l.outcome FROM key_usage_log l
  JOIN api_key k ON k.id = l.key_id
  WHERE k.project_id = $1 ORDER BY l.at DESC LIMIT 10;

-- name: ProjectSignals :one
-- What the project has actually connected, in one round trip: the source list,
-- the effort ladder and the "no data yet" copy all derive from these counters
-- rather than from a static list that says "Site checks" to a project with no
-- checks at all. Log volume is deliberately NOT here: it would answer "no lines"
-- to a project that is streaming. The line count comes from
-- ring.QueryBuilder.Summary instead, which is why this row carries the project id.
SELECT
  (SELECT count(*)::int FROM monitor m WHERE m.project_id = sqlc.arg(project_id)) AS monitor_count,
  (SELECT count(*)::int FROM monitor m JOIN target_facts f ON f.target_id = m.target_id
    WHERE m.project_id = sqlc.arg(project_id) AND f.status = 'down') AS monitors_down,
  (SELECT max(f.last_check_at)::timestamptz FROM monitor m JOIN target_facts f ON f.target_id = m.target_id
    WHERE m.project_id = sqlc.arg(project_id)) AS last_check_at,
  sqlc.arg(project_id)::bigint AS project_id,
  (SELECT count(*)::int FROM alert_channel c WHERE c.project_id = sqlc.arg(project_id)) AS channel_count;

-- name: ListSourceConnections :many
-- Sources the project connected by hand (deploy hooks, receivers). The built-in
-- two — site checks and app logs — are not rows here: they are facts derived
-- from what has arrived, and there is nothing to disconnect. hook_token is the
-- connection's inbound URL (universal hooks): the front renders it, so it
-- travels with the row rather than through a second endpoint. Drafts are the
-- token's storage for a panel someone opened to look — hidden until the first
-- event promotes them, because looking must not leave a connection card behind.
SELECT id, kind, status, last_signal_at, paused, hook_token, last_event
  FROM source_connection WHERE project_id = $1 AND status != 'draft' ORDER BY id;

-- name: SetSourcePaused :exec
UPDATE source_connection SET paused = $1 WHERE id = $2 AND tenant_id = $3;
