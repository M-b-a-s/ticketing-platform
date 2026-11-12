-- +goose Up
CREATE TABLE webhook_events (
  id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  provider    text        NOT NULL DEFAULT 'paystack',
  event_type  text        NOT NULL,              -- e.g. 'charge.success'
  dedupe_key  text        NOT NULL,              -- computed by the app, see below
  payload     jsonb       NOT NULL,
  status      text        NOT NULL DEFAULT 'received',
  attempts    integer     NOT NULL DEFAULT 0,
  last_error  text,
  processed_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT webhook_events_provider_dedupe_uniq UNIQUE (provider, dedupe_key),
  CONSTRAINT webhook_events_status_valid
    CHECK (status IN ('received','processed','failed','ignored')),
  CONSTRAINT webhook_events_attempts_nonneg CHECK (attempts >= 0)
);

-- The sweeper's query: anything not finished, oldest first.
CREATE INDEX webhook_events_unfinished_idx
  ON webhook_events (created_at) WHERE status IN ('received','failed');

-- +goose Down
DROP TABLE webhook_events;