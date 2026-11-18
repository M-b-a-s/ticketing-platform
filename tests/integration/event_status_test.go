//go:build integration

package integration

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/tests/integration/testdb"
)

func insertTicketType(t *testing.T, pool *pgxpool.Pool, event pgtype.UUID, name string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO ticket_types (event_id, name, price_kobo, total_quantity, available_quantity, sales_start, sales_end)
		 VALUES ($1, $2, 500000, 100, 100, now(), now() + interval '30 days')`, event, name)
	if err != nil {
		t.Fatalf("insert ticket type %q: %v", name, err)
	}
}

func TestPublishEventRules(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")
	draft, err := q.CreateEvent(ctx, newEventParams(owner))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a draft without ticket types cannot publish", func(t *testing.T) {
		if _, err := q.PublishEvent(ctx, draft.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("want pgx.ErrNoRows, got %v", err)
		}
	})

	insertTicketType(t, pool, draft.ID, "General")

	t.Run("a ready draft publishes", func(t *testing.T) {
		ev, err := q.PublishEvent(ctx, draft.ID)
		if err != nil || ev.Status != "published" {
			t.Fatalf("want a published event, got %q %v", ev.Status, err)
		}
	})

	t.Run("only drafts publish", func(t *testing.T) {
		if _, err := q.PublishEvent(ctx, draft.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a second publish must match nothing, got %v", err)
		}
	})

	t.Run("an event that already started cannot publish", func(t *testing.T) {
		past := insertEventAt(t, pool, owner, "past", time.Now().Add(-2*time.Hour))
		insertTicketType(t, pool, past, "General")

		if _, err := q.PublishEvent(ctx, past); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("want pgx.ErrNoRows, got %v", err)
		}
	})
}

func TestCountTicketTypesIsPerEvent(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")
	a := insertEventAt(t, pool, owner, "a", time.Now().Add(48*time.Hour))
	b := insertEventAt(t, pool, owner, "b", time.Now().Add(49*time.Hour))
	empty := insertEventAt(t, pool, owner, "empty", time.Now().Add(50*time.Hour))
	insertTicketType(t, pool, a, "General")
	insertTicketType(t, pool, a, "VIP")
	insertTicketType(t, pool, b, "General")

	for id, want := range map[pgtype.UUID]int64{a: 2, b: 1, empty: 0} {
		if got, err := q.CountTicketTypes(ctx, id); err != nil || got != want {
			t.Fatalf("want %d ticket types, got %d (%v)", want, got, err)
		}
	}
}

func TestCancelEventRules(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")

	newDraft := func(title string) pgtype.UUID {
		return insertEventAt(t, pool, owner, title, time.Now().Add(48*time.Hour))
	}

	t.Run("a draft can be cancelled", func(t *testing.T) {
		ev, err := q.CancelEvent(ctx, newDraft("d"))
		if err != nil || ev.Status != "cancelled" {
			t.Fatalf("got %q %v", ev.Status, err)
		}
	})

	t.Run("a published event can be cancelled, once", func(t *testing.T) {
		id := newDraft("p")
		insertTicketType(t, pool, id, "General")
		if _, err := q.PublishEvent(ctx, id); err != nil {
			t.Fatal(err)
		}

		if ev, err := q.CancelEvent(ctx, id); err != nil || ev.Status != "cancelled" {
			t.Fatalf("got %q %v", ev.Status, err)
		}
		if _, err := q.CancelEvent(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a second cancel must match nothing, got %v", err)
		}
	})

	t.Run("a completed event cannot be cancelled", func(t *testing.T) {
		id := newDraft("c")
		if _, err := pool.Exec(ctx, `UPDATE events SET status = 'completed' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := q.CancelEvent(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("want pgx.ErrNoRows, got %v", err)
		}
	})

	t.Run("a cancelled event can never be published", func(t *testing.T) {
		id := newDraft("x")
		insertTicketType(t, pool, id, "General")
		if _, err := q.CancelEvent(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := q.PublishEvent(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("want pgx.ErrNoRows, got %v", err)
		}
	})
}

// Two organizers double-clicking "publish" must not both win.
func TestPublishEventOnlyOneConcurrentCallerWins(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")
	ev, err := q.CreateEvent(ctx, newEventParams(owner))
	if err != nil {
		t.Fatal(err)
	}
	insertTicketType(t, pool, ev.ID, "General")

	const racers = 20
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := q.PublishEvent(ctx, ev.ID)
			switch {
			case err == nil:
				wins.Add(1)
			case !errors.Is(err, pgx.ErrNoRows):
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("exactly one publish must win, got %d", got)
	}
}