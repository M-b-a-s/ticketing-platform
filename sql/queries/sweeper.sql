-- name: ExpirePendingOrders :many
-- Claims up to batch_size pending orders whose hold has run out and marks them
-- expired. SKIP LOCKED means a sweeper never waits for an order that another
-- transaction (a second sweeper, or a buyer cancelling) is already working on.
WITH due AS (
  SELECT o.id FROM orders o
  WHERE o.status = 'pending' AND o.expires_at <= now()
  ORDER BY o.expires_at, o.id
  LIMIT sqlc.arg(batch_size)::int
  FOR UPDATE SKIP LOCKED
)
UPDATE orders
SET status = 'expired', updated_at = now()
FROM due
WHERE orders.id = due.id
RETURNING orders.id;

-- name: LockTicketTypesOfOrders :exec
-- Takes the row locks the release below needs, in id order. Every code path that
-- moves stock locks ticket types in this same order, so none can deadlock another.
SELECT t.id FROM ticket_types t
WHERE t.id IN (SELECT i.ticket_type_id FROM order_items i WHERE i.order_id = ANY(sqlc.arg(order_ids)::uuid[]))
ORDER BY t.id
FOR UPDATE;

-- name: ReleaseStockForOrders :exec
-- Gives the tickets of the given orders back, summed per ticket type. Call it only in
-- the same transaction that won the pending -> cancelled/expired change for them.
UPDATE ticket_types
SET available_quantity = ticket_types.available_quantity + r.qty, updated_at = now()
FROM (
  SELECT i.ticket_type_id, sum(i.quantity)::int AS qty
  FROM order_items i
  WHERE i.order_id = ANY(sqlc.arg(order_ids)::uuid[])
  GROUP BY i.ticket_type_id
) r
WHERE ticket_types.id = r.ticket_type_id;

-- name: DeleteOldIdempotencyKeys :execrows
DELETE FROM idempotency_keys
WHERE idempotency_keys.id IN (
  SELECT k.id FROM idempotency_keys k
  WHERE k.created_at < now() - (sqlc.arg(max_age_hours)::int * interval '1 hour')
  ORDER BY k.created_at
  LIMIT sqlc.arg(batch_size)::int
);