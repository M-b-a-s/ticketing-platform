package orders

import (
	"context"

	"github.com/jackc/pgx/v5"
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
}

type Store interface {
	// InTx runs fn in one transaction: commit if it returns nil, roll back on an
	// error or a panic.
	InTx(ctx context.Context, fn func(Tx) error) error
}

type PgStore struct{ pool *pgxpool.Pool }

func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

func (s *PgStore) InTx(ctx context.Context, fn func(Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(db.New(tx))
	})
}