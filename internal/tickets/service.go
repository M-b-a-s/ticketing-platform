package tickets

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

const (
	statusDraft     = "draft"
	statusPublished = "published"
)

type Store interface {
	auth.EventAccessStore // GetEventAccess, used by the route guards
	GetEvent(ctx context.Context, id pgtype.UUID) (db.Event, error)
	GetPublicEvent(ctx context.Context, id pgtype.UUID) (db.Event, error)
	CreateTicketType(ctx context.Context, arg db.CreateTicketTypeParams) (db.TicketType, error)
	ListTicketTypes(ctx context.Context, eventID pgtype.UUID) ([]db.TicketType, error)
	GetTicketType(ctx context.Context, arg db.GetTicketTypeParams) (db.TicketType, error)
	UpdateTicketType(ctx context.Context, arg db.UpdateTicketTypeParams) (db.TicketType, error)
	DeleteTicketType(ctx context.Context, arg db.DeleteTicketTypeParams) (int64, error)
	ListPublicTicketTypes(ctx context.Context, eventID pgtype.UUID) ([]db.TicketType, error)
}

var ErrNotFound = errors.New("tickets: not found")

type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "tickets: validation failed" }

// ConflictError is a request that is well-formed but not allowed in the current state.
type ConflictError struct{ Code, Message string }

func (e *ConflictError) Error() string { return "tickets: " + e.Code }

func conflict(code, message string) error { return &ConflictError{Code: code, Message: message} }

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) *Service { return &Service{store: store, now: time.Now} }

func pgErr(err error) *pgconn.PgError {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		return e
	}
	return nil
}

func isConstraint(err error, code, name string) bool {
	e := pgErr(err)
	return e != nil && e.Code == code && e.ConstraintName == name
}

func isForeignKeyViolation(err error) bool {
	e := pgErr(err)
	return e != nil && e.Code == "23503"
}

func (s *Service) event(ctx context.Context, id pgtype.UUID) (db.Event, error) {
	ev, err := s.store.GetEvent(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Event{}, ErrNotFound
	}
	return ev, err
}

// editableEvent refuses cancelled and completed events.
func (s *Service) editableEvent(ctx context.Context, id pgtype.UUID) (db.Event, error) {
	ev, err := s.event(ctx, id)
	if err != nil {
		return db.Event{}, err
	}
	if ev.Status != statusDraft && ev.Status != statusPublished {
		return db.Event{}, conflict("event_not_editable", "cancelled or completed events cannot be changed")
	}
	return ev, nil
}

func (s *Service) List(ctx context.Context, eventID pgtype.UUID) ([]db.TicketType, error) {
	rows, err := s.store.ListTicketTypes(ctx, eventID)
	if rows == nil {
		rows = []db.TicketType{}
	}
	return rows, err
}

func (s *Service) Get(ctx context.Context, eventID, id pgtype.UUID) (db.TicketType, error) {
	tt, err := s.store.GetTicketType(ctx, db.GetTicketTypeParams{ID: id, EventID: eventID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.TicketType{}, ErrNotFound
	}
	return tt, err
}

func (s *Service) Create(ctx context.Context, eventID pgtype.UUID, in TicketTypeInput) (db.TicketType, error) {
	in.Normalize()
	ev, err := s.editableEvent(ctx, eventID)
	if err != nil {
		return db.TicketType{}, err
	}
	if fields := in.Validate(ev.EndsAt.Time, s.now()); len(fields) > 0 {
		return db.TicketType{}, &ValidationError{Fields: fields}
	}

	tt, err := s.store.CreateTicketType(ctx, db.CreateTicketTypeParams{
		EventID:       eventID,
		Name:          in.Name,
		PriceKobo:     *in.PriceKobo,
		Currency:      in.Currency,
		TotalQuantity: int32(in.TotalQuantity),
		MaxPerOrder:   int32(in.MaxPerOrder),
		SalesStart:    toTS(in.SalesStart),
		SalesEnd:      toTS(in.SalesEnd),
	})
	if isConstraint(err, "23505", "ticket_types_event_name_uniq") {
		return db.TicketType{}, errNameTaken()
	}
	return tt, err
}

func (s *Service) Update(ctx context.Context, eventID, id pgtype.UUID, p TicketTypePatch) (db.TicketType, error) {
	p.Normalize()
	if p.IsEmpty() {
		return db.TicketType{}, &ValidationError{Fields: map[string]string{"body": "provide at least one field to change"}}
	}

	ev, err := s.editableEvent(ctx, eventID)
	if err != nil {
		return db.TicketType{}, err
	}
	current, err := s.Get(ctx, eventID, id)
	if err != nil {
		return db.TicketType{}, err
	}
	// The read gives friendly field errors. The UPDATE re-checks the price lock
	// and the CHECK constraints guard the stock arithmetic, so a sale that lands
	// between this read and the write cannot slip through.
	if fields := ValidatePatch(current, p, ev.EndsAt.Time); len(fields) > 0 {
		return db.TicketType{}, &ValidationError{Fields: fields}
	}

	updated, err := s.store.UpdateTicketType(ctx, db.UpdateTicketTypeParams{
		ID:            id,
		EventID:       eventID,
		Name:          optText(p.Name),
		PriceKobo:     optInt8(p.PriceKobo),
		TotalQuantity: optInt4(p.TotalQuantity),
		MaxPerOrder:   optInt4(p.MaxPerOrder),
		SalesStart:    optTime(p.SalesStart),
		SalesEnd:      optTime(p.SalesEnd),
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows): // a ticket sold between our read and our write locked the price
		return db.TicketType{}, conflict("ticket_type_changed", "the ticket type changed while your request was running, reload it and try again")
	case isConstraint(err, "23514", "ticket_types_available_nonneg"): // the total fell below what sold in the meantime
		return db.TicketType{}, &ValidationError{Fields: map[string]string{"total_quantity": "cannot be lower than the number already sold or reserved"}}
	case isConstraint(err, "23505", "ticket_types_event_name_uniq"):
		return db.TicketType{}, errNameTaken()
	case err != nil:
		return db.TicketType{}, err
	}
	return updated, nil
}

// Delete removes a never-sold ticket type from a DRAFT event. On a live event a
// type is retired by editing sales_end or total_quantity instead, which keeps the
// "a published event always has at least one ticket type" promise race-free.
func (s *Service) Delete(ctx context.Context, eventID, id pgtype.UUID) error {
	ev, err := s.event(ctx, eventID)
	if err != nil {
		return err
	}
	switch ev.Status {
	case statusDraft:
	case statusPublished:
		return conflict("event_published", "ticket types cannot be deleted once the event is published; end sales by editing sales_end or total_quantity instead")
	default:
		return conflict("event_not_editable", "cancelled or completed events cannot be changed")
	}

	current, err := s.Get(ctx, eventID, id)
	if err != nil {
		return err
	}
	if current.TotalQuantity != current.AvailableQuantity {
		return conflict("ticket_type_has_sales", "tickets have been sold or reserved, so this ticket type cannot be deleted")
	}

	n, err := s.store.DeleteTicketType(ctx, db.DeleteTicketTypeParams{ID: id, EventID: eventID})
	switch {
	case isForeignKeyViolation(err):
		return conflict("ticket_type_in_use", "orders refer to this ticket type, so it cannot be deleted")
	case err != nil:
		return err
	case n == 0: // the event was published, or a ticket sold, after our read
		return conflict("ticket_type_changed", "the ticket type changed while your request was running, reload it and try again")
	}
	return nil
}

// ListPublic returns what a buyer may choose from. An event that was never public
// is a 404; one that is cancelled or over simply has nothing on sale.
func (s *Service) ListPublic(ctx context.Context, eventID pgtype.UUID) ([]db.TicketType, error) {
	ev, err := s.store.GetPublicEvent(ctx, eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ev.Status != statusPublished {
		return []db.TicketType{}, nil
	}
	rows, err := s.store.ListPublicTicketTypes(ctx, eventID)
	if rows == nil {
		rows = []db.TicketType{}
	}
	return rows, err
}

func errNameTaken() error {
	return conflict("name_taken", "a ticket type with this name already exists for this event")
}

func toTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

func optText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func optInt8(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

func optInt4(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}

func optTime(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return toTS(*t)
}