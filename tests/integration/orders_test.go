//go:build integration

package integration

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/orders"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/tests/integration/testdb"
)

// onSale builds a published event whose sales window is open, with one ticket type
// per name, each holding `stock` tickets.
func onSale(t *testing.T, pool *pgxpool.Pool, q *db.Queries, tag string, stock int32, names ...string) (pgtype.UUID, []db.TicketType) {
	t.Helper()
	organizer := createUser(t, q, "organizer-"+tag+"@example.com")
	now := time.Now()
	published := now
	event := insertEventRaw(t, pool, organizer, "On sale "+tag, "published", &published, now.Add(48*time.Hour), now.Add(50*time.Hour))

	var types []db.TicketType
	for _, name := range names {
		tt, err := q.CreateTicketType(ctx, db.CreateTicketTypeParams{
			EventID: event, Name: name, PriceKobo: 500000, Currency: "NGN", TotalQuantity: stock, MaxPerOrder: 10,
			SalesStart: ts(now.Add(-time.Hour)), SalesEnd: ts(now.Add(24 * time.Hour)),
		})
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, tt)
	}
	return event, types
}

func makeBuyers(t *testing.T, q *db.Queries, n int) []pgtype.UUID {
	t.Helper()
	buyers := make([]pgtype.UUID, n)
	for i := range buyers {
		buyers[i] = createUser(t, q, fmt.Sprintf("buyer-%d@example.com", i))
	}
	return buyers
}

// stampede releases n buyers at the same instant and tallies how each one fared.
func stampede(svc *orders.Service, n int, input func(i int) orders.CreateInput) (created int, refused map[string]int, unexpected []error) {
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = svc.Create(ctx, input(i))
		}(i)
	}
	close(start)
	wg.Wait()

	refused = map[string]int{}
	for _, err := range errs {
		var ce *orders.ConflictError
		switch {
		case err == nil:
			created++
		case errors.As(err, &ce):
			refused[ce.Code]++
		default:
			unexpected = append(unexpected, err)
		}
	}
	return
}

// assertStock checks the books balance: tickets sold plus tickets left equals the
// stock we started with, and nothing went negative.
func assertStock(t *testing.T, pool *pgxpool.Pool, q *db.Queries, event, ticketType pgtype.UUID, stock, wantSold int) {
	t.Helper()
	tt, err := q.GetTicketType(ctx, db.GetTicketTypeParams{ID: ticketType, EventID: event})
	if err != nil {
		t.Fatal(err)
	}
	var sold int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(quantity), 0)::int FROM order_items WHERE ticket_type_id = $1`, ticketType).Scan(&sold); err != nil {
		t.Fatal(err)
	}
	if tt.AvailableQuantity < 0 || sold+int(tt.AvailableQuantity) != stock || sold != wantSold {
		t.Fatalf("books do not balance: %d in orders + %d available vs %d stock (want %d sold)", sold, tt.AvailableQuantity, stock, wantSold)
	}
}

// THE PHASE 4 EXIT TEST: 10 tickets, 100 buyers, run with -race.
func TestTenTicketsHundredBuyers(t *testing.T) {
	for _, qty := range []int{1, 3} {
		t.Run(fmt.Sprintf("each buyer wants %d", qty), func(t *testing.T) {
			pool := testdb.New(t)
			q := db.New(pool)
			event, types := onSale(t, pool, q, "x", 10, "General")
			buyers := makeBuyers(t, q, 100)
			svc := orders.NewService(orders.NewPgStore(pool))

			created, refused, unexpected := stampede(svc, 100, func(i int) orders.CreateInput {
				return orders.CreateInput{UserID: buyers[i], EventID: event,
					Lines: []orders.Line{{TicketTypeID: types[0].ID, Quantity: qty}}}
			})

			wantOrders := 10 / qty // 10 orders of 1, or 3 orders of 3 with one ticket left over
			for _, err := range unexpected {
				t.Errorf("unexpected error: %v", err)
			}
			if created != wantOrders || refused["sold_out"] != 100-wantOrders {
				t.Fatalf("want %d orders and %d sold_out, got %d and %v", wantOrders, 100-wantOrders, created, refused)
			}
			assertStock(t, pool, q, event, types[0].ID, 10, wantOrders*qty)
		})
	}
}

// One buyer firing many requests at once must not slip past the pending-order cap.
func TestOneBuyerCannotHoardStock(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	event, types := onSale(t, pool, q, "x", 100, "General")
	buyer := makeBuyers(t, q, 1)[0]
	svc := orders.NewService(orders.NewPgStore(pool))

	created, refused, unexpected := stampede(svc, 10, func(int) orders.CreateInput {
		return orders.CreateInput{UserID: buyer, EventID: event, Lines: []orders.Line{{TicketTypeID: types[0].ID, Quantity: 1}}}
	})

	for _, err := range unexpected {
		t.Errorf("unexpected error: %v", err)
	}
	if created != orders.MaxPendingPerEvent || refused["too_many_pending_orders"] != 10-orders.MaxPendingPerEvent {
		t.Fatalf("want exactly %d orders, got %d (%v)", orders.MaxPendingPerEvent, created, refused)
	}
	assertStock(t, pool, q, event, types[0].ID, 100, orders.MaxPendingPerEvent)
}

// Buyers who ask for the same two ticket types in opposite orders must never deadlock.
func TestOrdersForSeveralTicketTypesDoNotDeadlock(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	event, types := onSale(t, pool, q, "x", 30, "General", "VIP")
	a, b := types[0].ID, types[1].ID
	buyers := makeBuyers(t, q, 60)
	svc := orders.NewService(orders.NewPgStore(pool))

	created, refused, unexpected := stampede(svc, 60, func(i int) orders.CreateInput {
		lines := []orders.Line{{TicketTypeID: a, Quantity: 1}, {TicketTypeID: b, Quantity: 1}}
		if i%2 == 1 {
			lines[0], lines[1] = lines[1], lines[0] // the opposite order
		}
		return orders.CreateInput{UserID: buyers[i], EventID: event, Lines: lines}
	})

	for _, err := range unexpected {
		t.Errorf("unexpected error (a deadlock shows up here as SQLSTATE 40P01): %v", err)
	}
	if created != 30 || refused["sold_out"] != 30 {
		t.Fatalf("want 30 orders and 30 sold_out, got %d and %v", created, refused)
	}
	assertStock(t, pool, q, event, a, 30, 30)
	assertStock(t, pool, q, event, b, 30, 30)
}

// A failed order must leave no trace: the stock taken for its first line comes back.
func TestFailedOrderLeavesNoTrace(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	event, types := onSale(t, pool, q, "x", 5, "A", "B")
	if _, err := pool.Exec(ctx, `UPDATE ticket_types SET available_quantity = 0 WHERE id = $1`, types[1].ID); err != nil {
		t.Fatal(err)
	}
	buyer := makeBuyers(t, q, 1)[0]
	svc := orders.NewService(orders.NewPgStore(pool))

	_, err := svc.Create(ctx, orders.CreateInput{UserID: buyer, EventID: event, Lines: []orders.Line{
		{TicketTypeID: types[0].ID, Quantity: 1}, {TicketTypeID: types[1].ID, Quantity: 1},
	}})

	var ce *orders.ConflictError
	if !errors.As(err, &ce) || ce.Code != "sold_out" {
		t.Fatalf("want sold_out, got %v", err)
	}
	tt, _ := q.GetTicketType(ctx, db.GetTicketTypeParams{ID: types[0].ID, EventID: event})
	if tt.AvailableQuantity != 5 {
		t.Fatalf("the rollback must return A's stock, %d of 5 left", tt.AvailableQuantity)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM orders) + (SELECT count(*) FROM order_items)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a failed order must leave no rows, found %d (%v)", n, err)
	}
}

// The selling rules are enforced by the database itself, not just described by the service.
func TestSellingRulesAreEnforcedInSQL(t *testing.T) {
	cases := []struct{ name, sql, wantCode string }{
		{"sales ended", `UPDATE ticket_types SET sales_start = now() - interval '3 hours', sales_end = now() - interval '1 hour' WHERE id = $1`, "sales_closed"},
		{"sales not started", `UPDATE ticket_types SET sales_start = now() + interval '1 hour', sales_end = now() + interval '2 hours' WHERE id = $1`, "sales_not_started"},
		{"over the per-order limit", `UPDATE ticket_types SET max_per_order = 1 WHERE id = $1`, "quantity_exceeds_limit"},
		{"cancelled event", `UPDATE events SET status = 'cancelled' WHERE id = (SELECT event_id FROM ticket_types WHERE id = $1)`, "event_not_on_sale"},
		{"event already over", `UPDATE events SET starts_at = now() - interval '5 hours', ends_at = now() - interval '1 hour' WHERE id = (SELECT event_id FROM ticket_types WHERE id = $1)`, "event_not_on_sale"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := testdb.New(t)
			q := db.New(pool)
			event, types := onSale(t, pool, q, "x", 10, "General")
			buyer := makeBuyers(t, q, 1)[0]
			if _, err := pool.Exec(ctx, c.sql, types[0].ID); err != nil {
				t.Fatal(err)
			}

			_, err := orders.NewService(orders.NewPgStore(pool)).Create(ctx, orders.CreateInput{
				UserID: buyer, EventID: event, Lines: []orders.Line{{TicketTypeID: types[0].ID, Quantity: 2}},
			})

			var ce *orders.ConflictError
			if !errors.As(err, &ce) || ce.Code != c.wantCode {
				t.Fatalf("want %q, got %v", c.wantCode, err)
			}
			assertStock(t, pool, q, event, types[0].ID, 10, 0) // nothing was taken
		})
	}

	t.Run("a draft event does not exist for buyers", func(t *testing.T) {
		pool := testdb.New(t)
		q := db.New(pool)
		event, types := onSale(t, pool, q, "x", 10, "General")
		buyer := makeBuyers(t, q, 1)[0]
		if _, err := pool.Exec(ctx, `UPDATE events SET status = 'draft' WHERE id = $1`, event); err != nil {
			t.Fatal(err)
		}

		_, err := orders.NewService(orders.NewPgStore(pool)).Create(ctx, orders.CreateInput{
			UserID: buyer, EventID: event, Lines: []orders.Line{{TicketTypeID: types[0].ID, Quantity: 1}},
		})
		if !errors.Is(err, orders.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("a ticket type from another event", func(t *testing.T) {
		pool := testdb.New(t)
		q := db.New(pool)
		_, types := onSale(t, pool, q, "one", 10, "General")
		otherEvent, _ := onSale(t, pool, q, "two", 10, "General")
		buyer := makeBuyers(t, q, 1)[0]

		_, err := orders.NewService(orders.NewPgStore(pool)).Create(ctx, orders.CreateInput{
			UserID: buyer, EventID: otherEvent, Lines: []orders.Line{{TicketTypeID: types[0].ID, Quantity: 1}},
		})
		var ve *orders.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want a validation error, got %v", err)
		}
	})
}