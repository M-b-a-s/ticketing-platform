//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/orders"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/tests/integration/testdb"
)

func newSweeper(pool *pgxpool.Pool, batch int32) *orders.Sweeper {
	return orders.NewSweeper(orders.NewPgStore(pool),
		orders.SweepConfig{BatchSize: batch, Interval: 25 * time.Millisecond, KeyBatchSize: 2},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func lineOf(tt db.TicketType, qty int) orders.Line { return orders.Line{TicketTypeID: tt.ID, Quantity: qty} }

func placeOrder(t *testing.T, svc *orders.Service, buyer, event pgtype.UUID, lines ...orders.Line) orders.View {
	t.Helper()
	view, err := svc.Create(ctx, orders.CreateInput{UserID: buyer, EventID: event, Lines: lines})
	if err != nil {
		t.Fatalf("placing an order: %v", err)
	}
	return view
}

// expireEverything puts every order's hold in the past, whatever its status.
func expireEverything(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE orders SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
}

func stockLeft(t *testing.T, q *db.Queries, event, ticketType pgtype.UUID) int32 {
	t.Helper()
	tt, err := q.GetTicketType(ctx, db.GetTicketTypeParams{ID: ticketType, EventID: event})
	if err != nil {
		t.Fatal(err)
	}
	return tt.AvailableQuantity
}

func statusCounts(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT status, count(*)::int FROM orders GROUP BY status`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			t.Fatal(err)
		}
		counts[status] = n
	}
	return counts
}

// The general law: tickets still available + tickets held by live orders = stock.
// Orders that expired or were cancelled hold nothing.
func assertLiveStock(t *testing.T, pool *pgxpool.Pool, q *db.Queries, event, ticketType pgtype.UUID, stock int) {
	t.Helper()
	var held int
	err := pool.QueryRow(ctx, `SELECT COALESCE(sum(i.quantity), 0)::int
		FROM order_items i JOIN orders o ON o.id = i.order_id
		WHERE i.ticket_type_id = $1 AND o.status IN ('pending', 'paid', 'fulfilled')`, ticketType).Scan(&held)
	if err != nil {
		t.Fatal(err)
	}
	left := int(stockLeft(t, q, event, ticketType))
	if left < 0 || held+left != stock {
		t.Fatalf("the books do not balance: %d held by live orders + %d available vs %d stock", held, left, stock)
	}
}

func TestSweeperReturnsExpiredStockAndNothingElse(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	event, types := onSale(t, pool, q, "sweep", 125, "General")
	tt := types[0]
	buyers := makeBuyers(t, q, 102)
	svc := orders.NewService(orders.NewPgStore(pool))

	for _, b := range buyers[:100] { // these will be left to expire
		placeOrder(t, svc, b, event, lineOf(tt, 1))
	}
	paid := placeOrder(t, svc, buyers[100], event, lineOf(tt, 1))
	cancelled := placeOrder(t, svc, buyers[101], event, lineOf(tt, 1))
	if _, err := pool.Exec(ctx, `UPDATE orders SET status = 'paid', paid_at = now() WHERE id = $1`, paid.Order.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Cancel(ctx, buyers[101], cancelled.Order.ID); err != nil {
		t.Fatal(err)
	}

	expireEverything(t, pool) // even the paid and the cancelled order now have a past deadline
	for _, b := range buyers[:20] { // 20 fresh orders, still within their hold
		placeOrder(t, svc, b, event, lineOf(tt, 1))
	}

	n, err := newSweeper(pool, 30).SweepOnce(ctx) // four batches
	if err != nil || n != 100 {
		t.Fatalf("want exactly the 100 abandoned orders expired, got %d (%v)", n, err)
	}

	counts := statusCounts(t, pool)
	if counts["expired"] != 100 || counts["pending"] != 20 || counts["paid"] != 1 || counts["cancelled"] != 1 {
		t.Fatalf("unexpected statuses: %v", counts)
	}
	assertLiveStock(t, pool, q, event, tt.ID, 125) // 20 pending + 1 paid still held; the rest back
	if got := stockLeft(t, q, event, tt.ID); got != 104 {
		t.Fatalf("want 104 available, got %d", got)
	}

	if n, err := newSweeper(pool, 30).SweepOnce(ctx); err != nil || n != 0 {
		t.Fatalf("a second sweep must find nothing, got %d (%v)", n, err)
	}
}

// Sweepers running together, while buyers keep buying the same ticket types in the
// opposite order, must neither double-release, lose stock, nor deadlock.
func TestSweepersAndBuyersTogetherLoseNothing(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	event, types := onSale(t, pool, q, "mix", 200, "General", "VIP")
	a, b := types[0], types[1]
	buyers := makeBuyers(t, q, 200)
	svc := orders.NewService(orders.NewPgStore(pool))

	for _, buyer := range buyers[:100] {
		placeOrder(t, svc, buyer, event, lineOf(a, 1), lineOf(b, 1))
	}
	expireEverything(t, pool)

	var swept atomic.Int64
	var wg sync.WaitGroup
	sweepErrs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n, err := newSweeper(pool, 10).SweepOnce(ctx)
			swept.Add(int64(n))
			sweepErrs[i] = err
		}(i)
	}

	created, refused, unexpected := stampede(svc, 100, func(i int) orders.CreateInput {
		return orders.CreateInput{UserID: buyers[100+i], EventID: event, Lines: []orders.Line{lineOf(b, 1), lineOf(a, 1)}}
	})
	wg.Wait()

	for i, err := range sweepErrs {
		if err != nil {
			t.Errorf("sweeper %d failed (a deadlock shows up here as SQLSTATE 40P01): %v", i, err)
		}
	}
	for _, err := range unexpected {
		t.Errorf("a buyer hit an unexpected error: %v", err)
	}
	if swept.Load() != 100 || created != 100 || len(refused) != 0 {
		t.Fatalf("want 100 swept and 100 bought with no refusals, got %d, %d, %v", swept.Load(), created, refused)
	}
	counts := statusCounts(t, pool)
	if counts["expired"] != 100 || counts["pending"] != 100 {
		t.Fatalf("unexpected statuses: %v", counts)
	}
	assertLiveStock(t, pool, q, event, a.ID, 200)
	assertLiveStock(t, pool, q, event, b.ID, 200)
}

// A buyer cancelling while the sweeper expires the same order: whoever wins the status
// change releases the stock, so each order's tickets come back exactly once.
func TestASweepRacingBuyerCancelsReleasesEachOrderOnce(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	event, types := onSale(t, pool, q, "race", 50, "General")
	tt := types[0]
	buyers := makeBuyers(t, q, 50)
	svc := orders.NewService(orders.NewPgStore(pool))

	views := make([]orders.View, len(buyers))
	for i, b := range buyers {
		views[i] = placeOrder(t, svc, b, event, lineOf(tt, 1))
	}
	expireEverything(t, pool)

	var wg sync.WaitGroup
	cancelErrs := make([]error, len(buyers))
	sweepErrs := make([]error, 2)
	for i := range buyers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, cancelErrs[i] = svc.Cancel(ctx, buyers[i], views[i].Order.ID)
		}(i)
	}
	for i := range sweepErrs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, sweepErrs[i] = newSweeper(pool, 5).SweepOnce(ctx)
		}(i)
	}
	wg.Wait()

	for i, err := range cancelErrs {
		var ce *orders.ConflictError
		if err != nil && !(errors.As(err, &ce) && ce.Code == "order_expired") {
			t.Errorf("cancel %d: %v (only success or order_expired are acceptable)", i, err)
		}
	}
	for i, err := range sweepErrs {
		if err != nil {
			t.Errorf("sweeper %d: %v", i, err)
		}
	}
	counts := statusCounts(t, pool)
	if counts["cancelled"]+counts["expired"] != 50 || counts["pending"] != 0 {
		t.Fatalf("every order must end up cancelled or expired, got %v", counts)
	}
	assertLiveStock(t, pool, q, event, tt.ID, 50)
	if got := stockLeft(t, q, event, tt.ID); got != 50 { // a double release would also trip the available <= total CHECK
		t.Fatalf("all 50 tickets must be back exactly once, got %d", got)
	}
}

func TestKeyCleanupDeletesOnlyOldKeys(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	user := createUser(t, q, "keys@example.com")
	insert := func(key string, ageHours int) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO idempotency_keys (user_id, scope, key, request_hash, response_status, response_body, created_at)
			VALUES ($1, 'create_order', $2, 'hash', 201, '{}'::jsonb, now() - ($3::int * interval '1 hour'))`, user, key, ageHours)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"old-1", "old-2", "old-3", "old-4", "old-5"} {
		insert(k, 48)
	}
	for _, k := range []string{"new-1", "new-2", "new-3"} {
		insert(k, 1)
	}

	n, err := newSweeper(pool, 100).CleanupKeys(ctx) // batches of 2: three statements

	if err != nil || n != 5 {
		t.Fatalf("want the 5 old keys deleted, got %d (%v)", n, err)
	}
	if left := countRows(t, pool, `SELECT count(*) FROM idempotency_keys`); left != 3 {
		t.Fatalf("the 3 recent keys must survive, %d left", left)
	}
}

// The loop itself: it releases stock without being asked, and stops when told to.
func TestTheSweeperLoopReleasesStockAndStopsOnShutdown(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	event, types := onSale(t, pool, q, "loop", 10, "General")
	buyer := makeBuyers(t, q, 1)[0]
	svc := orders.NewService(orders.NewPgStore(pool))

	placeOrder(t, svc, buyer, event, lineOf(types[0], 3))
	if stockLeft(t, q, event, types[0].ID) != 7 {
		t.Fatal("the order should hold 3 tickets")
	}
	expireEverything(t, pool)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		newSweeper(pool, 100).Run(runCtx)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for stockLeft(t, q, event, types[0].ID) != 10 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the loop never released the expired order's stock")
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after shutdown")
	}
	if counts := statusCounts(t, pool); counts["expired"] != 1 {
		t.Fatalf("the order should be expired, got %v", counts)
	}
}