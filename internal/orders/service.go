package orders

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/pagination"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
)

var ErrNotFound = errors.New("orders: not found")

type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "orders: validation failed" }

// ConflictError is a well-formed request that cannot be satisfied right now.
type ConflictError struct{ Code, Message string }

func (e *ConflictError) Error() string { return "orders: " + e.Code }

func conflict(code, message string) error { return &ConflictError{Code: code, Message: message} }

type Service struct{ store Store }

func NewService(store Store) *Service { return &Service{store: store} }

const (
	scopeCreateOrder = "create_order"
	createdStatus    = 201
)

var (
	// ErrKeyReused: the same Idempotency-Key arrived with a different request.
	ErrKeyReused = errors.New("orders: idempotency key reused with a different request")
	// ErrKeyInProgress: the key exists but its first request never completed. It cannot
	// happen while claim and completion share one transaction, so it is purely defensive.
	ErrKeyInProgress = errors.New("orders: a request with this idempotency key is still in progress")
)

type idempotency struct{ key, requestHash string }

// Create reserves the requested tickets without idempotency protection. The HTTP
// layer always uses CreateIdempotent.
func (s *Service) Create(ctx context.Context, in CreateInput) (View, error) {
	view, _, err := s.create(ctx, in, nil)
	return view, err
}

// CreateIdempotent is Create, made safe to retry. replayed is true when the key had
// already produced an order, which is then returned instead of creating another.
func (s *Service) CreateIdempotent(ctx context.Context, in CreateInput, key string) (View, bool, error) {
	if !ValidIdempotencyKey(key) {
		return View{}, false, &ValidationError{Fields: map[string]string{"Idempotency-Key": idempotencyKeyRule}}
	}
	return s.create(ctx, in, &idempotency{key: key, requestHash: RequestHash(in)})
}

func (s *Service) create(ctx context.Context, in CreateInput, idem *idempotency) (View, bool, error) {
	if fields := in.Validate(); len(fields) > 0 {
		return View{}, false, &ValidationError{Fields: fields}
	}
	lines := sortedLines(in.Lines)

	var view View
	var replayOf pgtype.UUID // set when this request repeats one that already succeeded

	err := s.store.InTx(ctx, func(tx Tx) error {
		// One buyer's orders for one event are created one at a time. This also makes
		// two copies of the same request line up, so the second finds the first's key.
		if err := tx.LockUserEvent(ctx, uuidx.String(in.UserID)+":"+uuidx.String(in.EventID)); err != nil {
			return err
		}

		var keyID pgtype.UUID
		if idem != nil {
			id, err := tx.ClaimIdempotencyKey(ctx, db.ClaimIdempotencyKeyParams{
				UserID: in.UserID, Scope: scopeCreateOrder, Key: idem.key, RequestHash: idem.requestHash,
			})
			switch {
			case err == nil:
				keyID = id // first time we see this key: we own it
			case errors.Is(err, pgx.ErrNoRows):
				prev, err := tx.GetIdempotencyKey(ctx, db.GetIdempotencyKeyParams{
					UserID: in.UserID, Scope: scopeCreateOrder, Key: idem.key,
				})
				if err != nil {
					return err
				}
				switch {
				case prev.RequestHash != idem.requestHash:
					return ErrKeyReused
				case !prev.ResponseStatus.Valid:
					return ErrKeyInProgress
				}
				orderID, err := orderIDFrom(prev.ResponseBody)
				if err != nil {
					return err
				}
				replayOf = orderID
				return nil // nothing to create: the original order is the answer
			default:
				return err
			}
		}

		pending, err := tx.CountPendingOrders(ctx, db.CountPendingOrdersParams{UserID: in.UserID, EventID: in.EventID})
		if err != nil {
			return err
		}
		if pending >= MaxPendingPerEvent {
			return conflict("too_many_pending_orders",
				"you already have the maximum number of unpaid orders for this event; pay for or cancel one first")
		}

		type taken struct {
			line  Line
			name  string
			price int64
		}
		reserved := make([]taken, 0, len(lines))
		var total int64
		currency := ""

		for _, l := range lines {
			row, err := tx.ReserveTicketType(ctx, db.ReserveTicketTypeParams{
				ID: l.TicketTypeID, EventID: in.EventID, Quantity: int32(l.Quantity),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return diagnose(ctx, tx, in.EventID, l)
			}
			if err != nil {
				return err
			}
			if currency == "" {
				currency = row.Currency
			} else if row.Currency != currency {
				return &ValidationError{Fields: map[string]string{"items": "ticket types with different currencies cannot share an order"}}
			}
			total += row.PriceKobo * int64(l.Quantity) // the price is the database's, never the client's
			reserved = append(reserved, taken{line: l, name: row.Name, price: row.PriceKobo})
		}

		order, err := tx.CreateOrder(ctx, db.CreateOrderParams{
			UserID: in.UserID, EventID: in.EventID, TotalKobo: total, Currency: currency,
			HoldSeconds: int32(HoldDuration.Seconds()),
		})
		if err != nil {
			return err
		}

		items := make([]Item, 0, len(reserved))
		for _, r := range reserved {
			if _, err := tx.CreateOrderItem(ctx, db.CreateOrderItemParams{
				OrderID: order.ID, EventID: in.EventID, TicketTypeID: r.line.TicketTypeID,
				Quantity: int32(r.line.Quantity), UnitPriceKobo: r.price, // snapshot: later price edits don't touch this order
			}); err != nil {
				return err
			}
			items = append(items, Item{TicketTypeID: r.line.TicketTypeID, Name: r.name, Quantity: int32(r.line.Quantity), UnitPriceKobo: r.price})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name }) // same order as Get

		// Record the outcome in the SAME transaction as the order. Either both exist
		// after the commit, or neither does.
		if idem != nil {
			body, err := json.Marshal(map[string]string{"order_id": uuidx.String(order.ID)})
			if err != nil {
				return err
			}
			if err := tx.CompleteIdempotencyKey(ctx, db.CompleteIdempotencyKeyParams{
				ID: keyID, ResponseStatus: createdStatus, ResponseBody: body,
			}); err != nil {
				return err
			}
		}

		view = View{Order: order, Items: items}
		return nil
	})
	if err != nil {
		return View{}, false, err
	}

	if replayOf.Valid { // the original order, as it stands now
		v, err := s.Get(ctx, in.UserID, replayOf)
		return v, true, err
	}
	return view, false, nil
}

func orderIDFrom(body []byte) (pgtype.UUID, error) {
	var v struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return pgtype.UUID{}, err
	}
	return uuidx.Parse(v.OrderID)
}

// Get returns one of the caller's own orders. Anyone else's order does not exist.
func (s *Service) Get(ctx context.Context, userID, orderID pgtype.UUID) (View, error) {
	order, err := s.store.GetOrder(ctx, db.GetOrderParams{ID: orderID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, err
	}
	rows, err := s.store.ListOrderItems(ctx, orderID)
	if err != nil {
		return View{}, err
	}
	items := make([]Item, 0, len(rows))
	for _, r := range rows {
		items = append(items, Item{TicketTypeID: r.TicketTypeID, Name: r.TicketTypeName, Quantity: r.Quantity, UnitPriceKobo: r.UnitPriceKobo})
	}
	return View{Order: order, Items: items}, nil
}

type Page struct {
	Items      []db.Order
	NextCursor *string // nil on the last page
}

// List returns one page of the caller's orders, newest first.
func (s *Service) List(ctx context.Context, userID pgtype.UUID, limit int, after *pagination.Cursor) (Page, error) {
	params := db.ListOrdersByUserParams{UserID: userID, PageSize: int32(limit + 1)} // the extra row says whether another page exists
	if after != nil {
		id, err := uuidx.Parse(after.ID)
		if err != nil {
			return Page{}, pagination.ErrInvalidCursor
		}
		params.AfterCreatedAt = pgtype.Timestamptz{Time: after.Time, Valid: true}
		params.AfterID = id
	}

	rows, err := s.store.ListOrdersByUser(ctx, params)
	if err != nil {
		return Page{}, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	page := Page{Items: rows}
	if page.Items == nil {
		page.Items = []db.Order{}
	}
	if hasMore {
		last := rows[len(rows)-1]
		c := pagination.Encode(pagination.Cursor{Time: last.CreatedAt.Time, ID: uuidx.String(last.ID)})
		page.NextCursor = &c
	}
	return page, nil
}

// Cancel releases a pending order's tickets. Repeating it succeeds. Stock goes back
// exactly once: only the request that wins the pending -> cancelled change releases it.
func (s *Service) Cancel(ctx context.Context, userID, orderID pgtype.UUID) (View, error) {
	err := s.store.InTx(ctx, func(tx Tx) error {
		_, err := tx.CancelPendingOrder(ctx, db.CancelPendingOrderParams{ID: orderID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // not pending, or not theirs: nothing to release, diagnosed below
		}
		if err != nil {
			return err
		}
		return tx.ReleaseOrderStock(ctx, orderID) // same transaction as the status change
	})
	if err != nil {
		return View{}, err
	}

	view, err := s.Get(ctx, userID, orderID)
	if err != nil {
		return View{}, err // ErrNotFound for an order that is not theirs
	}
	switch view.Order.Status {
	case StatusCancelled:
		return view, nil // we just cancelled it, or it already was
	case StatusExpired:
		return View{}, conflict("order_expired", "this order expired and its tickets were released")
	default: // paid, fulfilled, refunded
		return View{}, conflict("order_not_cancellable", "this order can no longer be cancelled here")
	}
}

// diagnose explains why a line could not be reserved. It runs after the failed
// UPDATE, inside the same transaction, which rolls back anyway.
func diagnose(ctx context.Context, tx Tx, eventID pgtype.UUID, l Line) error {
	row, err := tx.GetTicketTypeForSale(ctx, db.GetTicketTypeForSaleParams{ID: l.TicketTypeID, EventID: eventID})
	if errors.Is(err, pgx.ErrNoRows) {
		return &ValidationError{Fields: map[string]string{"items": "a ticket type does not belong to this event"}}
	}
	if err != nil {
		return err
	}

	now := row.DbNow.Time
	switch {
	case row.EventStatus == "draft":
		return ErrNotFound // as far as a buyer is concerned, a draft does not exist
	case row.EventStatus != "published" || !row.EventEndsAt.Time.After(now):
		return conflict("event_not_on_sale", "this event is no longer on sale")
	case now.Before(row.SalesStart.Time):
		return conflict("sales_not_started", "sales for this ticket type have not started")
	case !now.Before(row.SalesEnd.Time):
		return conflict("sales_closed", "sales for this ticket type have ended")
	case int32(l.Quantity) > row.MaxPerOrder:
		return conflict("quantity_exceeds_limit", fmt.Sprintf("at most %d of this ticket type per order", row.MaxPerOrder))
	default:
		return conflict("sold_out", "not enough tickets left") // exact counts stay private
	}
}