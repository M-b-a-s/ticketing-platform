package events

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/pagination"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
)

// Store is the slice of the generated queries the service needs. It grows with each step.
type Store interface {
	CreateEvent(ctx context.Context, arg db.CreateEventParams) (db.Event, error)
	ListManagedEvents(ctx context.Context, arg db.ListManagedEventsParams) ([]db.ListManagedEventsRow, error)
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