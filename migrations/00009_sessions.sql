-- +goose Up
CREATE TABLE sessions (
  id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id             uuid        NOT NULL REFERENCES users(id),
  refresh_token_hash  text        NOT NULL,   -- SHA-256 of the current refresh token, never the token itself
  previous_token_hash text,                   -- the token before the last rotation, for reuse detection
  user_agent          text,
  ip                  inet,
  last_used_at        timestamptz NOT NULL DEFAULT now(),
  expires_at          timestamptz NOT NULL,
  revoked_at          timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT sessions_refresh_hash_uniq UNIQUE (refresh_token_hash)
);
CREATE INDEX sessions_user_active_idx ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_previous_hash_idx ON sessions (previous_token_hash)
  WHERE previous_token_hash IS NOT NULL;
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- +goose Down
DROP TABLE sessions;