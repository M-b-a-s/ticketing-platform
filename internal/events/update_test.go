package events

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

var eventID = pgtype.UUID{Bytes: [16]byte{5}, Valid: true}

func storeWithEvent() *fakeStore { return &fakeStore{event: currentEvent()} }

func TestUpdateSendsOnlyProvidedFields(t *testing.T) {
	store := storeWithEvent()

	if _, err := newTestService(store).Update(context.Background(), eventID, EventPatch{Title: ptr("  New  ")}); err != nil {
		t.Fatal(err)
	}

	got := store.updateGot
	if got.ID != eventID || got.Title != (pgtype.Text{String: "New", Valid: true}) {
		t.Fatalf("title should be trimmed and sent: %+v", got)
	}
	if got.VenueName.Valid || got.Description.Valid || got.StartsAt.Valid || got.EndsAt.Valid {
		t.Fatalf("untouched fields must stay NULL so the query keeps them: %+v", got)
	}
}

func TestUpdateCanClearTheDescription(t *testing.T) {
	store := storeWithEvent()

	if _, err := newTestService(store).Update(context.Background(), eventID, EventPatch{Description: ptr("")}); err != nil {
		t.Fatal(err)
	}
	if !store.updateGot.Description.Valid || store.updateGot.Description.String != "" {
		t.Fatalf("an explicit empty description must be sent as an empty value: %+v", store.updateGot.Description)
	}
}

func TestUpdateRefusesFrozenEvents(t *testing.T) {
	for _, status := range []string{StatusCancelled, StatusCompleted} {
		store := storeWithEvent()
		store.event.Status = status

		_, err := newTestService(store).Update(context.Background(), eventID, EventPatch{Title: ptr("x")})

		if !errors.Is(err, ErrNotEditable) || store.updateCalled {
			t.Fatalf("%s: want ErrNotEditable and no write, got %v called=%v", status, err, store.updateCalled)
		}
	}
}

func TestUpdateNotFound(t *testing.T) {
	store := &fakeStore{getErr: pgx.ErrNoRows}

	_, err := newTestService(store).Update(context.Background(), eventID, EventPatch{Title: ptr("x")})

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUpdateValidationBlocksTheWrite(t *testing.T) {
	store := storeWithEvent()

	_, err := newTestService(store).Update(context.Background(), eventID, EventPatch{Title: ptr("")})

	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields["title"] == "" || store.updateCalled {
		t.Fatalf("want a title validation error and no write, got %v called=%v", err, store.updateCalled)
	}
}

func TestUpdateEmptyPatchTouchesNothing(t *testing.T) {
	store := storeWithEvent()

	_, err := newTestService(store).Update(context.Background(), eventID, EventPatch{})

	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields["body"] == "" || store.getCalled || store.updateCalled {
		t.Fatalf("an empty patch must fail fast without any query, got %v", err)
	}
}

func TestUpdateLostRaceReportsNotEditable(t *testing.T) {
	store := storeWithEvent()
	store.updateErr = pgx.ErrNoRows // someone cancelled it between our read and our write

	_, err := newTestService(store).Update(context.Background(), eventID, EventPatch{Title: ptr("x")})

	if !errors.Is(err, ErrNotEditable) {
		t.Fatalf("want ErrNotEditable, got %v", err)
	}
}

func TestUpdateMapsTimeOrderConstraintToValidationError(t *testing.T) {
	store := storeWithEvent()
	store.updateErr = &pgconn.PgError{Code: "23514", ConstraintName: "events_time_order"}

	_, err := newTestService(store).Update(context.Background(), eventID, EventPatch{Title: ptr("x")})

	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields["ends_at"] == "" {
		t.Fatalf("want a validation error on ends_at, got %v", err)
	}
}

var _ = db.Event{} // keeps the import honest if you trim tests later