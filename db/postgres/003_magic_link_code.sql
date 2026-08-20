-- +goose Up
-- +goose StatementBegin

-- One outstanding magic-link code per email. The code is stored as sha256(code)
-- (constant-time compared), single-use (redeemed_at), with an attempt cap and a
-- TTL (expires_at). A new request overwrites the row — the latest code wins, the
-- prior one is invalidated. Block 3: previously the code was generated and never
-- stored, so any token logged you in (and prod login was impossible).
CREATE TABLE magic_link_code (
    email text PRIMARY KEY,
    code_hash bytea NOT NULL,
    attempts int NOT NULL DEFAULT 0,
    expires_at timestamptz NOT NULL,
    redeemed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Sliding-window throttle for the anonymous magic-link endpoint, keyed by the
-- caller's IP. The endpoint sends mail, so it must be rate-limited per IP as
-- well as per email (the per-email cooldown is read off magic_link_code.created_at).
CREATE TABLE magic_link_ip (
    ip text PRIMARY KEY,
    first_at timestamptz NOT NULL DEFAULT now(),
    count int NOT NULL DEFAULT 1
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE magic_link_ip;
DROP TABLE magic_link_code;
-- +goose StatementEnd
