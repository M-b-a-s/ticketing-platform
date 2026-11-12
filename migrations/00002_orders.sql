-- +goose Up

-- Lets child tables prove they reference a ticket type from the SAME event.
-- Redundant with the PK on id, but a foreign key can only target a unique key.
ALTER TABLE ticket_types
  ADD CONSTRAINT ticket_types_id_event_uniq UNIQUE (id, event_id);

CREATE TABLE orders (
  id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid        NOT NULL REFERENCES users(id),
  event_id    uuid        NOT NULL REFERENCES events(id),
  status      text        NOT NULL DEFAULT 'pending',
  total_kobo  bigint      NOT NULL,
  currency    text        NOT NULL DEFAULT 'NGN',
  expires_at  timestamptz NOT NULL,   -- reservation deadline while pending (now() + 10 min, set by the app)
  paid_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT orders_id_event_uniq UNIQUE (id, event_id),
  CONSTRAINT orders_status_valid  CHECK (status IN ('pending','paid','fulfilled','expired','cancelled','refunded')),
  CONSTRAINT orders_total_nonneg  CHECK (total_kobo >= 0)
);
CREATE INDEX orders_user_idx           ON orders (user_id, created_at DESC);
CREATE INDEX orders_event_idx          ON orders (event_id);
CREATE INDEX orders_pending_expiry_idx ON orders (expires_at) WHERE status = 'pending';

CREATE TABLE order_items (
  id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id        uuid        NOT NULL,
  event_id        uuid        NOT NULL,
  ticket_type_id  uuid        NOT NULL,
  quantity        integer     NOT NULL,
  unit_price_kobo bigint      NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT order_items_order_fk       FOREIGN KEY (order_id, event_id)       REFERENCES orders (id, event_id),
  CONSTRAINT order_items_ticket_type_fk FOREIGN KEY (ticket_type_id, event_id) REFERENCES ticket_types (id, event_id),
  CONSTRAINT order_items_order_type_uniq UNIQUE (order_id, ticket_type_id),
  CONSTRAINT order_items_qty_pos         CHECK (quantity > 0),
  CONSTRAINT order_items_price_nonneg    CHECK (unit_price_kobo >= 0)
);
CREATE INDEX order_items_ticket_type_idx ON order_items (ticket_type_id);

-- +goose Down
DROP TABLE order_items;
DROP TABLE orders;
ALTER TABLE ticket_types DROP CONSTRAINT ticket_types_id_event_uniq;