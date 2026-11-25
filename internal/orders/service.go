package orders

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
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

type Result struct {
	Order db.Order
	Items []db.OrderItem
}

// Create reserves the requested tickets and records a pending order that holds them
// for HoldDuration. It is all or nothing: if any line cannot be reserved, the
// transaction rolls back and no stock is taken.
func (s *Service) Create(ctx context.Context, in CreateInput) (Result, error) {
	if fields := in.Validate(); len(fields) > 0 {
		return Result{}, &ValidationError{Fields: fields}
	}
	lines := sortedLines(in.Lines)

	var res Result
	err := s.store.InTx(ctx, func(tx Tx) error {
		// One buyer's orders for one event are created one at a time, so the cap
		// below cannot be raced past by simultaneous requests.
		if err := tx.LockUserEvent(ctx, uuidx.String(in.UserID)+":"+uuidx.String(in.EventID)); err != nil {
			return err
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
			reserved = append(reserved, taken{line: l, price: row.PriceKobo})
		}

		order, err := tx.CreateOrder(ctx, db.CreateOrderParams{
			UserID: in.UserID, EventID: in.EventID, TotalKobo: total, Currency: currency,
			HoldSeconds: int32(HoldDuration.Seconds()),
		})
		if err != nil {
			return err
		}

		items := make([]db.OrderItem, 0, len(reserved))
		for _, r := range reserved {
			item, err := tx.CreateOrderItem(ctx, db.CreateOrderItemParams{
				OrderID: order.ID, EventID: in.EventID, TicketTypeID: r.line.TicketTypeID,
				Quantity: int32(r.line.Quantity), UnitPriceKobo: r.price, // snapshot: later price edits don't touch this order
			})
			if err != nil {
				return err
			}
			items = append(items, item)
		}

		res = Result{Order: order, Items: items}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
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