-- +goose Up
CREATE INDEX events_organizer_starts_idx ON events (organizer_id, starts_at DESC, id DESC);

-- +goose Down
DROP INDEX events_organizer_starts_idx;