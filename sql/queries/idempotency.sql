-- name: ClaimIdempotencyKey :one
-- Succeeds (returns the new row's id) only for the FIRST request with this key; a
-- repeat gets no row back. If the first request is still running in another
-- transaction, this statement waits for it to commit or roll back before deciding.
INSERT INTO idempotency_keys (user_id, scope, key, request_hash)
VALUES (sqlc.arg(user_id), sqlc.arg(scope), sqlc.arg(key), sqlc.arg(request_hash))
ON CONFLICT (user_id, scope, key) DO NOTHING
RETURNING id;

-- name: GetIdempotencyKey :one
SELECT id, request_hash, response_status, response_body
FROM idempotency_keys
WHERE user_id = sqlc.arg(user_id) AND scope = sqlc.arg(scope) AND key = sqlc.arg(key);

-- name: CompleteIdempotencyKey :exec
UPDATE idempotency_keys
SET response_status = sqlc.arg(response_status)::int,
    response_body   = sqlc.arg(response_body)::jsonb,
    updated_at      = now()
WHERE id = sqlc.arg(id);