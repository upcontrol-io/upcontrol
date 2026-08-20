-- Instance-level settings (023_instance_setting.sql): the self-host UI's
-- writable knobs, the first being the AI API key. Values arrive sealed
-- (AES-256-GCM under UC_SECRET_KEY_HEX) — nothing here sees plaintext.

-- name: GetInstanceSetting :one
SELECT value_enc FROM instance_setting WHERE key = $1;

-- name: UpsertInstanceSetting :exec
INSERT INTO instance_setting (key, value_enc, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (key) DO UPDATE SET value_enc = EXCLUDED.value_enc, updated_at = now();

-- name: DeleteInstanceSetting :exec
DELETE FROM instance_setting WHERE key = $1;
