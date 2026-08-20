-- +goose Up
-- +goose StatementBegin
-- The Grafana receiver was offered as a connect tile but never existed:
-- internal/source/webhook verifies stripe, github and vercel only, so a POST to
-- /hooks/grafana had nowhere to land. Pressing the tile still wrote a
-- source_connection row, which then sat on the Sources screen as a feed that
-- could not receive anything. The tile is gone (Aug 14, 2026, user decision);
-- these rows go with it, or the screen keeps a card nothing can ever update.
DELETE FROM source_connection WHERE kind = 'grafana';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Nothing to restore: the rows described a connection that never worked.
SELECT 1;
-- +goose StatementEnd
