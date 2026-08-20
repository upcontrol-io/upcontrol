-- Analytics visitor directory (plan: product-analytics §Decision 6, T1).
-- Written only by the recorder's flush loop (internal/analytics): first-touch
-- columns exactly once via ON CONFLICT DO NOTHING, identity and last_* per
-- flush. token_hash is sha256(uc_vid); the raw cookie never reaches SQL.

-- name: UpsertWebVisitorFirst :one
-- First-touch attribution (§Decision 9): the first non-empty referrer/UTM
-- wins and is never overwritten. A conflict-side DO UPDATE that fills only
-- still-empty columns — NOT DO NOTHING: a server-side event (public_check_run
-- fires instantly; the browser's first page_view flushes ~1.5 s later) can
-- create the row before the batch that carries the referrer arrives, and a
-- one-shot insert would burn the attribution on empty strings forever.
-- RETURNING id covers both branches, so no read-back round trip is needed.
INSERT INTO web_visitor (token_hash, first_seen_at, last_seen_at,
                         first_referrer, first_utm_source, first_utm_medium,
                         first_utm_campaign, first_country, first_device,
                         first_path, is_bot)
VALUES (sqlc.arg(token_hash), sqlc.arg(first_seen_at), sqlc.arg(first_seen_at),
        sqlc.arg(first_referrer), sqlc.arg(first_utm_source), sqlc.arg(first_utm_medium),
        sqlc.arg(first_utm_campaign), sqlc.arg(first_country), sqlc.arg(first_device),
        sqlc.arg(first_path), sqlc.arg(is_bot))
ON CONFLICT (token_hash) DO UPDATE SET
    first_referrer    = CASE WHEN web_visitor.first_referrer = ''    THEN EXCLUDED.first_referrer    ELSE web_visitor.first_referrer END,
    first_utm_source  = CASE WHEN web_visitor.first_utm_source = ''  THEN EXCLUDED.first_utm_source  ELSE web_visitor.first_utm_source END,
    first_utm_medium  = CASE WHEN web_visitor.first_utm_medium = ''  THEN EXCLUDED.first_utm_medium  ELSE web_visitor.first_utm_medium END,
    first_utm_campaign = CASE WHEN web_visitor.first_utm_campaign = '' THEN EXCLUDED.first_utm_campaign ELSE web_visitor.first_utm_campaign END,
    first_country     = CASE WHEN web_visitor.first_country = ''     THEN EXCLUDED.first_country     ELSE web_visitor.first_country END,
    first_device      = CASE WHEN web_visitor.first_device = ''      THEN EXCLUDED.first_device      ELSE web_visitor.first_device END,
    first_path        = CASE WHEN web_visitor.first_path = ''        THEN EXCLUDED.first_path        ELSE web_visitor.first_path END,
    is_bot            = web_visitor.is_bot AND EXCLUDED.is_bot
RETURNING id;

-- name: LinkVisitorEmail :exec
-- watch_signup: the email goes ONLY to this Postgres row, never into
-- ClickHouse props (§Decision 7). Last write wins — a newer verified address
-- is better data than the first one typed.
UPDATE web_visitor SET email = sqlc.arg(email) WHERE id = sqlc.arg(id);

-- name: LinkVisitorPerson :exec
-- signed_in: person/tenant last-wins, signed_in_at first-wins (the funnel
-- step is the first occurrence).
UPDATE web_visitor
   SET person_id = sqlc.arg(person_id),
       tenant_id = sqlc.arg(tenant_id),
       signed_in_at = COALESCE(signed_in_at, sqlc.arg(signed_in_at))
 WHERE id = sqlc.arg(id);

-- name: MarkVisitorAccountCreated :exec
UPDATE web_visitor
   SET account_created_at = COALESCE(account_created_at, sqlc.arg(account_created_at))
 WHERE id = sqlc.arg(id);

-- name: TouchVisitorLastSeen :exec
-- Per recorder flush: last_seen (sessionization input), latest country/device,
-- and the events counter that feeds the visitors directory.
UPDATE web_visitor
   SET last_seen_at = sqlc.arg(last_seen_at),
       last_country = sqlc.arg(last_country),
       last_device = sqlc.arg(last_device),
       events_count = events_count + sqlc.arg(n_events)
 WHERE id = sqlc.arg(id);
