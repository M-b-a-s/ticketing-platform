package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

type fakeStore struct {
	// create
	called bool
	got    db.CreateEventParams

	// list
	rows    []db.ListManagedEventsRow
	listGot db.ListManagedEventsParams

	// get / update
	event        db.Event
	getErr       error
	getCalled    bool
	updateGot    db.UpdateEventParams
	updateErr    error
	updateCalled bool

	// route guard
	access    db.GetEventAccessRow
	accessErr error
}

func (f *fakeStore) CreateEvent(_ context.Context, arg db.CreateEventParams) (db.Event, error) {
	f.called = true
	f.got = arg
	return db.Event{
		ID:          pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		OrganizerID: arg.OrganizerID,
		Title:       arg.Title,
		VenueName:   arg.VenueName,
		StartsAt:    arg.StartsAt,
		EndsAt:      arg.EndsAt,
		Status:      StatusDraft,
	}, nil
}

func (f *fakeStore) ListManagedEvents(_ context.Context, arg db.ListManagedEventsParams) ([]db.ListManagedEventsRow, error) {
	f.listGot = arg
	return f.rows, nil
}

func (f *fakeStore) GetEvent(context.Context, pgtype.UUID) (db.Event, error) {
	f.getCalled = true
	return f.event, f.getErr
}

func (f *fakeStore) UpdateEvent(_ context.Context, arg db.UpdateEventParams) (db.Event, error) {
	f.updateCalled = true
	f.updateGot = arg
	if f.updateErr != nil {
		return db.Event{}, f.updateErr
	}
	ev := f.event
	if arg.Title.Valid {
		ev.Title = arg.Title.String
	}
	return ev, nil
}

func (f *fakeStore) GetEventAccess(context.Context, db.GetEventAccessParams) (db.GetEventAccessRow, error) {
	return f.access, f.accessErr
}



func newTestService(store Store) *Service {
	s := NewService(store)
	s.now = func() time.Time { return fixedNow }
	return s
}

var organizer = pgtype.UUID{Bytes: [16]byte{7}, Valid: true}

func TestCreateUsesCallerAsOwnerAndNormalizes(t *testing.T) {
	store := &fakeStore{}
	in := validInput()
	in.Title = "  Test Concert  "

	if _, err := newTestService(store).Create(context.Background(), organizer, in); err != nil {
		t.Fatal(err)
	}

	if store.got.OrganizerID != organizer {
		t.Fatal("the owner must be the caller passed to Create")
	}
	if store.got.Title != "Test Concert" {
		t.Fatalf("title should be trimmed, got %q", store.got.Title)
	}
	if store.got.Description.Valid {
		t.Fatal("an empty description should be stored as NULL")
	}
}

func TestCreateRejectsInvalidInputWithoutTouchingTheStore(t *testing.T) {
	store := &fakeStore{}
	in := validInput()
	in.Title = ""

	_, err := newTestService(store).Create(context.Background(), organizer, in)

	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields["title"] == "" {
		t.Fatalf("expected a validation error on title, got %v", err)
	}
	if store.called {
		t.Fatal("the store must not be called for invalid input")
	}
}