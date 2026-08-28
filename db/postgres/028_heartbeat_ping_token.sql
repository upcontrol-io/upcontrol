-- +goose Up
-- +goose StatementBegin
-- Heartbeat monitors created before the ping route existed: mint their token
-- and open a first window, or every one of them is "missed" a minute after
-- this deploy, before anyone has seen the URL.
UPDATE monitor
   SET ping_token = replace(gen_random_uuid()::text, '-', '')
 WHERE kind = 'heartbeat' AND ping_token IS NULL;

UPDATE monitor_schedule ms
   SET next_due_at = now() + make_interval(secs => m.interval_sec + COALESCE(m.grace_sec, m.interval_sec)),
       leased_by = NULL, lease_until = NULL
  FROM monitor m
 WHERE m.id = ms.monitor_id AND m.kind = 'heartbeat';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- data-only, nothing to reverse
-- +goose StatementEnd
