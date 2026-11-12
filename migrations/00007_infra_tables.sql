-- +goose Up
CREATE TABLE idempotency_keys (
  id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         uuid        NOT NULL REFERENCES users(id),
  scope           text        NOT NULL,   -- e.g. 'create_order', 'init_payment'
  key             text        NOT NULL,
  request_hash    text        NOT NULL,   -- same key + different body => 422
  response_status integer,                -- null while the first request is still in flight
  response_body   jsonb,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT idempotency_keys_user_scope_key_uniq UNIQUE (user_id, scope, key)
);
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);

CREATE TABLE outbox_events (
  id             bigserial   PRIMARY KEY,
  aggregate_type text        NOT NULL,
  aggregate_id   uuid        NOT NULL,
  event_type     text        NOT NULL,    -- e.g. order.paid, ticket.issued
  payload        jsonb       NOT NULL,
  attempts       integer     NOT NULL DEFAULT 0,
  last_error     text,
  published_at   timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT outbox_events_attempts_nonneg CHECK (attempts >= 0)
);
CREATE INDEX outbox_unpublished_idx ON outbox_events (id) WHERE published_at IS NULL;

CREATE TABLE audit_logs (
  id          bigserial   PRIMARY KEY,
  actor_id    uuid        REFERENCES users(id),   -- null for system actions
  action      text        NOT NULL,
  entity_type text        NOT NULL,
  entity_id   text        NOT NULL,
  metadata    jsonb,
  ip          inet,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_entity_idx ON audit_logs (entity_type, entity_id, created_at);
CREATE INDEX audit_logs_actor_idx  ON audit_logs (actor_id, created_at);

-- +goose Down
DROP TABLE audit_logs;
DROP TABLE outbox_events;
DROP TABLE idempotency_keys;