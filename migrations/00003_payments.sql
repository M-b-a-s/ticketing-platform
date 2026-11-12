-- +goose Up
CREATE TABLE payments (
  id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id                uuid        NOT NULL REFERENCES orders(id),
  provider                text        NOT NULL DEFAULT 'paystack',
  reference               text        NOT NULL,   -- ours; sent to Paystack at initialize
  provider_transaction_id text,                   -- Paystack's id, known once they report on it
  amount_kobo             bigint      NOT NULL,
  currency                text        NOT NULL DEFAULT 'NGN',
  status                  text        NOT NULL DEFAULT 'pending',
  authorization_url       text,                   -- lets the buyer resume checkout within the hold
  failure_reason          text,
  paid_at                 timestamptz,
  created_at              timestamptz NOT NULL DEFAULT now(),
  updated_at              timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT payments_provider_reference_uniq UNIQUE (provider, reference),
  CONSTRAINT payments_status_valid CHECK (status IN ('pending','succeeded','failed','abandoned')),
  CONSTRAINT payments_amount_pos   CHECK (amount_kobo > 0)
);
CREATE INDEX payments_order_idx ON payments (order_id);

-- At most one successful payment per order: a duplicate "success" can never be recorded twice.
CREATE UNIQUE INDEX payments_one_success_per_order
  ON payments (order_id) WHERE status = 'succeeded';

-- +goose Down
DROP TABLE payments;