-- +goose Up
-- +goose StatementBegin
-- Idempotency must return the SAME accepted count on replay (plan §3.8: "a
-- retry of the same batch gets the same accepted"). ingest_batch already
-- dedupes on batch_key;
-- this adds the count it returns so a replay's receipt matches the first.
ALTER TABLE ingest_batch ADD COLUMN accepted int NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE ingest_batch DROP COLUMN accepted;
-- +goose StatementEnd
