//go:build integration

package integration

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/tests/integration/testdb"
)

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func newEventParams(organizer pgtype.UUID) db.CreateEventParams {
	return db.CreateEventParams{
		OrganizerID: organizer,
		Title:       "Original",
		VenueName:   "Venue",
		StartsAt:    ts(time.Now().Add(48 * time.Hour)),
		EndsAt:      ts(time.Now().Add(52 * time.Hour)),
	}
}

func TestEventQueries(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")

	ev, err := q.CreateEvent(ctx, newEventParams(owner))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != "draft" {
		t.Fatalf("new events must start as draft, got %q", ev.Status)
	}

	t.Run("update is partial", func(t *testing.T) {
		upd, err := q.UpdateEvent(ctx, db.UpdateEventParams{ID: ev.ID, Title: pgtype.Text{String: "Renamed", Valid: true}})
		if err != nil {
			t.Fatal(err)
		}
		if upd.Title != "Renamed" || upd.VenueName != "Venue" || !upd.EndsAt.Time.Equal(ev.EndsAt.Time) {
			t.Fatalf("only the title should change: %+v", upd)
		}
	})

	t.Run("database rejects an end before the start", func(t *testing.T) {
		_, err := q.UpdateEvent(ctx, db.UpdateEventParams{ID: ev.ID, StartsAt: ts(ev.EndsAt.Time.Add(time.Hour))})
		if pgErrCode(err) != "23514" { // check_violation
			t.Fatalf("expected the time-order CHECK to fire, got %v", err)
		}
	})

	t.Run("cancelled events are frozen", func(t *testing.T) {
		if _, err := q.TransitionEventStatus(ctx, db.TransitionEventStatusParams{ID: ev.ID, FromStatus: "draft", ToStatus: "cancelled"}); err != nil {
			t.Fatal(err)
		}
		_, err := q.UpdateEvent(ctx, db.UpdateEventParams{ID: ev.ID, Title: pgtype.Text{String: "Too late", Valid: true}})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a cancelled event must not be editable, got %v", err)
		}
	})
}

// Two organizers clicking "publish" at once must not both succeed.
func TestTransitionEventStatusOnlyOneConcurrentCallerWins(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")
	ev, err := q.CreateEvent(ctx, newEventParams(owner))
	if err != nil {
		t.Fatal(err)
	}

	const racers = 20
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := q.TransitionEventStatus(ctx, db.TransitionEventStatusParams{ID: ev.ID, FromStatus: "draft", ToStatus: "published"})
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
		t.Fatalf("exactly one transition must win, got %d", got)
	}
	if final, _ := q.GetEvent(ctx, ev.ID); final.Status != "published" {
		t.Fatalf("event should end up published, got %q", final.Status)
	}
}