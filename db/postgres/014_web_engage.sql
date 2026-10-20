-- +goose Up
-- +goose StatementBegin
-- Engaged time, an hourly rollup beside web_heat rather than a kind inside it:
-- web_heat keeps only a plan's busiest pages and folds weeks, and an average
-- read over three pages, or a 31-day line in weekly points, would be a wrong
-- number rather than a coarse one. One row is one path on one device in one
-- UTC hour. views counts the page views the door stored; ms adds what each
-- heat beacon reports as engaged since the previous one (the tab visible and
-- input in the last 30 s); r10..r300 count the views that reached 10, 30, 120
-- and 300 engaged seconds, each sent once a view, so reach is exact rather
-- than read off an average. A heat beacon lands in its own hour, which may be
-- the hour after its view. ucworker's history-trim drops rows past the web
-- depth, like web_heat.
CREATE TABLE web_engage (
  tenant_id     bigint NOT NULL,
  project_id    bigint NOT NULL,
  hour          timestamptz NOT NULL,
  path          text NOT NULL,
  device        text NOT NULL,
  views         bigint NOT NULL DEFAULT 0,
  ms            bigint NOT NULL DEFAULT 0,
  r10           bigint NOT NULL DEFAULT 0,
  r30           bigint NOT NULL DEFAULT 0,
  r120          bigint NOT NULL DEFAULT 0,
  r300          bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, project_id, hour, path, device)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS web_engage;
-- +goose StatementEnd
