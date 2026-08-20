-- +goose Up
-- +goose StatementBegin
-- Check names created before Aug 14, 2026 were built from the address the
-- visitor typed rather than from the target, so a pasted "datrade.io/" produced
-- "datrade.io//pricing", and every subdomain discovery found (api., app.) was
-- labelled with the root host — a status page listing three components all
-- called "harpa.ai". These names are published, so they are worth repairing.
--
-- Derived exactly as monitorName() now does: the target's own host, plus its
-- path when the path is not the root. Website checks only — a heartbeat's name
-- is its owner's word, not a URL.
UPDATE monitor
   SET name = regexp_replace(
                rtrim(regexp_replace(target, '^https?://', ''), '/'),
                '\?.*$', ''
              )
 WHERE kind <> 'heartbeat'
   AND target ~ '^https?://'
   AND name <> regexp_replace(
                 rtrim(regexp_replace(target, '^https?://', ''), '/'),
                 '\?.*$', ''
               );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The old names were derived from an input we no longer hold, so there is
-- nothing to restore them from.
SELECT 1;
-- +goose StatementEnd
