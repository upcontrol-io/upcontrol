-- +goose Up
-- +goose StatementBegin
-- Status pages created before Aug 14, 2026 are addressed as /status/prj-12: our
-- own row id, handed to a customer's visitors. The slug is now the site's name
-- (harpa.ai -> harpa-ai), so this renames the pages that predate it.
--
-- Rewriting a slug is normally forbidden — it is a link somebody may already
-- have shared — but "prj-12" is an id nobody would have chosen to share, the
-- feature is days old, and leaving it means the format the product promises
-- exists only for accounts created after a certain Thursday.
--
-- Renamed only when the derived slug is free: the column is UNIQUE across
-- tenants, so the second page for a host keeps the id-shaped name rather than
-- failing the migration. `regexp_replace` mirrors slugFromHost() in Go:
-- lowercase, every run of non-alphanumerics to one dash, trimmed, max 40.
--
-- DISTINCT ON is not a tidiness flourish: several projects can derive the SAME
-- slug (every account created before the fix was called "example.com"), and
-- `NOT EXISTS` only sees the pre-statement snapshot, so without it the rows
-- collide with each other mid-UPDATE and the whole migration fails. One row per
-- name, oldest first; the rest keep their id-shaped slug.
UPDATE status_page s
   SET slug = cand.slug
  FROM (
    SELECT DISTINCT ON (slug) id, slug
      FROM (
        SELECT sp.id,
               left(
                 trim(both '-' from regexp_replace(lower(p.domain), '[^a-z0-9]+', '-', 'g')),
                 40
               ) AS slug
          FROM status_page sp
          JOIN project p ON p.id = sp.project_id
         WHERE sp.slug ~ '^prj-[0-9]+$'
      ) AS derived
     WHERE derived.slug <> ''
       AND NOT EXISTS (SELECT 1 FROM status_page t WHERE t.slug = derived.slug)
     ORDER BY slug, id
  ) AS cand
 WHERE s.id = cand.id;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The project id is still derivable, so this one CAN be undone: put every
-- page back on the id-shaped slug it was created with.
UPDATE status_page SET slug = 'prj-' || project_id::text WHERE slug !~ '^prj-[0-9]+$';
-- +goose StatementEnd
