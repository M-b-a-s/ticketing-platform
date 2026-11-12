-- +goose Up
CREATE TABLE refunds (
  id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  payment_id       uuid        NOT NULL REFERENCES payments(id),
  provider_refund_id text,                  -- Paystack's refund id, once they accept it
  amount_kobo      bigint      NOT NULL,
  reason           text        NOT NULL,
  status           text        NOT NULL DEFAULT 'pending',
  failure_reason   text,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT refunds_amount_pos    CHECK (amount_kobo > 0),
  CONSTRAINT refunds_reason_valid  CHECK (reason IN ('late_payment_no_stock','customer_request','event_cancelled')),
  CONSTRAINT refunds_status_valid  CHECK (status IN ('pending','processing','processed','failed')),
  CONSTRAINT refunds_payment_reason_uniq UNIQUE (payment_id, reason)
);

-- +goose Down
DROP TABLE refunds;