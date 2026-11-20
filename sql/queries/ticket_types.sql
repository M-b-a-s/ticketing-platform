-- name: CreateTicketType :one
INSERT INTO ticket_types (event_id, name, price_kobo, currency, total_quantity, available_quantity, max_per_order, sales_start, sales_end)
VALUES (sqlc.arg(event_id), sqlc.arg(name), sqlc.arg(price_kobo), sqlc.arg(currency),
        sqlc.arg(total_quantity), sqlc.arg(total_quantity), sqlc.arg(max_per_order),
        sqlc.arg(sales_start), sqlc.arg(sales_end))
RETURNING *;

-- name: ListTicketTypes :many
SELECT * FROM ticket_types WHERE event_id = $1 ORDER BY price_kobo ASC, name ASC;

-- name: GetTicketType :one
-- Scoped by event: a ticket type is only ever reachable through its own event.
SELECT * FROM ticket_types WHERE id = sqlc.arg(id) AND event_id = sqlc.arg(event_id);

-- name: UpdateTicketType :one
-- Partial update: a NULL argument leaves that column unchanged. Changing the total
-- moves available_quantity by the same amount, relative to its CURRENT value, so
-- reservations running at the same moment are never lost. A total below what is
-- already sold fails the available >= 0 CHECK. The price may only change while
-- nothing has been sold or reserved (re-sending the same price is allowed).
UPDATE ticket_types SET
  name               = COALESCE(sqlc.narg(name)::text, name),
  price_kobo         = COALESCE(sqlc.narg(price_kobo)::bigint, price_kobo),
  total_quantity     = COALESCE(sqlc.narg(total_quantity)::int, total_quantity),
  available_quantity = available_quantity + (COALESCE(sqlc.narg(total_quantity)::int, total_quantity) - total_quantity),
  max_per_order      = COALESCE(sqlc.narg(max_per_order)::int, max_per_order),
  sales_start        = COALESCE(sqlc.narg(sales_start)::timestamptz, sales_start),
  sales_end          = COALESCE(sqlc.narg(sales_end)::timestamptz, sales_end),
  updated_at         = now()
WHERE id = sqlc.arg(id)
  AND event_id = sqlc.arg(event_id)
  AND (sqlc.narg(price_kobo)::bigint IS NULL
       OR price_kobo = sqlc.narg(price_kobo)::bigint
       OR available_quantity = total_quantity)
RETURNING *;

-- name: DeleteTicketType :execrows
-- Only a never-sold type of a DRAFT event. Zero rows deleted means "not allowed".
DELETE FROM ticket_types
WHERE ticket_types.id = sqlc.arg(id)
  AND ticket_types.event_id = sqlc.arg(event_id)
  AND ticket_types.available_quantity = ticket_types.total_quantity
  AND EXISTS (SELECT 1 FROM events e WHERE e.id = ticket_types.event_id AND e.status = 'draft');

-- name: ListPublicTicketTypes :many
SELECT t.*
FROM ticket_types t
JOIN events e ON e.id = t.event_id
WHERE t.event_id = $1 AND e.status = 'published' AND e.published_at IS NOT NULL
ORDER BY t.price_kobo ASC, t.name ASC;

-- name: MaxTicketSalesEnd :one
-- NULL (Valid=false) when the event has no ticket types.
SELECT max(ticket_types.sales_end)::timestamptz AS max_sales_end
FROM ticket_types
WHERE ticket_types.event_id = $1;