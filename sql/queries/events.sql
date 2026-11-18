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

-- name: ListManagedEvents :many
-- Events the user owns or staffs, newest start first. Keyset pagination:
-- pass the last row's (starts_at, id) as after_*; leave them NULL for page one.
SELECT sqlc.embed(e),
       COALESCE(e.organizer_id = sqlc.arg(user_id)::uuid, false)::boolean AS is_owner,
       COALESCE(s.role, '')::text                                         AS staff_role
FROM events e
LEFT JOIN event_staff s
  ON s.event_id = e.id AND s.user_id = sqlc.arg(user_id)::uuid
WHERE (e.organizer_id = sqlc.arg(user_id)::uuid OR s.user_id IS NOT NULL)
  AND (sqlc.narg(after_starts_at)::timestamptz IS NULL
       OR (e.starts_at, e.id) < (sqlc.narg(after_starts_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY e.starts_at DESC, e.id DESC
LIMIT sqlc.arg(page_size)::int;

-- name: CountTicketTypes :one
SELECT count(*) FROM ticket_types WHERE event_id = $1;

-- name: PublishEvent :one
-- Atomic: publishes only if the event is still a draft, still starts in the
-- future, and has at least one ticket type, all judged inside this statement.
UPDATE events
SET status = 'published', updated_at = now()
WHERE events.id = $1
  AND events.status = 'draft'
  AND events.starts_at > now()
  AND EXISTS (SELECT 1 FROM ticket_types t WHERE t.event_id = events.id)
RETURNING *;

-- name: CancelEvent :one
UPDATE events
SET status = 'cancelled', updated_at = now()
WHERE id = $1 AND status IN ('draft', 'published')
RETURNING *;