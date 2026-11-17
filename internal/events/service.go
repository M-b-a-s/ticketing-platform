package events

import (
	"context"
	"errors"
	"time"

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