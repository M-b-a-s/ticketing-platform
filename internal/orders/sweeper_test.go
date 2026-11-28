package orders

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

type fakeSweepTx struct {
	batches       [][]pgtype.UUID // successive results of ExpirePendingOrders
	calls         []string
	locked        [][]pgtype.UUID
	released      [][]pgtype.UUID
	releaseErr    error
	deleteResults []int64
	deleteParams  []db.DeleteOldIdempotencyKeysParams
}

func (f *fakeSweepTx) ExpirePendingOrders(_ context.Context, _ int32) ([]pgtype.UUID, error) {
	f.calls = append(f.calls, "expire")
	if len(f.batches) == 0 {
		return nil, nil
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return b, nil
}

func (f *fakeSweepTx) LockTicketTypesOfOrders(_ context.Context, ids []pgtype.UUID) error {
	f.calls = append(f.calls, "lock")
	f.locked = append(f.locked, ids)
	return nil
}

func (f *fakeSweepTx) ReleaseStockForOrders(_ context.Context, ids []pgtype.UUID) error {
	f.calls = append(f.calls, "release")
	f.released = append(f.released, ids)
	return f.releaseErr
}

func (f *fakeSweepTx) DeleteOldIdempotencyKeys(_ context.Context, arg db.DeleteOldIdempotencyKeysParams) (int64, error) {
	f.deleteParams = append(f.deleteParams, arg)
	if len(f.deleteResults) == 0 {
		return 0, nil
	}
	n := f.deleteResults[0]
	f.deleteResults = f.deleteResults[1:]
	return n, nil
}

type fakeSweepStore struct {
	tx  *fakeSweepTx
	txs atomic.Int32
}

func (s *fakeSweepStore) InSweepTx(_ context.Context, fn func(SweepTx) error) error {
	s.txs.Add(1)
	return fn(s.tx)
}

func idList(n int) []pgtype.UUID {
	out := make([]pgtype.UUID, n)
	for i := range out {
		out[i] = pgtype.UUID{Bytes: [16]byte{byte(i), byte(i >> 8), 1}, Valid: true}
	}
	return out
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestABatchExpiresThenLocksThenReleasesTheSameOrders(t *testing.T) {
	tx := &fakeSweepTx{batches: [][]pgtype.UUID{idList(3)}}

	n, err := NewSweeper(&fakeSweepStore{tx: tx}, SweepConfig{BatchSize: 100}, quietLogger()).sweepBatch(context.Background())

	if err != nil || n != 3 {
		t.Fatalf("want 3 orders expired, got %d (%v)", n, err)
	}
	if want := []string{"expire", "lock", "release"}; len(tx.calls) != 3 || tx.calls[0] != want[0] || tx.calls[1] != want[1] || tx.calls[2] != want[2] {
		t.Fatalf("stock must be locked, in order, before it is released: %v", tx.calls)
	}
	if len(tx.locked[0]) != 3 || len(tx.released[0]) != 3 || tx.locked[0][0] != tx.released[0][0] {
		t.Fatal("exactly the orders that were expired must have their stock released")
	}
}

func TestANothingDueBatchTouchesNoStock(t *testing.T) {
	tx := &fakeSweepTx{}

	n, err := NewSweeper(&fakeSweepStore{tx: tx}, SweepConfig{}, quietLogger()).sweepBatch(context.Background())

	if err != nil || n != 0 || len(tx.calls) != 1 || tx.calls[0] != "expire" {
		t.Fatalf("an empty batch must stop after the claim, got %d %v %v", n, tx.calls, err)
	}
}

func TestSweepOnceDrainsABacklogUntilAShortBatch(t *testing.T) {
	tx := &fakeSweepTx{batches: [][]pgtype.UUID{idList(100), idList(100), idList(3)}}

	total, err := NewSweeper(&fakeSweepStore{tx: tx}, SweepConfig{BatchSize: 100}, quietLogger()).SweepOnce(context.Background())

	if err != nil || total != 203 {
		t.Fatalf("want 203, got %d (%v)", total, err)
	}
	expires := 0
	for _, c := range tx.calls {
		if c == "expire" {
			expires++
		}
	}
	if expires != 3 {
		t.Fatalf("the short third batch must end the sweep, but expire ran %d times", expires)
	}
}

func TestSweepOnceIsBoundedPerTick(t *testing.T) {
	batches := make([][]pgtype.UUID, 10)
	for i := range batches {
		batches[i] = idList(2) // always a full batch: an endless backlog
	}
	tx := &fakeSweepTx{batches: batches}

	total, err := NewSweeper(&fakeSweepStore{tx: tx}, SweepConfig{BatchSize: 2, MaxBatchesPerTick: 5}, quietLogger()).SweepOnce(context.Background())

	if err != nil || total != 10 {
		t.Fatalf("a tick may process at most 5 batches of 2, got %d (%v)", total, err)
	}
}

func TestAFailedBatchSurfacesTheErrorAndCountsOnlyCommittedWork(t *testing.T) {
	tx := &fakeSweepTx{batches: [][]pgtype.UUID{idList(2), idList(2)}}
	sw := NewSweeper(&fakeSweepStore{tx: tx}, SweepConfig{BatchSize: 2}, quietLogger())

	if n, err := sw.sweepBatch(context.Background()); err != nil || n != 2 {
		t.Fatalf("the first batch should succeed, got %d (%v)", n, err)
	}
	tx.releaseErr = errors.New("deadlock detected")
	total, err := sw.SweepOnce(context.Background())

	if err == nil || total != 0 {
		t.Fatalf("a failed batch must return its error and add nothing to the total, got %d (%v)", total, err)
	}
}

func TestCleanupKeysDeletesInBatches(t *testing.T) {
	tx := &fakeSweepTx{deleteResults: []int64{1000, 1000, 7}}

	total, err := NewSweeper(&fakeSweepStore{tx: tx}, SweepConfig{}, quietLogger()).CleanupKeys(context.Background())

	if err != nil || total != 2007 || len(tx.deleteParams) != 3 {
		t.Fatalf("want 2007 deleted over 3 statements, got %d over %d (%v)", total, len(tx.deleteParams), err)
	}
	if tx.deleteParams[0].MaxAgeHours != 24 || tx.deleteParams[0].BatchSize != 1000 {
		t.Fatalf("unexpected defaults: %+v", tx.deleteParams[0])
	}
}

func TestDefaults(t *testing.T) {
	got := NewSweeper(nil, SweepConfig{}, quietLogger()).cfg

	if got.Interval != 10*time.Second || got.BatchSize != 100 || got.MaxBatchesPerTick != 50 ||
		got.KeyMaxAge != 24*time.Hour || got.KeyCleanupEvery != time.Hour || got.KeyBatchSize != 1000 {
		t.Fatalf("unexpected defaults: %+v", got)
	}
}

func TestRunStopsWhenTheContextEnds(t *testing.T) {
	store := &fakeSweepStore{tx: &fakeSweepTx{}}
	sw := NewSweeper(store, SweepConfig{Interval: 5 * time.Millisecond}, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sw.Run(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for store.txs.Load() < 3 {
		select {
		case <-deadline:
			t.Fatal("the loop never ticked")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context ended")
	}
}