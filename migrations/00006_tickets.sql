-- +goose Up

-- Lets tickets prove their event and ticket type match the order item they came from.
ALTER TABLE order_items
  ADD CONSTRAINT order_items_id_event_type_uniq UNIQUE (id, event_id, ticket_type_id);

CREATE TABLE tickets (
  id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  order_item_id  uuid        NOT NULL,
  seq            integer     NOT NULL,   -- 1..quantity within the order item
  ticket_type_id uuid        NOT NULL,
  event_id       uuid        NOT NULL,
  owner_id       uuid        NOT NULL REFERENCES users(id),
  code           text        NOT NULL,   -- random, unguessable token encoded in the QR
  status         text        NOT NULL DEFAULT 'valid',
  checked_in_at  timestamptz,
  checked_in_by  uuid        REFERENCES users(id),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT tickets_order_item_fk
    FOREIGN KEY (order_item_id, event_id, ticket_type_id)
    REFERENCES order_items (id, event_id, ticket_type_id),
  CONSTRAINT tickets_item_seq_uniq UNIQUE (order_item_id, seq),   -- idempotent issuance
  CONSTRAINT tickets_code_uniq     UNIQUE (code),
  CONSTRAINT tickets_seq_pos       CHECK (seq > 0),
  CONSTRAINT tickets_status_valid  CHECK (status IN ('valid','checked_in','cancelled','refunded')),
  CONSTRAINT tickets_checkin_pair  CHECK ((checked_in_at IS NULL) = (checked_in_by IS NULL)),
  CONSTRAINT tickets_checkin_time  CHECK (status <> 'checked_in' OR checked_in_at IS NOT NULL)
);
CREATE INDEX tickets_owner_idx        ON tickets (owner_id);
CREATE INDEX tickets_event_status_idx ON tickets (event_id, status);

CREATE TABLE check_in_attempts (
  id           bigserial   PRIMARY KEY,
  event_id     uuid        NOT NULL REFERENCES events(id),   -- the event being scanned for
  ticket_id    uuid        REFERENCES tickets(id),           -- null when the code matched nothing
  attendant_id uuid        NOT NULL REFERENCES users(id),
  result       text        NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT check_in_attempts_result_valid
    CHECK (result IN ('accepted','already_used','invalid','wrong_event','void'))
);
CREATE INDEX check_in_attempts_ticket_idx ON check_in_attempts (ticket_id);
CREATE INDEX check_in_attempts_event_idx  ON check_in_attempts (event_id, created_at);

-- +goose Down
DROP TABLE check_in_attempts;
DROP TABLE tickets;
ALTER TABLE order_items DROP CONSTRAINT order_items_id_event_type_uniq;