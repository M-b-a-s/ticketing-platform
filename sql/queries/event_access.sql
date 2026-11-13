-- name: GetEventAccess :one
-- One round trip: is the caller the owner, and what is their staff role (if any)?
-- No row means the event does not exist.
SELECT
  COALESCE(e.organizer_id = sqlc.arg(user_id)::uuid, false)::boolean AS is_owner,
  COALESCE(s.role, '')::text                                         AS staff_role
FROM events e
LEFT JOIN event_staff s
  ON s.event_id = e.id AND s.user_id = sqlc.arg(user_id)::uuid
WHERE e.id = sqlc.arg(event_id)::uuid;