package orders

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
)

type fakeTx struct {
	pending      int64
	reservable   map[pgtype.UUID]db.ReserveTicketTypeRow // present means the reservation succeeds
	reservedInOrder []pgtype.UUID
	sale         db.GetTicketTypeForSaleRow
	saleErr      error
	lockedKey    string
	orderParams  *db.CreateOrderParams
	itemParams   []db.CreateOrderItemParams
}

func (f *fakeTx) LockUserEvent(_ context.Context, key string) error { f.lockedKey = key; return nil }

func (f *fakeTx) CountPendingOrders(context.Context, db.CountPendingOrdersParams) (int64, error) {
	return f.pending, nil
}

func (f *fakeTx) ReserveTicketType(_ context.Context, arg db.ReserveTicketTypeParams) (db.ReserveTicketTypeRow, error) {
	f.reservedInOrder = append(f.reservedInOrder, arg.ID)
	if row, ok := f.reservable[arg.ID]; ok {
		return row, nil
	}
	return db.ReserveTicketTypeRow{}, pgx.ErrNoRows
}

func (f *fakeTx) GetTicketTypeForSale(context.Context, db.GetTicketTypeForSaleParams) (db.GetTicketTypeForSaleRow, error) {
	return f.sale, f.saleErr
}

func (f *fakeTx) CreateOrder(_ context.Context, arg db.CreateOrderParams) (db.Order, error) {
	f.orderParams = &arg
	return db.Order{ID: id(100), UserID: arg.UserID, EventID: arg.EventID, TotalKobo: arg.TotalKobo, Currency: arg.Currency, Status: "pending"}, nil
}

func (f *fakeTx) CreateOrderItem(_ context.Context, arg db.CreateOrderItemParams) (db.OrderItem, error) {
	f.itemParams = append(f.itemParams, arg)
	return db.OrderItem{OrderID: arg.OrderID, TicketTypeID: arg.TicketTypeID, Quantity: arg.Quantity, UnitPriceKobo: arg.UnitPriceKobo}, nil
}

func (f *fakeTx) CancelPendingOrder(context.Context, db.CancelPendingOrderParams) (db.Order, error) {
	return db.Order{}, pgx.ErrNoRows
}

func (f *fakeTx) ReleaseOrderStock(context.Context, pgtype.UUID) error { return nil }

type fakeStore struct {
	tx  *fakeTx
	ran bool
}

func (s *fakeStore) InTx(_ context.Context, fn func(Tx) error) error {
	s.ran = true
	return fn(s.tx)
}

func (s *fakeStore) GetOrder(context.Context, db.GetOrderParams) (db.Order, error) {
	return db.Order{}, pgx.ErrNoRows
}

func (s *fakeStore) ListOrderItems(context.Context, pgtype.UUID) ([]db.ListOrderItemsRow, error) {
	return nil, nil
}

func (s *fakeStore) ListOrdersByUser(context.Context, db.ListOrdersByUserParams) ([]db.Order, error) {
	return nil, nil
}

var (
	buyer = id(50)
	event = id(60)
)

func rowAt(price int64) db.ReserveTicketTypeRow {
	return db.ReserveTicketTypeRow{PriceKobo: price, Currency: "NGN"}
}

func TestCreateHappyPath(t *testing.T) {
	tx := &fakeTx{reservable: map[pgtype.UUID]db.ReserveTicketTypeRow{id(2): rowAt(500000), id(9): rowAt(1500000)}}
	store := &fakeStore{tx: tx}

	res, err := NewService(store).Create(context.Background(), CreateInput{
		UserID: buyer, EventID: event,
		Lines: []Line{{TicketTypeID: id(9), Quantity: 2}, {TicketTypeID: id(2), Quantity: 3}}, // deliberately out of order
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(tx.reservedInOrder) != 2 || tx.reservedInOrder[0] != id(2) || tx.reservedInOrder[1] != id(9) {
		t.Fatalf("stock must be reserved in ticket type id order, got %v", tx.reservedInOrder)
	}
	if want := int64(3*500000 + 2*1500000); res.Order.TotalKobo != want {
		t.Fatalf("total should come from the database prices: want %d, got %d", want, res.Order.TotalKobo)
	}
	if tx.orderParams.HoldSeconds != 600 || tx.orderParams.UserID != buyer {
		t.Fatalf("unexpected order: %+v", tx.orderParams)
	}
	if len(tx.itemParams) != 2 || tx.itemParams[0].UnitPriceKobo != 500000 || tx.itemParams[1].UnitPriceKobo != 1500000 {
		t.Fatalf("each item must snapshot the price it was reserved at: %+v", tx.itemParams)
	}
	if tx.lockedKey != uuidx.String(buyer)+":"+uuidx.String(event) {
		t.Fatalf("the buyer-and-event lock key is wrong: %q", tx.lockedKey)
	}
}

func TestCreateInvalidInputNeverOpensATransaction(t *testing.T) {
	store := &fakeStore{tx: &fakeTx{}}

	_, err := NewService(store).Create(context.Background(), CreateInput{UserID: buyer, EventID: event})

	var ve *ValidationError
	if !errors.As(err, &ve) || store.ran {
		t.Fatalf("want a validation error and no transaction, got %v ran=%v", err, store.ran)
	}
}

func TestCreateEnforcesThePendingCap(t *testing.T) {
	tx := &fakeTx{pending: MaxPendingPerEvent, reservable: map[pgtype.UUID]db.ReserveTicketTypeRow{id(1): rowAt(1)}}

	_, err := NewService(&fakeStore{tx: tx}).Create(context.Background(),
		CreateInput{UserID: buyer, EventID: event, Lines: []Line{{TicketTypeID: id(1), Quantity: 1}}})

	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Code != "too_many_pending_orders" || len(tx.reservedInOrder) != 0 {
		t.Fatalf("want the cap to stop the order before any stock moves, got %v", err)
	}
}

func TestCreateExplainsWhyAReservationFailed(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) pgtype.Timestamptz { return pgtype.Timestamptz{Time: now.Add(d), Valid: true} }
	onSale := db.GetTicketTypeForSaleRow{
		MaxPerOrder: 10, SalesStart: ts(-time.Hour), SalesEnd: ts(time.Hour),
		EventStatus: "published", EventEndsAt: ts(48 * time.Hour), DbNow: ts(0),
	}
	with := func(mutate func(*db.GetTicketTypeForSaleRow)) db.GetTicketTypeForSaleRow {
		r := onSale
		mutate(&r)
		return r
	}

	cases := []struct {
		name string
		sale db.GetTicketTypeForSaleRow
		qty  int
		want string // a ConflictError code, or "not_found" / "validation"
	}{
		{"sold out", onSale, 1, "sold_out"},
		{"over the per-order limit", with(func(r *db.GetTicketTypeForSaleRow) { r.MaxPerOrder = 2 }), 3, "quantity_exceeds_limit"},
		{"sales not started", with(func(r *db.GetTicketTypeForSaleRow) { r.SalesStart = ts(time.Hour); r.SalesEnd = ts(2 * time.Hour) }), 1, "sales_not_started"},
		{"sales closed", with(func(r *db.GetTicketTypeForSaleRow) { r.SalesStart = ts(-2 * time.Hour); r.SalesEnd = ts(-time.Hour) }), 1, "sales_closed"},
		{"event cancelled", with(func(r *db.GetTicketTypeForSaleRow) { r.EventStatus = "cancelled" }), 1, "event_not_on_sale"},
		{"event already over", with(func(r *db.GetTicketTypeForSaleRow) { r.EventEndsAt = ts(-time.Minute) }), 1, "event_not_on_sale"},
		{"event is a draft", with(func(r *db.GetTicketTypeForSaleRow) { r.EventStatus = "draft" }), 1, "not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx := &fakeTx{sale: c.sale} // nothing is reservable
			_, err := NewService(&fakeStore{tx: tx}).Create(context.Background(),
				CreateInput{UserID: buyer, EventID: event, Lines: []Line{{TicketTypeID: id(1), Quantity: c.qty}}})

			var ce *ConflictError
			switch {
			case c.want == "not_found" && !errors.Is(err, ErrNotFound):
				t.Fatalf("want ErrNotFound, got %v", err)
			case c.want != "not_found" && (!errors.As(err, &ce) || ce.Code != c.want):
				t.Fatalf("want conflict %q, got %v", c.want, err)
			}
			if tx.orderParams != nil {
				t.Fatal("no order may be created when a line cannot be reserved")
			}
		})
	}

	t.Run("a ticket type from another event", func(t *testing.T) {
		tx := &fakeTx{saleErr: pgx.ErrNoRows}
		_, err := NewService(&fakeStore{tx: tx}).Create(context.Background(),
			CreateInput{UserID: buyer, EventID: event, Lines: []Line{{TicketTypeID: id(1), Quantity: 1}}})

		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want a validation error, got %v", err)
		}
	})
}