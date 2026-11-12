-- +goose Up
CREATE TABLE event_staff (
  id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  event_id    uuid        NOT NULL REFERENCES events(id),
  user_id     uuid        NOT NULL REFERENCES users(id),
  role        text        NOT NULL,
  added_by    uuid        REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT event_staff_event_user_uniq UNIQUE (event_id, user_id),
  CONSTRAINT event_staff_role_valid CHECK (role IN ('event_admin','gate_attendant'))
);
CREATE INDEX event_staff_user_idx ON event_staff (user_id);

-- +goose Down
DROP TABLE event_staff;