-- name: UpsertMagicLinkCode :exec
-- Store a freshly generated code for an email, replacing any prior code and
-- resetting attempts/redeemed. expires_at is passed in (now + TTL at the caller).
-- The claim columns are replaced too: a code carries what ITS request was
-- started for, never an earlier request's.
INSERT INTO magic_link_code (email, code_hash, expires_at, claim_slug, claim_token, claim_next)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (email) DO UPDATE
   SET code_hash  = EXCLUDED.code_hash,
       attempts   = 0,
       expires_at = EXCLUDED.expires_at,
       redeemed_at = NULL,
       created_at = now(),
       claim_slug = EXCLUDED.claim_slug,
       claim_token = EXCLUDED.claim_token,
       claim_next = EXCLUDED.claim_next;

-- name: GetMagicLinkCode :one
SELECT email, code_hash, attempts, expires_at, redeemed_at, created_at,
       claim_slug, claim_token, claim_next
  FROM magic_link_code WHERE email = $1;

-- name: MarkMagicLinkRedeemed :execrows
-- One-time redemption: only the first redeem after issue succeeds. The claim
-- the request stored is answered by this redeem and goes with it.
UPDATE magic_link_code
   SET redeemed_at = now(), claim_slug = NULL, claim_token = NULL, claim_next = NULL
 WHERE email = $1 AND redeemed_at IS NULL;

-- name: IncMagicLinkAttempts :execrows
-- Record a failed verification so the attempt cap bites.
UPDATE magic_link_code SET attempts = attempts + 1 WHERE email = $1;

-- name: RecordMagicLinkIP :one
-- Sliding window: the count resets to 1 once 5 minutes pass without a request.
-- Returns the post-record count; the caller compares against the per-IP cap.
INSERT INTO magic_link_ip (ip, first_at, count) VALUES ($1, now(), 1)
ON CONFLICT (ip) DO UPDATE
   SET count    = CASE WHEN magic_link_ip.first_at > now() - interval '5 minutes'
                       THEN magic_link_ip.count + 1 ELSE 1 END,
       first_at = CASE WHEN magic_link_ip.first_at > now() - interval '5 minutes'
                       THEN magic_link_ip.first_at ELSE now() END
RETURNING count;
