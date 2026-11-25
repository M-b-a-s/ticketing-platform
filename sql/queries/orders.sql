-- name: ReserveTicketType :one
-- Takes stock if, and only if, every selling condition holds, all judged inside this
-- one statement using the database clock. No row back means "not reservable"; the
-- service then runs GetTicketTypeForSale to find out why.
UPDATE ticket_types
SET available_quantity = ticket_types.available_quantity - sqlc.arg(quantity)::int,
    updated_at = now()
WHERE ticket_types.id = sqlc.arg(id)
  AND ticket_types.event_id = sqlc.arg(event_id)
  AND ticket_types.available_quantity >= sqlc.arg(quantity)::int
  AND ticket_types.max_per_order >= sqlc.arg(quantity)::int
  AND ticket_types.sales_start <= now()
  AND ticket_types.sales_end > now()
  AND EXISTS (SELECT 1 FROM events e
              WHERE e.id = ticket_types.event_id AND e.status = 'published' AND e.ends_at > now())
RETURNING ticket_types.id, ticket_types.price_kobo, ticket_types.currency;

-- name: GetTicketTypeForSale :one
-- Diagnosis only: explains why ReserveTicketType found nothing. Scoped by event.
SELECT t.id, t.available_quantity, t.max_per_order, t.sales_start, t.sales_end,
       e.status AS event_status, e.ends_at AS event_ends_at, now()::timestamptz AS db_now
FROM ticket_types t
JOIN events e ON e.id = t.event_id
WHERE t.id = sqlc.arg(id) AND t.event_id = sqlc.arg(event_id);

-- name: LockUserEvent :exec
-- Serializes one buyer's order creation for one event, so the pending-order cap
-- cannot be raced past. Released automatically when the transaction ends.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(key)::text, 0));

-- name: CountPendingOrders :one
SELECT count(*) FROM orders
WHERE orders.user_id = sqlc.arg(user_id)
  AND orders.event_id = sqlc.arg(event_id)
  AND orders.status = 'pending'
  AND orders.expires_at > now();

-- name: CreateOrder :one
INSERT INTO orders (user_id, event_id, total_kobo, currency, expires_at)
VALUES (sqlc.arg(user_id), sqlc.arg(event_id), sqlc.arg(total_kobo), sqlc.arg(currency),
        now() + (sqlc.arg(hold_seconds)::int * interval '1 second'))
RETURNING *;

-- name: CreateOrderItem :one
INSERT INTO order_items (order_id, event_id, ticket_type_id, quantity, unit_price_kobo)
VALUES (sqlc.arg(order_id), sqlc.arg(event_id), sqlc.arg(ticket_type_id), sqlc.arg(quantity), sqlc.arg(unit_price_kobo))
RETURNING *;