-- name: CreateUser :one
INSERT INTO users (email, password_hash, full_name)
VALUES ($1, $2, $3)
RETURNING id, email, full_name, role, created_at;


-- name: GetUserByEmail :one
SELECT id, email, password_hash, full_name, role, created_at
FROM users
WHERE email = $1;

-- name: GetUserByID :one
SELECT id, email, full_name, role, created_at
FROM users
WHERE id = $1;

-- name: UpdateUserPasswordHash :exec
UPDATE users
SET password_hash = $2, updated_at = now()
WHERE id = $1;