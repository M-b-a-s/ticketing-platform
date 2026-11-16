package events

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

// Statuses must match the events_status_valid CHECK constraint.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusCancelled = "cancelled"
	StatusCompleted = "completed"
)

const (
	MaxTitleLen       = 120
	MaxVenueLen       = 120
	MaxDescriptionLen = 5000
	MaxEventSpan      = 30 * 24 * time.Hour
	MaxLeadTime       = 3 * 365 * 24 * time.Hour
)

// transitions is the whole status machine. Anything not listed is forbidden.
var transitions = map[string][]string{
	StatusDraft:     {StatusPublished, StatusCancelled},
	StatusPublished: {StatusCancelled, StatusCompleted},
}

func CanTransition(from, to string) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

type EventInput struct {
	Title       string
	Description string
	VenueName   string
	StartsAt    time.Time
	EndsAt      time.Time
}

func (in *EventInput) Normalize() {
	in.Title = strings.TrimSpace(in.Title)
	in.Description = strings.TrimSpace(in.Description)
	in.VenueName = strings.TrimSpace(in.VenueName)
}

// Validate returns field -> message, empty when the input is acceptable.
func (in EventInput) Validate(now time.Time) map[string]string {
	f := map[string]string{}

	if n := utf8.RuneCountInString(in.Title); n == 0 || n > MaxTitleLen {
		f["title"] = "is required and must be at most 120 characters"
	}
	if n := utf8.RuneCountInString(in.VenueName); n == 0 || n > MaxVenueLen {
		f["venue_name"] = "is required and must be at most 120 characters"
	}
	if utf8.RuneCountInString(in.Description) > MaxDescriptionLen {
		f["description"] = "must be at most 5000 characters"
	}

	switch {
	case in.StartsAt.IsZero():
		f["starts_at"] = "is required"
	case !in.StartsAt.After(now):
		f["starts_at"] = "must be in the future"
	case in.StartsAt.After(now.Add(MaxLeadTime)):
		f["starts_at"] = "is too far in the future"
	}

	switch {
	case in.EndsAt.IsZero():
		f["ends_at"] = "is required"
	case !in.EndsAt.After(in.StartsAt):
		f["ends_at"] = "must be after starts_at"
	case !in.StartsAt.IsZero() && in.EndsAt.Sub(in.StartsAt) > MaxEventSpan:
		f["ends_at"] = "event cannot last longer than 30 days"
	}
	return f
}

// ValidateForPublish checks the rules that apply at the moment of publishing.
func ValidateForPublish(e db.Event, now time.Time, ticketTypeCount int) map[string]string {
	f := map[string]string{}
	if !e.StartsAt.Time.After(now) {
		f["starts_at"] = "must be in the future to publish"
	}
	if ticketTypeCount < 1 {
		f["ticket_types"] = "at least one ticket type is required to publish"
	}
	return f
}