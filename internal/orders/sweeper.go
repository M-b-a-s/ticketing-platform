package orders

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

// SweepTx is what one sweep transaction needs. *db.Queries satisfies it when bound
// to a transaction.
type SweepTx interface {
	ExpirePendingOrders(ctx context.Context, batchSize int32) ([]pgtype.UUID, error)
	LockTicketTypesOfOrders(ctx context.Context, orderIDs []pgtype.UUID) error
	ReleaseStockForOrders(ctx context.Context, orderIDs []pgtype.UUID) error
	DeleteOldIdempotencyKeys(ctx context.Context, arg db.DeleteOldIdempotencyKeysParams) (int64, error)
}

type SweepStore interface {
	InSweepTx(ctx context.Context, fn func(SweepTx) error) error
}

// SweepConfig's zero value means "use the defaults".
type SweepConfig struct {
	Interval          time.Duration // how often to look for expired orders (default 10s)
	BatchSize         int32         // orders expired per transaction (default 100)
	MaxBatchesPerTick int           // cap on one tick's work, so a backlog cannot delay shutdown (default 50)
	KeyMaxAge         time.Duration // idempotency keys older than this are deleted (default 24h)
	KeyCleanupEvery   time.Duration // how often to run that cleanup (default 1h)
	KeyBatchSize      int32         // keys deleted per statement (default 1000)
}

func (c SweepConfig) withDefaults() SweepConfig {
	if c.Interval <= 0 {
		c.Interval = 10 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.MaxBatchesPerTick <= 0 {
		c.MaxBatchesPerTick = 50
	}
	if c.KeyMaxAge <= 0 {
		c.KeyMaxAge = 24 * time.Hour
	}
	if c.KeyCleanupEvery <= 0 {
		c.KeyCleanupEvery = time.Hour
	}
	if c.KeyBatchSize <= 0 {
		c.KeyBatchSize = 1000
	}
	return c
}

// A batch that has started is allowed to finish even if shutdown begins, so it is
// not rolled back half done. This bounds how long that can take.
const batchTimeout = 30 * time.Second

type Sweeper struct {
	store SweepStore
	cfg   SweepConfig
	log   *slog.Logger
}

func NewSweeper(store SweepStore, cfg SweepConfig, log *slog.Logger) *Sweeper {
	return &Sweeper{store: store, cfg: cfg.withDefaults(), log: log}
}

// SweepOnce expires every order that is due, one batch at a time, and returns how
// many it expired. It is safe to run in several processes at once.
func (s *Sweeper) SweepOnce(ctx context.Context) (int, error) {
	total := 0
	for i := 0; i < s.cfg.MaxBatchesPerTick && ctx.Err() == nil; i++ {
		n, err := s.sweepBatch(ctx)
		if err != nil {
			return total, err
		}
		total += n
		if n < int(s.cfg.BatchSize) { // a short batch means nothing more is due
			break
		}
	}
	return total, nil
}

func (s *Sweeper) sweepBatch(ctx context.Context) (int, error) {
	batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)
	defer cancel()

	expired := 0
	err := s.store.InSweepTx(batchCtx, func(tx SweepTx) error {
		ids, err := tx.ExpirePendingOrders(batchCtx, s.cfg.BatchSize)
		if err != nil {
			return err
		}
		expired = len(ids)
		if expired == 0 {
			return nil
		}
		// We won the pending -> expired change for exactly these orders, so releasing
		// their stock, in the same transaction, is both required and safe.
		if err := tx.LockTicketTypesOfOrders(batchCtx, ids); err != nil {
			return err
		}
		return tx.ReleaseStockForOrders(batchCtx, ids)
	})
	if err != nil {
		return 0, err
	}
	return expired, nil
}

// CleanupKeys deletes idempotency keys past their retention, in batches.
func (s *Sweeper) CleanupKeys(ctx context.Context) (int, error) {
	total := 0
	for i := 0; i < s.cfg.MaxBatchesPerTick && ctx.Err() == nil; i++ {
		var n int64
		err := s.store.InSweepTx(ctx, func(tx SweepTx) error {
			var err error
			n, err = tx.DeleteOldIdempotencyKeys(ctx, db.DeleteOldIdempotencyKeysParams{
				MaxAgeHours: int32(s.cfg.KeyMaxAge / time.Hour),
				BatchSize:   s.cfg.KeyBatchSize,
			})
			return err
		})
		if err != nil {
			return total, err
		}
		total += int(n)
		if n < int64(s.cfg.KeyBatchSize) {
			break
		}
	}
	return total, nil
}

// Run sweeps until ctx ends. It sweeps once immediately, to catch up on anything
// that expired while the worker was down.
func (s *Sweeper) Run(ctx context.Context) {
	s.tick(ctx)
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// RunKeyCleanup deletes old idempotency keys until ctx ends.
func (s *Sweeper) RunKeyCleanup(ctx context.Context) {
	s.cleanupTick(ctx)
	t := time.NewTicker(s.cfg.KeyCleanupEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.cleanupTick(ctx)
		}
	}
}

func (s *Sweeper) tick(ctx context.Context) {
	n, err := s.SweepOnce(ctx)
	if n > 0 {
		s.log.InfoContext(ctx, "expired orders released", "orders", n)
	}
	if err != nil { // the batch rolled back, so its orders are still pending and the next tick retries them
		s.log.ErrorContext(ctx, "expiry sweep failed", "error", err)
	}
}

func (s *Sweeper) cleanupTick(ctx context.Context) {
	n, err := s.CleanupKeys(ctx)
	if n > 0 {
		s.log.InfoContext(ctx, "old idempotency keys deleted", "keys", n)
	}
	if err != nil {
		s.log.ErrorContext(ctx, "idempotency key cleanup failed", "error", err)
	}
}