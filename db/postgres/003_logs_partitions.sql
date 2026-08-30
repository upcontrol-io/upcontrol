-- +goose Up
-- +goose StatementBegin
-- 001 created logs_today and logs_tomorrow at install time and left rolling
-- them forward to a job that never landed. Once the calendar passed their
-- upper bound every flush failed with "no partition of relation logs found
-- for row": the batcher swaps a batch out before writing it and never retries,
-- so those lines were accepted with a 200 and then dropped on the floor.
-- Partitions are named after the UTC day they hold from here, which is what
-- lets ucworker's log-partitions job create ahead and drop behind. The two
-- install-time partitions go with their rows (owner decision, 2026-08-30).
DROP TABLE IF EXISTS logs_today;
DROP TABLE IF EXISTS logs_tomorrow;
-- Today and the next three days, so a fresh install can write immediately and
-- the hourly job has a full day of slack before the first one it must make.
DO $$
DECLARE
  d date := (now() AT TIME ZONE 'UTC')::date;
  i int;
BEGIN
  FOR i IN 0..3 LOOP
    EXECUTE format('CREATE TABLE IF NOT EXISTS %I PARTITION OF logs FOR VALUES FROM (%L) TO (%L)',
                   'logs_' || to_char(d + i, 'YYYYMMDD'),
                   (d + i)::timestamp AT TIME ZONE 'UTC',
                   (d + i + 1)::timestamp AT TIME ZONE 'UTC');
  END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Forward-only: the dated partitions stay. Recreating logs_today over a day
-- another partition already covers would fail, and the rows this dropped are
-- gone either way.
SELECT 1;
-- +goose StatementEnd
