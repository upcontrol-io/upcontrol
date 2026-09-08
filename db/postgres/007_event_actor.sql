-- +goose Up
-- +goose StatementBegin
-- Who did it. An event has always carried what happened and with what labels, but never a
-- person, so every people-shaped answer had to be computed on the client and shipped as a
-- finished reading. One column moves all four feeds onto the server and makes them reachable
-- from any language that can POST an event.
--
-- Empty string rather than NULL: "no actor" is a real answer — a server-side event with nobody
-- behind it — and it keeps the two readings honestly apart. count(*) is events;
-- count(DISTINCT actor) FILTER (WHERE actor <> '') is people.
ALTER TABLE events ADD COLUMN actor text NOT NULL DEFAULT '';

-- The funnel and breakdown reads: one event name over a window.
CREATE INDEX events_people_by_name ON events (tenant_id, project_id, name, ts) WHERE actor <> '';
-- The retention read: an actor's whole history, to find the week they were first seen.
CREATE INDEX events_people_by_actor ON events (tenant_id, project_id, actor, ts) WHERE actor <> '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS events_people_by_actor;
DROP INDEX IF EXISTS events_people_by_name;
ALTER TABLE events DROP COLUMN IF EXISTS actor;
-- +goose StatementEnd
