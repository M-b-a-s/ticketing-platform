//go:build integration

package integration

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/tests/integration/testdb"
)

func newTT(t *testing.T, q *db.Queries, event pgtype.UUID, name string) db.TicketType {
	t.Helper()
	tt, err := q.CreateTicketType(ctx, db.CreateTicketTypeParams{
		EventID: event, Name: name, PriceKobo: 500000, Currency: "NGN", TotalQuantity: 100, MaxPerOrder: 10,
		SalesStart: ts(time.Now().Add(-time.Hour)), SalesEnd: ts(time.Now().Add(24 * time.Hour)),
	})
	if err != nil {
		t.Fatalf("create ticket type %q: %v", name, err)
	}
	return tt
}

// reserve mimics what Stage 4's reservation will do: take stock only if enough remains.
func reserve(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID, n int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE ticket_types SET available_quantity = available_quantity - $2 WHERE id = $1 AND available_quantity >= $2`, id, n); err != nil {
		t.Fatal(err)
	}
}

func total(n int) pgtype.Int4 { return pgtype.Int4{Int32: int32(n), Valid: true} }

func TestTicketTypeConstraints(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")
	a := insertEventAt(t, pool, owner, "a", time.Now().Add(48*time.Hour))
	b := insertEventAt(t, pool, owner, "b", time.Now().Add(49*time.Hour))

	tt := newTT(t, q, a, "General")
	if tt.AvailableQuantity != 100 || tt.TotalQuantity != 100 {
		t.Fatalf("a new type starts with all stock available, got %d of %d", tt.AvailableQuantity, tt.TotalQuantity)
	}

	t.Run("names are unique per event", func(t *testing.T) {
		_, err := q.CreateTicketType(ctx, db.CreateTicketTypeParams{
			EventID: a, Name: "General", PriceKobo: 1, Currency: "NGN", TotalQuantity: 1, MaxPerOrder: 1,
			SalesStart: ts(time.Now()), SalesEnd: ts(time.Now().Add(time.Hour)),
		})
		if pgErrCode(err) != "23505" {
			t.Fatalf("want a unique violation, got %v", err)
		}
		newTT(t, q, b, "General") // the same name on another event is fine
	})

	t.Run("the database refuses nonsense", func(t *testing.T) {
		_, err := q.CreateTicketType(ctx, db.CreateTicketTypeParams{
			EventID: a, Name: "Backwards", PriceKobo: 1, Currency: "NGN", TotalQuantity: 1, MaxPerOrder: 1,
			SalesStart: ts(time.Now().Add(time.Hour)), SalesEnd: ts(time.Now()),
		})
		if pgErrCode(err) != "23514" {
			t.Fatalf("a sales window that ends before it starts must violate a CHECK, got %v", err)
		}
	})
}

func TestUpdateTicketTypeAccounting(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")
	ev := insertEventAt(t, pool, owner, "e", time.Now().Add(48*time.Hour))
	other := insertEventAt(t, pool, owner, "other", time.Now().Add(49*time.Hour))
	tt := newTT(t, q, ev, "General")
	reserve(t, pool, tt.ID, 30) // 30 sold or reserved: 100 total, 70 available

	update := func(p db.UpdateTicketTypeParams) (db.TicketType, error) {
		p.ID, p.EventID = tt.ID, ev
		return q.UpdateTicketType(ctx, p)
	}

	t.Run("raising the total raises availability by the same amount", func(t *testing.T) {
		got, err := update(db.UpdateTicketTypeParams{TotalQuantity: total(150)})
		if err != nil || got.TotalQuantity != 150 || got.AvailableQuantity != 120 {
			t.Fatalf("want 150 total and 120 available, got %d and %d (%v)", got.TotalQuantity, got.AvailableQuantity, err)
		}
	})

	t.Run("a total below what sold is refused by the database", func(t *testing.T) {
		_, err := update(db.UpdateTicketTypeParams{TotalQuantity: total(20)})
		if pgErrCode(err) != "23514" {
			t.Fatalf("want a check violation, got %v", err)
		}
	})

	t.Run("a price cannot change once tickets are sold", func(t *testing.T) {
		_, err := update(db.UpdateTicketTypeParams{PriceKobo: pgtype.Int8{Int64: 600000, Valid: true}})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("want pgx.ErrNoRows, got %v", err)
		}
		if _, err := update(db.UpdateTicketTypeParams{PriceKobo: pgtype.Int8{Int64: 500000, Valid: true}}); err != nil {
			t.Fatalf("re-sending the same price must be allowed, got %v", err)
		}
	})

	t.Run("a type is only reachable through its own event", func(t *testing.T) {
		_, err := q.UpdateTicketType(ctx, db.UpdateTicketTypeParams{ID: tt.ID, EventID: other, Name: pgtype.Text{String: "hijacked", Valid: true}})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("want pgx.ErrNoRows, got %v", err)
		}
		if _, err := q.GetTicketType(ctx, db.GetTicketTypeParams{ID: tt.ID, EventID: other}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("reading through the wrong event must find nothing, got %v", err)
		}
	})

	t.Run("an unsold type can be repriced", func(t *testing.T) {
		fresh := newTT(t, q, ev, "VIP")
		got, err := q.UpdateTicketType(ctx, db.UpdateTicketTypeParams{ID: fresh.ID, EventID: ev, PriceKobo: pgtype.Int8{Int64: 900000, Valid: true}})
		if err != nil || got.PriceKobo != 900000 {
			t.Fatalf("got %v %v", got.PriceKobo, err)
		}
	})
}

// Stock moves by relative arithmetic, so a total edit racing with reservations
// loses none of them.
func TestRaisingTheTotalWhileReservationsRun(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")
	ev := insertEventAt(t, pool, owner, "e", time.Now().Add(48*time.Hour))
	tt := newTT(t, q, ev, "General") // 100 of 100

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reserve(t, pool, tt.ID, 1)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := q.UpdateTicketType(ctx, db.UpdateTicketTypeParams{ID: tt.ID, EventID: ev, TotalQuantity: total(120)}); err != nil {
			t.Errorf("raising the total failed: %v", err)
		}
	}()
	wg.Wait()

	got, err := q.GetTicketType(ctx, db.GetTicketTypeParams{ID: tt.ID, EventID: ev})
	if err != nil || got.TotalQuantity != 120 || got.AvailableQuantity != 70 {
		t.Fatalf("want 120 total and 70 available (120 - 50 reserved), got %d and %d (%v)", got.TotalQuantity, got.AvailableQuantity, err)
	}
}

func TestDeleteTicketTypeRules(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")
	ev := insertEventAt(t, pool, owner, "e", time.Now().Add(48*time.Hour))
	elsewhere := insertEventAt(t, pool, owner, "elsewhere", time.Now().Add(49*time.Hour))

	del := func(id, event pgtype.UUID) int64 {
		n, err := q.DeleteTicketType(ctx, db.DeleteTicketTypeParams{ID: id, EventID: event})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	unsold, sold, spare := newTT(t, q, ev, "unsold"), newTT(t, q, ev, "sold"), newTT(t, q, ev, "spare")
	reserve(t, pool, sold.ID, 1)

	if got := del(sold.ID, ev); got != 0 {
		t.Fatalf("a type with sales must not be deleted, deleted %d", got)
	}
	if got := del(spare.ID, elsewhere); got != 0 {
		t.Fatalf("deleting through the wrong event must do nothing, deleted %d", got)
	}
	if got := del(unsold.ID, ev); got != 1 {
		t.Fatalf("an unsold type of a draft event must delete, deleted %d", got)
	}

	if _, err := q.PublishEvent(ctx, ev); err != nil { // `sold` keeps the event publishable
		t.Fatal(err)
	}
	if got := del(spare.ID, ev); got != 0 {
		t.Fatalf("a published event keeps its ticket types, deleted %d", got)
	}
}

func TestMaxTicketSalesEnd(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")
	ev := insertEventAt(t, pool, owner, "e", time.Now().Add(48*time.Hour))

	if got, err := q.MaxTicketSalesEnd(ctx, ev); err != nil || got.Valid {
		t.Fatalf("an event without ticket types has no sales end, got %+v %v", got, err)
	}

	early, late := time.Now().Add(24*time.Hour).Truncate(time.Second), time.Now().Add(30*time.Hour).Truncate(time.Second)
	for name, end := range map[string]time.Time{"early": early, "late": late} {
		if _, err := q.CreateTicketType(ctx, db.CreateTicketTypeParams{
			EventID: ev, Name: name, PriceKobo: 1, Currency: "NGN", TotalQuantity: 1, MaxPerOrder: 1,
			SalesStart: ts(time.Now().Add(-time.Hour)), SalesEnd: ts(end),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := q.MaxTicketSalesEnd(ctx, ev); err != nil || !got.Time.Equal(late) {
		t.Fatalf("want the later sales end, got %v %v", got.Time, err)
	}
}

func TestPublicTicketTypesFollowTheEventLifecycle(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")
	ev, err := q.CreateEvent(ctx, newEventParams(owner))
	if err != nil {
		t.Fatal(err)
	}
	newTT(t, q, ev.ID, "General")

	count := func() int {
		rows, err := q.ListPublicTicketTypes(ctx, ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}

	if count() != 0 {
		t.Fatal("a draft's ticket types must not be public")
	}
	if _, err := q.PublishEvent(ctx, ev.ID); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatal("a published event's ticket types must be public")
	}
	if _, err := q.CancelEvent(ctx, ev.ID); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatal("a cancelled event has nothing on sale")
	}
}