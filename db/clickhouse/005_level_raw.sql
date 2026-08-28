-- 005_level_raw.sql — the level as the client sent it (capped at 32 bytes at
-- ingest). `level` stays the normalized value every reader, index and rollup
-- groups by. Never edit an applied migration — add the next file.

ALTER TABLE logs ADD COLUMN IF NOT EXISTS level_raw String DEFAULT '' AFTER level;
