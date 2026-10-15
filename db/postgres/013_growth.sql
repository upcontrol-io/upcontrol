-- +goose Up
-- +goose StatementBegin
-- What a sign-in was started for, kept with its code: the unclaimed status
-- page to claim, a CLI project's claim token, the /app path to land on. The
-- request stores them, the redeem answers them once as pendingClaim and
-- clears them, so a link opened on any device finishes the claim and nothing
-- claim-shaped ever rides a mail.
ALTER TABLE magic_link_code ADD COLUMN claim_slug text;
ALTER TABLE magic_link_code ADD COLUMN claim_token text;
ALTER TABLE magic_link_code ADD COLUMN claim_next text;

-- A project's first install_verified. The SDK sends one on every process
-- start, so this stamp is the only thing telling the first from a restart.
ALTER TABLE project ADD COLUMN install_verified_at timestamptz;
UPDATE project p SET install_verified_at = f.first
  FROM (SELECT project_id, min(ts) AS first FROM events
         WHERE name = 'install_verified' GROUP BY project_id) f
 WHERE f.project_id = p.id;

-- A workspace's first check. Checks are deleted outright, so an empty
-- workspace cannot tell its first check from one added after deleting every
-- check: this stamp does. A workspace already holding one is stamped with its
-- oldest.
ALTER TABLE tenant ADD COLUMN first_check_at timestamptz;
UPDATE tenant t SET first_check_at = f.first
  FROM (SELECT tenant_id, min(created_at) AS first FROM monitor GROUP BY tenant_id) f
 WHERE f.tenant_id = t.id;

-- account_created now fires at the first sign-in that proves the address, the
-- one that stamps email_verified_at. Whoever already signed in has proven it:
-- without this their next sign-in would count as a new account.
UPDATE person p SET email_verified_at = p.created_at
 WHERE p.email IS NOT NULL AND p.email_verified_at IS NULL
   AND (EXISTS (SELECT 1 FROM session s WHERE s.person_id = p.id)
        OR EXISTS (SELECT 1 FROM web_events e WHERE e.person_id = p.id AND e.name = 'signed_in'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The person stamps stay: email_verified_at predates this file.
ALTER TABLE tenant DROP COLUMN IF EXISTS first_check_at;
ALTER TABLE project DROP COLUMN IF EXISTS install_verified_at;
ALTER TABLE magic_link_code DROP COLUMN IF EXISTS claim_next;
ALTER TABLE magic_link_code DROP COLUMN IF EXISTS claim_token;
ALTER TABLE magic_link_code DROP COLUMN IF EXISTS claim_slug;
-- +goose StatementEnd
