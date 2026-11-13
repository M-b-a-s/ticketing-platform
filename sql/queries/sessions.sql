-- name: CreateSession :one
INSERT INTO sessions (user_id, refresh_token_hash, user_agent, ip, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, user_id, expires_at;

-- name: RotateSession :one
-- Atomic: only one concurrent caller can match the current hash.
UPDATE sessions
SET previous_token_hash = refresh_token_hash,
    refresh_token_hash  = $2,
    last_used_at        = now(),
    updated_at          = now()
WHERE refresh_token_hash = $1
  AND revoked_at IS NULL
  AND expires_at > now()
RETURNING id, user_id, expires_at;

-- name: GetSessionByPreviousHash :one
SELECT id, user_id
FROM sessions
WHERE previous_token_hash = $1
  AND revoked_at IS NULL;

-- name: RevokeSession :exec
UPDATE sessions
SET revoked_at = now(), updated_at = now()
WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeSessionByRefreshHash :exec
UPDATE sessions
SET revoked_at = now(), updated_at = now()
WHERE refresh_token_hash = $1 AND revoked_at IS NULL;