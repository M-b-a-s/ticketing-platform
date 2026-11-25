package orders

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

// Tx is everything one order transaction needs. *db.Queries satisfies it when it is
// bound to a transaction.
type Tx interface {
	LockUserEvent(ctx context.Context, key string) error
	CountPendingOrders(ctx context.Context, arg db.CountPendingOrdersParams) (int64, error)
	ReserveTicketType(ctx context.Context, arg db.ReserveTicketTypeParams) (db.ReserveTicketTypeRow, error)
	GetTicketTypeForSale(ctx context.Context, arg db.GetTicketTypeForSaleParams) (db.GetTicketTypeForSaleRow, error)
	CreateOrder(ctx context.Context, arg db.CreateOrderParams) (db.Order, error)
	CreateOrderItem(ctx context.Context, arg db.CreateOrderItemParams) (db.OrderItem, error)
	CancelPendingOrder(ctx context.Context, arg db.CancelPendingOrderParams) (db.Order, error)
	ReleaseOrderStock(ctx context.Context, orderID pgtype.UUID) error
}

type Store interface {
	// InTx runs fn in one transaction: commit if it returns nil, roll back on an
	// error or a panic.
	InTx(ctx context.Context, fn func(Tx) error) error

	// plain reads, outside any transaction
	GetOrder(ctx context.Context, arg db.GetOrderParams) (db.Order, error)
	ListOrderItems(ctx context.Context, orderID pgtype.UUID) ([]db.ListOrderItemsRow, error)
	ListOrdersByUser(ctx context.Context, arg db.ListOrdersByUserParams) ([]db.Order, error)
}

type PgStore struct {
	*db.Queries // the non-transactional reads
	pool        *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{Queries: db.New(pool), pool: pool}
}

func (s *PgStore) InTx(ctx context.Context, fn func(Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(db.New(tx))
	})
}