-- Availability incidents whose monitor was deleted were left open forever:
-- incident.monitor_id is ON DELETE SET NULL, and until now nothing closed the
-- incident when the check went away. A public status page therefore kept
-- announcing "Some systems are down" for a component it no longer even lists,
-- and /app kept an incident that could never resolve. The code fix closes them
-- at delete time; this closes the ones already stranded.
--
-- Detector incidents legitimately carry a NULL monitor_id (they are
-- project-scoped), so the detector filter is what keeps this migration off
-- them.

-- +goose Up
-- +goose StatementBegin
UPDATE incident
   SET resolved_at   = now(),
       status        = 'ok',
       close_reason  = 'monitor_deleted'
 WHERE resolved_at IS NULL
   AND monitor_id IS NULL
   AND detector = 'availability';
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO incident_update (incident_id, kind, text)
SELECT id, 'resolved', 'Monitor deleted'
  FROM incident
 WHERE close_reason = 'monitor_deleted'
   AND detector = 'availability'
   AND NOT EXISTS (
         SELECT 1 FROM incident_update u
          WHERE u.incident_id = incident.id AND u.kind = 'resolved'
       );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Irreversible by design: which of these were closed by this migration and
-- which by the delete path is not recorded, and reopening a resolved incident
-- would re-raise an alarm about a check that no longer exists.
SELECT 1;
-- +goose StatementEnd
