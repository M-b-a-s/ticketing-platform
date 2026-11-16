package events

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

// Store is the slice of the generated queries the service needs. It grows with each step.
type Store interface {
	CreateEvent(ctx context.Context, arg db.CreateEventParams) (db.Event, error)
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