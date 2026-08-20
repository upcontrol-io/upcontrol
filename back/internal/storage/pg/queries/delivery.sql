-- name: LeasePendingDeliveries :many
-- Atomically pick up pending items whose next_try_at has arrived. Sets the
-- lease so no other worker picks up the same item (invariant 5/6).
UPDATE delivery_queue
   SET leased_by = $1, lease_until = $2
 WHERE id IN (
   SELECT id FROM delivery_queue
   WHERE state = 'pending'
     AND leased_by IS NULL
     AND next_try_at <= now()
   ORDER BY next_try_at
   LIMIT $3
   FOR UPDATE SKIP LOCKED
 )
RETURNING id, tenant_id, incident_id, channel_id, class, payload, attempts;

-- name: GetChannelForDelivery :one
SELECT ac.id, ac.public_id, ac.kind, ac.target, ac.breaker_open_until,
       ac.recipient_person_id, ac.muted_until
  FROM alert_channel ac
 WHERE ac.id = $1;

-- name: GetEmailChannelTarget :one
-- The backup address a channel fails over to when its own breaker trips. NULL
-- if the tenant has no email channel (the delivery then goes dead).
SELECT target FROM alert_channel WHERE tenant_id = $1 AND kind = 'email' ORDER BY created_at LIMIT 1;

-- name: RecordDeliveryAttempt :exec
INSERT INTO delivery_attempt (queue_id, outcome, detail)
VALUES ($1, $2, $3);

-- name: MarkDelivered :exec
UPDATE delivery_queue
   SET state = 'sent', leased_by = NULL, lease_until = NULL, attempts = attempts + 1
 WHERE id = $1;

-- name: MarkDead :exec
UPDATE delivery_queue
   SET state = 'dead', dead_reason = $1, leased_by = NULL, lease_until = NULL, attempts = attempts + 1
 WHERE id = $2;

-- name: Reschedule :exec
UPDATE delivery_queue
   SET leased_by = NULL, lease_until = NULL, attempts = attempts + 1, next_try_at = $1
 WHERE id = $2;

-- name: SetChannelBreaker :exec
UPDATE alert_channel SET breaker_open_until = $1 WHERE id = $2;

-- name: EnqueueDelivery :exec
-- Called by the incident lifecycle when an alert should go out.
INSERT INTO delivery_queue (tenant_id, incident_id, channel_id, idem_key, class, payload)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (idem_key) DO NOTHING;

-- name: EnqueueDeliveryAt :exec
-- Same, but scheduled: the 15-minute resolve follow-up is enqueued at incident
-- open with next_try_at in the future, and COMPOSED at send time from the
-- incident's then-current state — composing now would freeze an answer to a
-- question that has not been asked yet.
INSERT INTO delivery_queue (tenant_id, incident_id, channel_id, idem_key, class, payload, next_try_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (idem_key) DO NOTHING;

-- name: GetIncidentForFollowUp :one
-- The follow-up's facts, read at send time: still open → "still down",
-- resolved → "recovered".
SELECT i.status, i.title, i.public_id, coalesce(m.name, '') AS monitor_name
  FROM incident i LEFT JOIN monitor m ON m.id = i.monitor_id
 WHERE i.id = $1;
