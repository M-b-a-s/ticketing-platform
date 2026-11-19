-- +goose Up
ALTER TABLE events ADD COLUMN published_at timestamptz;

-- events that are already public (dev data) keep working
UPDATE events SET published_at = updated_at WHERE status IN ('published', 'completed');

-- serves the public list: upcoming published events in start order
CREATE INDEX events_public_idx ON events (starts_at, id) WHERE status = 'published';

-- +goose Down
DROP INDEX events_public_idx;
ALTER TABLE events DROP COLUMN published_at;