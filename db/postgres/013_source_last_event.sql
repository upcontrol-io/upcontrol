-- +goose Up
-- +goose StatementBegin
-- The hook panel's receipt (Aug 14, 2026, user decision): when an event lands
-- on a connection's hook we show WHAT landed — "Received github_push · 2s ago"
-- — because the provider's own "send test webhook" button is the real tester,
-- and our job is to display the proof. One column, overwritten per event: the
-- receipt is the last message, not a log (the log is the events table).
ALTER TABLE source_connection ADD COLUMN last_event text;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE source_connection DROP COLUMN last_event;
-- +goose StatementEnd
