package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

// publishableStore is a draft starting in 48 hours with one ticket type.
func publishableStore() *fakeStore {
	s := storeWithEvent()
	s.count = 1
	return s
}

func statusEvent(status string) db.Event {
	ev := currentEvent()
	ev.Status = status
	return ev
}

func TestPublishHappyPath(t *testing.T) {
	store := publishableStore()

	ev, err := newTestService(store).Publish(context.Background(), eventID)

	if err != nil || ev.Status != StatusPublished || !store.publishCalled {
		t.Fatalf("want a published event and one write, got %v %v called=%v", ev.Status, err, store.publishCalled)
	}
}

func TestPublishRequirements(t *testing.T) {
	cases := map[string]struct {
		mutate func(*fakeStore)
		field  string
	}{
		"no ticket types": {func(s *fakeStore) { s.count = 0 }, "ticket_types"},
		"already started": {func(s *fakeStore) {
			s.event.StartsAt = pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}
		}, "starts_at"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			store := publishableStore()
			c.mutate(store)

			_, err := newTestService(store).Publish(context.Background(), eventID)

			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Fields[c.field] == "" || store.publishCalled {
				t.Fatalf("want a %q validation error and no write, got %v called=%v", c.field, err, store.publishCalled)
			}
		})
	}
}

func TestPublishIsIdempotent(t *testing.T) {
	store := &fakeStore{event: statusEvent(StatusPublished)} // count is 0 on purpose: nothing is re-validated

	ev, err := newTestService(store).Publish(context.Background(), eventID)

	if err != nil || ev.Status != StatusPublished || store.publishCalled {
		t.Fatalf("repeating publish must succeed without writing, got %v called=%v", err, store.publishCalled)
	}
}

func TestPublishRefusesFinalStates(t *testing.T) {
	for _, status := range []string{StatusCancelled, StatusCompleted} {
		store := &fakeStore{event: statusEvent(status), count: 1}

		_, err := newTestService(store).Publish(context.Background(), eventID)

		var te *TransitionError
		if !errors.As(err, &te) || te.From != status || te.To != StatusPublished || store.publishCalled {
			t.Fatalf("%s: want a TransitionError and no write, got %v", status, err)
		}
	}
}

func TestPublishLostRace(t *testing.T) {
	t.Run("another request published it first: success", func(t *testing.T) {
		store := publishableStore()
		store.getQueue = []db.Event{statusEvent(StatusDraft), statusEvent(StatusPublished)}
		store.publishErr = pgx.ErrNoRows

		ev, err := newTestService(store).Publish(context.Background(), eventID)

		if err != nil || ev.Status != StatusPublished {
			t.Fatalf("want success, got %v %v", ev.Status, err)
		}
	})

	t.Run("it was cancelled in between: conflict", func(t *testing.T) {
		store := publishableStore()
		store.getQueue = []db.Event{statusEvent(StatusDraft), statusEvent(StatusCancelled)}
		store.publishErr = pgx.ErrNoRows

		if _, err := newTestService(store).Publish(context.Background(), eventID); !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
	})
}

func TestPublishNotFound(t *testing.T) {
	store := &fakeStore{getErr: pgx.ErrNoRows}

	if _, err := newTestService(store).Publish(context.Background(), eventID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCancelFromDraftAndPublished(t *testing.T) {
	for _, status := range []string{StatusDraft, StatusPublished} {
		store := &fakeStore{event: statusEvent(status)}

		ev, err := newTestService(store).Cancel(context.Background(), eventID)

		if err != nil || ev.Status != StatusCancelled || !store.cancelCalled {
			t.Fatalf("%s: want a cancelled event and one write, got %v %v", status, ev.Status, err)
		}
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	store := &fakeStore{event: statusEvent(StatusCancelled)}

	ev, err := newTestService(store).Cancel(context.Background(), eventID)

	if err != nil || ev.Status != StatusCancelled || store.cancelCalled {
		t.Fatalf("repeating cancel must succeed without writing, got %v called=%v", err, store.cancelCalled)
	}
}

func TestCancelRefusesCompletedEvents(t *testing.T) {
	store := &fakeStore{event: statusEvent(StatusCompleted)}

	_, err := newTestService(store).Cancel(context.Background(), eventID)

	var te *TransitionError
	if !errors.As(err, &te) || store.cancelCalled {
		t.Fatalf("want a TransitionError and no write, got %v", err)
	}
}

func TestCancelLostRace(t *testing.T) {
	store := &fakeStore{
		getQueue:  []db.Event{statusEvent(StatusPublished), statusEvent(StatusCompleted)},
		cancelErr: pgx.ErrNoRows,
	}

	if _, err := newTestService(store).Cancel(context.Background(), eventID); !errors.Is(err, ErrConflict) {
		t.Fatalf("an event that completed in between is a conflict, got %v", err)
	}
}