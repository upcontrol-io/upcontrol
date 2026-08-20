-- +goose Up
-- +goose StatementBegin
-- Until Aug 14, 2026 every account's project was created with a hardcoded
-- "example.com", whatever site its owner had actually asked us to watch — so
-- the Projects tab, the sidebar and the delete-confirmation prompt all named a
-- domain the customer had never typed. Provisioning now passes the checked host;
-- this repairs the rows created before it did.
--
-- Only rows that still hold the placeholder are touched, and only when the
-- account's earliest check points somewhere else — an account that genuinely
-- watches example.com keeps its name. `split_part` peels the scheme and the
-- path off the monitor's target, matching what bareHost() does in Go.
UPDATE project p
   SET domain = sub.host
  FROM (
    SELECT DISTINCT ON (m.project_id)
           m.project_id,
           split_part(split_part(regexp_replace(m.target, '^https?://', ''), '/', 1), ':', 1) AS host
      FROM monitor m
     WHERE m.kind <> 'heartbeat'
     ORDER BY m.project_id, m.id
  ) AS sub
 WHERE p.id = sub.project_id
   AND p.domain = 'example.com'
   AND sub.host <> ''
   AND sub.host <> 'example.com';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Irreversible by design: the old value was a placeholder identical for every
-- account, so there is nothing to restore it from. Down is a no-op rather than
-- a rename back to "example.com", which would re-break every repaired row.
SELECT 1;
-- +goose StatementEnd
