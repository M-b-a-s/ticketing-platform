-- name: CreateSession :one
INSERT INTO sessions (user_id, refresh_token_hash, user_agent, ip, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, user_id, expires_at;

-- name: RotateSession :one
-- Atomic: only one concurrent caller can match the current hash.
-- Returns the user's role so the new access token needs no extra query.
WITH rotated AS (
  UPDATE sessions
  SET previous_token_hash = refresh_token_hash,
      refresh_token_hash  = sqlc.arg(new_hash),
      last_used_at        = now(),
      updated_at          = now()
  WHERE refresh_token_hash = sqlc.arg(old_hash)
    AND revoked_at IS NULL
    AND expires_at > now()
  RETURNING id, user_id, expires_at
)
SELECT r.id, r.user_id, r.expires_at, u.role
FROM rotated r
JOIN users u ON u.id = r.user_id;

-- name: GetSessionByPreviousHash :one
SELECT id, user_id
FROM sessions
WHERE previous_token_hash = sqlc.arg(hash)::text
  AND revoked_at IS NULL;

-- name: RevokeSession :exec
UPDATE sessions
SET revoked_at = now(), updated_at = now()
WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeSessionByRefreshHash :exec
UPDATE sessions
SET revoked_at = now(), updated_at = now()
WHERE refresh_token_hash = $1 AND revoked_at IS NULL;