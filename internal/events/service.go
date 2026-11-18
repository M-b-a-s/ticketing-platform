package events

import (
	"context"
	"errors"
	"time"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/pagination"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
)

// Store is the slice of the generated queries the service needs. It grows with each step.
type Store interface {
	auth.EventAccessStore // GetEventAccess, used by the route guards
	CreateEvent(ctx context.Context, arg db.CreateEventParams) (db.Event, error)
	ListManagedEvents(ctx context.Context, arg db.ListManagedEventsParams) ([]db.ListManagedEventsRow, error)
	GetEvent(ctx context.Context, id pgtype.UUID) (db.Event, error)
	UpdateEvent(ctx context.Context, arg db.UpdateEventParams) (db.Event, error)
	CountTicketTypes(ctx context.Context, eventID pgtype.UUID) (int64, error)
	PublishEvent(ctx context.Context, id pgtype.UUID) (db.Event, error)
	CancelEvent(ctx context.Context, id pgtype.UUID) (db.Event, error)
}

type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string { return "events: validation failed" }

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// Create makes a draft event owned by organizerID. The owner comes from the
// authenticated caller, never from the request body.
func (s *Service) Create(ctx context.Context, organizerID pgtype.UUID, in EventInput) (db.Event, error) {
	in.Normalize()
	if fields := in.Validate(s.now()); len(fields) > 0 {
		return db.Event{}, &ValidationError{Fields: fields}
	}

	return s.store.CreateEvent(ctx, db.CreateEventParams{
		OrganizerID: organizerID,
		Title:       in.Title,
		Description: pgtype.Text{String: in.Description, Valid: in.Description != ""},
		VenueName:   in.VenueName,
		StartsAt:    pgtype.Timestamptz{Time: in.StartsAt.UTC(), Valid: true},
		EndsAt:      pgtype.Timestamptz{Time: in.EndsAt.UTC(), Valid: true},
	})
}

// -- ListManagedEvents returns a page of events owned by organizerID, sorted by starts_at DESC, id DESC. --
type ManagedEvent struct {
	Event  db.Event
	MyRole string // "owner", "event_admin" or "gate_attendant"
}

type ManagedPage struct {
	Items      []ManagedEvent
	NextCursor *string // nil on the last page
}

// ListManaged returns one page of the events the caller owns or staffs.
func (s *Service) ListManaged(ctx context.Context, user pgtype.UUID, limit int, after *pagination.Cursor) (ManagedPage, error) {
	// Ask for one extra row: if it comes back, another page exists.
	params := db.ListManagedEventsParams{UserID: user, PageSize: int32(limit + 1)}
	if after != nil {
		id, err := uuidx.Parse(after.ID)
		if err != nil {
			return ManagedPage{}, pagination.ErrInvalidCursor
		}
		params.AfterStartsAt = pgtype.Timestamptz{Time: after.Time, Valid: true}
		params.AfterID = id
	}

	rows, err := s.store.ListManagedEvents(ctx, params)
	if err != nil {
		return ManagedPage{}, err
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	page := ManagedPage{Items: make([]ManagedEvent, 0, len(rows))}
	for _, r := range rows {
		role := r.StaffRole
		if r.IsOwner {
			role = "owner"
		}
		page.Items = append(page.Items, ManagedEvent{Event: r.Event, MyRole: role})
	}
	if hasMore {
		last := rows[len(rows)-1].Event
		c := pagination.Encode(pagination.Cursor{Time: last.StartsAt.Time, ID: uuidx.String(last.ID)})
		page.NextCursor = &c
	}
	return page, nil
}

var (
	ErrNotFound    = errors.New("events: event not found")
	ErrNotEditable = errors.New("events: event can no longer be edited")
)

func (s *Service) Get(ctx context.Context, id pgtype.UUID) (db.Event, error) {
	ev, err := s.store.GetEvent(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Event{}, ErrNotFound
	}
	return ev, err
}

// Update applies a partial change to a draft or published event.
func (s *Service) Update(ctx context.Context, id pgtype.UUID, p EventPatch) (db.Event, error) {
	p.Normalize()
	if p.IsEmpty() {
		return db.Event{}, &ValidationError{Fields: map[string]string{"body": "provide at least one field to change"}}
	}

	// The read below gives friendly validation errors. The real guarantees are
	// the status check inside UpdateEvent's WHERE clause and the CHECK constraints.
	current, err := s.Get(ctx, id)
	if err != nil {
		return db.Event{}, err
	}
	if !Editable(current.Status) {
		return db.Event{}, ErrNotEditable
	}
	if fields := ValidatePatch(current, p, s.now()); len(fields) > 0 {
		return db.Event{}, &ValidationError{Fields: fields}
	}

	updated, err := s.store.UpdateEvent(ctx, db.UpdateEventParams{
		ID:          id,
		Title:       optText(p.Title),
		Description: optText(p.Description),
		VenueName:   optText(p.VenueName),
		StartsAt:    optTime(p.StartsAt),
		EndsAt:      optTime(p.EndsAt),
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows): // the status changed between our read and our write
		return db.Event{}, ErrNotEditable
	case isTimeOrderViolation(err): // two edits that were each valid alone clashed
		return db.Event{}, &ValidationError{Fields: map[string]string{"ends_at": "must be after starts_at"}}
	case err != nil:
		return db.Event{}, err
	}
	return updated, nil
}

func isTimeOrderViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "events_time_order"
}

func optText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func optTime(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// ErrConflict means the event changed under us in a way that matters; the
// client should re-read it and decide again.
var ErrConflict = errors.New("events: event changed while the request was running")

// TransitionError is a status change the state machine does not allow.
type TransitionError struct{ From, To string }

func (e *TransitionError) Error() string {
	return fmt.Sprintf("events: cannot change status from %s to %s", e.From, e.To)
}

// Publish moves a draft to published. Repeating it on a published event succeeds.
func (s *Service) Publish(ctx context.Context, id pgtype.UUID) (db.Event, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return db.Event{}, err
	}
	if current.Status == StatusPublished {
		return current, nil // idempotent
	}
	if !CanTransition(current.Status, StatusPublished) {
		return db.Event{}, &TransitionError{From: current.Status, To: StatusPublished}
	}

	// Friendly, field-level errors. The UPDATE below re-checks all of this
	// atomically, so a change between this read and that write cannot slip through.
	count, err := s.store.CountTicketTypes(ctx, id)
	if err != nil {
		return db.Event{}, err
	}
	if fields := ValidateForPublish(current, s.now(), int(count)); len(fields) > 0 {
		return db.Event{}, &ValidationError{Fields: fields}
	}

	updated, err := s.store.PublishEvent(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.afterLostRace(ctx, id, StatusPublished)
	}
	return updated, err
}

// Cancel moves a draft or published event to cancelled. Repeating it succeeds.
func (s *Service) Cancel(ctx context.Context, id pgtype.UUID) (db.Event, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return db.Event{}, err
	}
	if current.Status == StatusCancelled {
		return current, nil // idempotent
	}
	if !CanTransition(current.Status, StatusCancelled) {
		return db.Event{}, &TransitionError{From: current.Status, To: StatusCancelled}
	}

	updated, err := s.store.CancelEvent(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.afterLostRace(ctx, id, StatusCancelled)
	}
	return updated, err
}

// afterLostRace runs when the atomic UPDATE matched nothing: another request got
// there first, or a precondition changed. If the event is already where this
// request wanted it, that is success. Anything else is a conflict.
func (s *Service) afterLostRace(ctx context.Context, id pgtype.UUID, want string) (db.Event, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return db.Event{}, err
	}
	if current.Status == want {
		return current, nil
	}
	return db.Event{}, ErrConflict
}