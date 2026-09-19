-- +goose Up
-- +goose StatementBegin
-- The web door (POST /w, uc.js). A page view is an ordinary events row named
-- uc.pageview; everything else a page did lands here, aggregated. One row is
-- one element cell of one path on one device on one UTC day, and a beacon adds
-- into it, so mouse movement costs rows by distinct cells rather than by
-- moves, and none of it reaches the log ring a plan's window_lines counts.
--
-- kind: click | rage | move carry the element (selector) and the position in
-- it in 64ths (fx, fy); scroll carries no element, fy is the deepest reach in
-- twentieths (0..20) and n the page views that stopped there. device is the
-- viewport bucket (mobile | tablet | desktop), because the layout, and so the
-- map, follows the width. ucworker's history-trim drops rows past the plan's
-- history_days, like series_1h.
CREATE TABLE web_heat (
  tenant_id     bigint NOT NULL,
  project_id    bigint NOT NULL,
  day           date NOT NULL,
  path          text NOT NULL,
  device        text NOT NULL,
  kind          text NOT NULL,
  selector      text NOT NULL,
  fx            smallint NOT NULL,
  fy            smallint NOT NULL,
  n             bigint NOT NULL,
  PRIMARY KEY (tenant_id, project_id, path, device, day, kind, selector, fx, fy)
);

-- A heatmap link's token: read-only, one project, an hour. Stored as sha256
-- like install_token; the plaintext rides the page's URL fragment only.
CREATE TABLE heatmap_link (
  token_hash    bytea PRIMARY KEY,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  expires_at    timestamptz NOT NULL
);

-- The day's salt for a cookieless visitor: actor = hash(salt, project, address,
-- browser). A day's row is deleted once the day is over, which is what makes a
-- visitor impossible to follow from one day to the next, by us included.
CREATE TABLE web_salt (
  day           date PRIMARY KEY,
  salt          bytea NOT NULL
);

-- The retention grid reads people with the server-owned uc. names left out
-- (a page view's actor is a visitor hash that changes daily). Its own index
-- keeps that read off every page view the project ever received.
CREATE INDEX events_people_app_by_actor ON events (tenant_id, project_id, actor, ts)
  WHERE actor <> '' AND name NOT LIKE 'uc.%';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS events_people_app_by_actor;
DROP TABLE IF EXISTS web_salt;
DROP TABLE IF EXISTS heatmap_link;
DROP TABLE IF EXISTS web_heat;
-- +goose StatementEnd
