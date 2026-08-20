-- +goose Up
-- +goose StatementBegin
-- web_visitor is the Postgres half of product analytics (plan: product-analytics
-- §Decision 6): a directory of current visitor state — first-touch attribution,
-- identity, counters. ClickHouse web_events is the raw stream; this table is
-- what answers "who is this visitor". First-touch columns are written exactly
-- once (INSERT ... ON CONFLICT DO NOTHING in queries/analytics.sql); identity
-- and last_* columns are touched on every recorder flush. token_hash is
-- sha256(uc_vid cookie): the raw cookie never reaches the database.
CREATE TABLE web_visitor (
  id                 bigserial PRIMARY KEY,
  token_hash         bytea NOT NULL UNIQUE,
  first_seen_at      timestamptz NOT NULL DEFAULT now(),
  last_seen_at       timestamptz NOT NULL DEFAULT now(),
  first_referrer     text NOT NULL DEFAULT '',
  first_utm_source   text NOT NULL DEFAULT '',
  first_utm_medium   text NOT NULL DEFAULT '',
  first_utm_campaign text NOT NULL DEFAULT '',
  first_country      text NOT NULL DEFAULT '',
  first_device       text NOT NULL DEFAULT '',
  first_path         text NOT NULL DEFAULT '',
  last_country       text NOT NULL DEFAULT '',
  last_device        text NOT NULL DEFAULT '',
  email              text NOT NULL DEFAULT '',
  person_id          bigint REFERENCES person(id) ON DELETE SET NULL,
  tenant_id          bigint,
  signed_in_at       timestamptz,
  account_created_at timestamptz,
  events_count       bigint NOT NULL DEFAULT 0,
  is_bot             boolean NOT NULL DEFAULT false
);
-- Partial indexes: the two lookup paths that matter are "known visitors"
-- (person linked) and "reachable visitors" (email left); anonymous rows
-- neither query ever returns do not need index entries.
CREATE INDEX web_visitor_person_idx ON web_visitor (person_id) WHERE person_id IS NOT NULL;
CREATE INDEX web_visitor_email_idx ON web_visitor (email) WHERE email <> '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE web_visitor;
-- +goose StatementEnd
