-- name: CreateEvent :one
INSERT INTO events (organizer_id, title, description, venue_name, starts_at, ends_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetEvent :one
SELECT * FROM events WHERE id = $1;

-- name: UpdateEvent :one
-- Partial update: a NULL argument leaves that column unchanged.
-- Only draft and published events are editable.
UPDATE events SET
  title       = COALESCE(sqlc.narg(title), title),
  description = COALESCE(sqlc.narg(description), description),
  venue_name  = COALESCE(sqlc.narg(venue_name), venue_name),
  starts_at   = COALESCE(sqlc.narg(starts_at), starts_at),
  ends_at     = COALESCE(sqlc.narg(ends_at), ends_at),
  updated_at  = now()
WHERE id = sqlc.arg(id)
  AND status IN ('draft', 'published')
RETURNING *;

-- name: TransitionEventStatus :one
-- Compare-and-swap: succeeds only if the event is still in from_status.
UPDATE events
SET status = sqlc.arg(to_status), updated_at = now()
WHERE id = sqlc.arg(id) AND status = sqlc.arg(from_status)
RETURNING *;