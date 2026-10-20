-- +goose Up
-- +goose StatementBegin
-- Segments: what the site says about a visitor (member, anon, trial, new),
-- set by data-segment on the tag or upcontrol('segment', name), and the kind
-- of source the page load came from (search, social, campaign, referral,
-- direct), worked out by the script. Neither is an identity: a segment is one
-- of at most four short names the project declared (PUT /v1/web/segments),
-- and a source is a fixed class. '' is "not marked" on both, which every read without a filter
-- still counts.
--
-- Both join the heat key and the engage key. The compaction's cell caps rank
-- per path, device, day and kind across every segment and source together, so
-- a completed day costs no more rows than before; only the current day grows.
ALTER TABLE web_heat ADD COLUMN segment text NOT NULL DEFAULT '';
ALTER TABLE web_heat ADD COLUMN source text NOT NULL DEFAULT '';
ALTER TABLE web_heat DROP CONSTRAINT web_heat_pkey;
ALTER TABLE web_heat ADD PRIMARY KEY
  (tenant_id, project_id, path, device, day, kind, segment, source, selector, fx, fy);

ALTER TABLE web_engage ADD COLUMN segment text NOT NULL DEFAULT '';
ALTER TABLE web_engage ADD COLUMN source text NOT NULL DEFAULT '';
ALTER TABLE web_engage DROP CONSTRAINT web_engage_pkey;
ALTER TABLE web_engage ADD PRIMARY KEY (tenant_id, project_id, hour, path, device, segment, source);

-- A project's declared segments, at most four: the cap is what keeps a
-- segment from becoming a user id. Keyed by project, which a claim keeps.
CREATE TABLE web_segment (
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  name          text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, name)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS web_segment;
DELETE FROM web_engage WHERE segment <> '' OR source <> '';
ALTER TABLE web_engage DROP CONSTRAINT web_engage_pkey;
ALTER TABLE web_engage DROP COLUMN source;
ALTER TABLE web_engage DROP COLUMN segment;
ALTER TABLE web_engage ADD PRIMARY KEY (tenant_id, project_id, hour, path, device);
DELETE FROM web_heat WHERE segment <> '' OR source <> '';
ALTER TABLE web_heat DROP CONSTRAINT web_heat_pkey;
ALTER TABLE web_heat DROP COLUMN source;
ALTER TABLE web_heat DROP COLUMN segment;
ALTER TABLE web_heat ADD PRIMARY KEY (tenant_id, project_id, path, device, day, kind, selector, fx, fy);
-- +goose StatementEnd
