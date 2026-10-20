-- +goose NO TRANSACTION
-- +goose Up
-- Product alerts read one event's rows over a window every five minutes, actor
-- or not, and the only index that served them was (tenant_id, project_id, ts):
-- every row of the window was fetched just to test its name, page views
-- included. Built concurrently, so a busy events table keeps taking writes. A
-- failed concurrent build leaves an INVALID index that IF NOT EXISTS would skip
-- on the retry, so the retry drops it first. The new index covers the partial
-- one on the same columns (WHERE actor <> ''), which only costs writes now.
DROP INDEX CONCURRENTLY IF EXISTS events_by_name;
CREATE INDEX CONCURRENTLY events_by_name ON events (tenant_id, project_id, name, ts);
DROP INDEX CONCURRENTLY IF EXISTS events_people_by_name;

-- +goose Down
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_people_by_name ON events (tenant_id, project_id, name, ts) WHERE actor <> '';
DROP INDEX CONCURRENTLY IF EXISTS events_by_name;
