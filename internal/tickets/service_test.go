package tickets

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

var (
	eventA = pgtype.UUID{Bytes: [16]byte{0xa}, Valid: true}
	eventB = pgtype.UUID{Bytes: [16]byte{0xb}, Valid: true}
	typeID = pgtype.UUID{Bytes: [16]byte{7}, Valid: true}
)

type fakeStore struct {
	event    db.Event
	eventErr error

	publicEvent      db.Event
	publicErr        error
	publicTypes      []db.TicketType
	publicListCalled bool

	types []db.TicketType

	createGot    db.CreateTicketTypeParams
	createCalled bool
	createErr    error

	updateGot    db.UpdateTicketTypeParams
	updateCalled bool
	updateErr    error

	deleteCalled bool
	deleteRows   int64
	deleteErr    error

	access    db.GetEventAccessRow
	accessErr error
}

func (f *fakeStore) GetEvent(context.Context, pgtype.UUID) (db.Event, error) {
	return f.event, f.eventErr
}

func (f *fakeStore) GetPublicEvent(context.Context, pgtype.UUID) (db.Event, error) {
	return f.publicEvent, f.publicErr
}

func (f *fakeStore) CreateTicketType(_ context.Context, arg db.CreateTicketTypeParams) (db.TicketType, error) {
	f.createCalled = true
	f.createGot = arg
	if f.createErr != nil {
		return db.TicketType{}, f.createErr
	}
	return db.TicketType{ID: typeID, EventID: arg.EventID, Name: arg.Name, PriceKobo: arg.PriceKobo, Currency: arg.Currency,
		TotalQuantity: arg.TotalQuantity, AvailableQuantity: arg.TotalQuantity, MaxPerOrder: arg.MaxPerOrder,
		SalesStart: arg.SalesStart, SalesEnd: arg.SalesEnd}, nil
}

func (f *fakeStore) ListTicketTypes(context.Context, pgtype.UUID) ([]db.TicketType, error) {
	return f.types, nil
}

// GetTicketType is scoped by event, exactly like the real query.
func (f *fakeStore) GetTicketType(_ context.Context, arg db.GetTicketTypeParams) (db.TicketType, error) {
	for _, t := range f.types {
		if t.ID == arg.ID && t.EventID == arg.EventID {
			return t, nil
		}
	}
	return db.TicketType{}, pgx.ErrNoRows
}

func (f *fakeStore) UpdateTicketType(_ context.Context, arg db.UpdateTicketTypeParams) (db.TicketType, error) {
	f.updateCalled = true
	f.updateGot = arg
	if f.updateErr != nil {
		return db.TicketType{}, f.updateErr
	}
	t, _ := f.GetTicketType(context.Background(), db.GetTicketTypeParams{ID: arg.ID, EventID: arg.EventID})
	if arg.Name.Valid {
		t.Name = arg.Name.String
	}
	return t, nil
}

func (f *fakeStore) DeleteTicketType(context.Context, db.DeleteTicketTypeParams) (int64, error) {
	f.deleteCalled = true
	return f.deleteRows, f.deleteErr
}

func (f *fakeStore) ListPublicTicketTypes(context.Context, pgtype.UUID) ([]db.TicketType, error) {
	f.publicListCalled = true
	return f.publicTypes, nil
}

func (f *fakeStore) GetEventAccess(context.Context, db.GetEventAccessParams) (db.GetEventAccessRow, error) {
	return f.access, f.accessErr
}

func newTestService(store Store) *Service {
	s := NewService(store)
	s.now = func() time.Time { return fixedNow }
	return s
}

// fixture: a draft event with one unsold ticket type.
func fixture() *fakeStore {
	tt := existing(100, 100)
	tt.ID, tt.EventID = typeID, eventA
	return &fakeStore{
		event:      db.Event{ID: eventA, Status: statusDraft, EndsAt: tstz(eventEnds)},
		types:      []db.TicketType{tt},
		deleteRows: 1,
	}
}

func asConflict(t *testing.T, err error, code string) {
	t.Helper()
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("want conflict %q, got %v", code, err)
	}
}

func asValidation(t *testing.T, err error, field string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields[field] == "" {
		t.Fatalf("want a validation error on %q, got %v", field, err)
	}
}

func TestCreate(t *testing.T) {
	store := fixture()

	tt, err := newTestService(store).Create(context.Background(), eventA, validInput())

	if err != nil || tt.Name != "General" {
		t.Fatalf("create failed: %v", err)
	}
	got := store.createGot
	if got.EventID != eventA || got.PriceKobo != 500000 || got.Currency != "NGN" || got.TotalQuantity != 100 || got.MaxPerOrder != 10 {
		t.Fatalf("unexpected insert: %+v", got)
	}
}

func TestCreateRules(t *testing.T) {
	t.Run("frozen events accept nothing", func(t *testing.T) {
		for _, status := range []string{"cancelled", "completed"} {
			store := fixture()
			store.event.Status = status
			_, err := newTestService(store).Create(context.Background(), eventA, validInput())
			asConflict(t, err, "event_not_editable")
			if store.createCalled {
				t.Fatal("no write for a frozen event")
			}
		}
	})
	t.Run("invalid input never reaches the store", func(t *testing.T) {
		store := fixture()
		in := validInput()
		in.Name = ""
		_, err := newTestService(store).Create(context.Background(), eventA, in)
		asValidation(t, err, "name")
		if store.createCalled {
			t.Fatal("no write for invalid input")
		}
	})
	t.Run("duplicate name", func(t *testing.T) {
		store := fixture()
		store.createErr = &pgconn.PgError{Code: "23505", ConstraintName: "ticket_types_event_name_uniq"}
		_, err := newTestService(store).Create(context.Background(), eventA, validInput())
		asConflict(t, err, "name_taken")
	})
	t.Run("unknown event", func(t *testing.T) {
		store := &fakeStore{eventErr: pgx.ErrNoRows}
		if _, err := newTestService(store).Create(context.Background(), eventA, validInput()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
}

func TestUpdate(t *testing.T) {
	ctx := context.Background()

	t.Run("sends only the provided fields", func(t *testing.T) {
		store := fixture()
		if _, err := newTestService(store).Update(ctx, eventA, typeID, TicketTypePatch{Name: ptr("  VIP  ")}); err != nil {
			t.Fatal(err)
		}
		got := store.updateGot
		if got.Name != (pgtype.Text{String: "VIP", Valid: true}) ||
			got.PriceKobo.Valid || got.TotalQuantity.Valid || got.MaxPerOrder.Valid || got.SalesStart.Valid || got.SalesEnd.Valid {
			t.Fatalf("untouched fields must stay NULL: %+v", got)
		}
	})
	t.Run("empty patch touches nothing", func(t *testing.T) {
		store := fixture()
		_, err := newTestService(store).Update(ctx, eventA, typeID, TicketTypePatch{})
		asValidation(t, err, "body")
		if store.updateCalled {
			t.Fatal("no write")
		}
	})
	t.Run("price is locked after sales", func(t *testing.T) {
		store := fixture()
		store.types[0].AvailableQuantity = 70
		_, err := newTestService(store).Update(ctx, eventA, typeID, TicketTypePatch{PriceKobo: ptr(int64(1))})
		asValidation(t, err, "price_kobo")
		if store.updateCalled {
			t.Fatal("no write")
		}
	})
	t.Run("total cannot drop below what sold", func(t *testing.T) {
		store := fixture()
		store.types[0].AvailableQuantity = 70
		_, err := newTestService(store).Update(ctx, eventA, typeID, TicketTypePatch{TotalQuantity: ptr(20)})
		asValidation(t, err, "total_quantity")
	})
	t.Run("a ticket type of another event is not found", func(t *testing.T) {
		store := fixture()
		store.types[0].EventID = eventB
		_, err := newTestService(store).Update(ctx, eventA, typeID, TicketTypePatch{Name: ptr("x")})
		if !errors.Is(err, ErrNotFound) || store.updateCalled {
			t.Fatalf("want ErrNotFound and no write, got %v", err)
		}
	})
	t.Run("a sale that lands mid-request", func(t *testing.T) {
		store := fixture()
		store.updateErr = pgx.ErrNoRows
		_, err := newTestService(store).Update(ctx, eventA, typeID, TicketTypePatch{PriceKobo: ptr(int64(1))})
		asConflict(t, err, "ticket_type_changed")
	})
	t.Run("the stock CHECK fires", func(t *testing.T) {
		store := fixture()
		store.updateErr = &pgconn.PgError{Code: "23514", ConstraintName: "ticket_types_available_nonneg"}
		_, err := newTestService(store).Update(ctx, eventA, typeID, TicketTypePatch{TotalQuantity: ptr(50)})
		asValidation(t, err, "total_quantity")
	})
	t.Run("frozen events", func(t *testing.T) {
		store := fixture()
		store.event.Status = "cancelled"
		_, err := newTestService(store).Update(ctx, eventA, typeID, TicketTypePatch{Name: ptr("x")})
		asConflict(t, err, "event_not_editable")
	})
}

func TestDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("an unsold type of a draft event", func(t *testing.T) {
		store := fixture()
		if err := newTestService(store).Delete(ctx, eventA, typeID); err != nil || !store.deleteCalled {
			t.Fatalf("want a delete, got %v called=%v", err, store.deleteCalled)
		}
	})

	cases := []struct {
		name   string
		mutate func(*fakeStore)
		code   string
	}{
		{"published events keep their types", func(s *fakeStore) { s.event.Status = statusPublished }, "event_published"},
		{"cancelled events are frozen", func(s *fakeStore) { s.event.Status = "cancelled" }, "event_not_editable"},
		{"a type with sales", func(s *fakeStore) { s.types[0].AvailableQuantity = 99 }, "ticket_type_has_sales"},
		{"zero rows deleted means it changed", func(s *fakeStore) { s.deleteRows = 0 }, "ticket_type_changed"},
		{"orders reference it", func(s *fakeStore) { s.deleteErr = &pgconn.PgError{Code: "23503"} }, "ticket_type_in_use"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := fixture()
			c.mutate(store)
			asConflict(t, newTestService(store).Delete(ctx, eventA, typeID), c.code)
		})
	}

	t.Run("another event's ticket type", func(t *testing.T) {
		store := fixture()
		store.types[0].EventID = eventB
		if err := newTestService(store).Delete(ctx, eventA, typeID); !errors.Is(err, ErrNotFound) || store.deleteCalled {
			t.Fatalf("want ErrNotFound and no delete, got %v", err)
		}
	})
}

func TestListPublic(t *testing.T) {
	ctx := context.Background()

	t.Run("an event that was never public is a 404", func(t *testing.T) {
		store := &fakeStore{publicErr: pgx.ErrNoRows}
		if _, err := newTestService(store).ListPublic(ctx, eventA); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
	t.Run("a cancelled event has nothing on sale", func(t *testing.T) {
		store := &fakeStore{publicEvent: db.Event{Status: "cancelled"}}
		rows, err := newTestService(store).ListPublic(ctx, eventA)
		if err != nil || len(rows) != 0 || store.publicListCalled {
			t.Fatalf("want an empty list without querying types, got %v %v", rows, err)
		}
	})
	t.Run("a published event lists its types", func(t *testing.T) {
		store := &fakeStore{publicEvent: db.Event{Status: statusPublished}, publicTypes: []db.TicketType{existing(10, 10)}}
		if rows, err := newTestService(store).ListPublic(ctx, eventA); err != nil || len(rows) != 1 {
			t.Fatalf("got %v %v", rows, err)
		}
	})
}