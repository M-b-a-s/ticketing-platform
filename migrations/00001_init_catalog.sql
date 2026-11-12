-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TYPE user_role AS ENUM
  ('customer', 'organizer', 'event_admin', 'platform_admin', 'gate_attendant');

CREATE TABLE users (
  id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  email         citext      NOT NULL,
  password_hash text        NOT NULL,
  full_name     text        NOT NULL,
  role          user_role   NOT NULL DEFAULT 'customer',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT users_email_uniq   UNIQUE (email),
  CONSTRAINT users_email_format CHECK (position('@' in email::text) > 1)
);

CREATE TABLE events (
  id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  organizer_id  uuid        NOT NULL REFERENCES users(id),
  title         text        NOT NULL,
  description   text,
  venue_name    text        NOT NULL,
  starts_at     timestamptz NOT NULL,
  ends_at       timestamptz NOT NULL,
  status        text        NOT NULL DEFAULT 'draft',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT events_status_valid CHECK (status IN ('draft', 'published', 'cancelled', 'completed')),
  CONSTRAINT events_time_order   CHECK (ends_at > starts_at)
);
CREATE INDEX events_organizer_idx ON events (organizer_id);

CREATE TABLE ticket_types (
  id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  event_id           uuid        NOT NULL REFERENCES events(id),
  name               text        NOT NULL,
  price_kobo         bigint      NOT NULL,
  currency           text        NOT NULL DEFAULT 'NGN',
  total_quantity     integer     NOT NULL,
  available_quantity integer     NOT NULL,
  max_per_order      integer     NOT NULL DEFAULT 10,
  sales_start        timestamptz NOT NULL,
  sales_end          timestamptz NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ticket_types_event_name_uniq     UNIQUE (event_id, name),
  CONSTRAINT ticket_types_price_nonneg        CHECK (price_kobo >= 0),
  CONSTRAINT ticket_types_total_nonneg        CHECK (total_quantity >= 0),
  CONSTRAINT ticket_types_available_nonneg    CHECK (available_quantity >= 0),
  CONSTRAINT ticket_types_available_lte_total CHECK (available_quantity <= total_quantity),
  CONSTRAINT ticket_types_max_per_order_pos   CHECK (max_per_order > 0),
  CONSTRAINT ticket_types_sales_window        CHECK (sales_end > sales_start)
);

-- +goose Down
DROP TABLE ticket_types;
DROP TABLE events;
DROP TABLE users;
DROP TYPE user_role;
DROP EXTENSION citext;