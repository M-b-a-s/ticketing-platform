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

// EventPatch is a partial update: nil means "leave this field unchanged".
type EventPatch struct {
	Title       *string
	Description *string // pointing at "" clears the description
	VenueName   *string
	StartsAt    *time.Time
	EndsAt      *time.Time
}

func (p EventPatch) IsEmpty() bool {
	return p.Title == nil && p.Description == nil && p.VenueName == nil &&
		p.StartsAt == nil && p.EndsAt == nil
}

func (p *EventPatch) Normalize() {
	trim := func(s *string) *string {
		if s == nil {
			return nil
		}
		v := strings.TrimSpace(*s)
		return &v
	}
	p.Title, p.Description, p.VenueName = trim(p.Title), trim(p.Description), trim(p.VenueName)
}

// Editable reports whether an event in this status may still be changed.
func Editable(status string) bool {
	return status == StatusDraft || status == StatusPublished
}

// ValidatePatch checks only what the patch touches, against the event as it
// stands now. "Starts in the future" applies only when starts_at itself is
// being changed, so fixing a typo in the title of an event that has already
// begun is still allowed.
func ValidatePatch(current db.Event, p EventPatch, now time.Time) map[string]string {
	f := map[string]string{}

	if p.Title != nil {
		if n := utf8.RuneCountInString(*p.Title); n == 0 || n > MaxTitleLen {
			f["title"] = "is required and must be at most 120 characters"
		}
	}
	if p.VenueName != nil {
		if n := utf8.RuneCountInString(*p.VenueName); n == 0 || n > MaxVenueLen {
			f["venue_name"] = "is required and must be at most 120 characters"
		}
	}
	if p.Description != nil && utf8.RuneCountInString(*p.Description) > MaxDescriptionLen {
		f["description"] = "must be at most 5000 characters"
	}

	start, end := current.StartsAt.Time, current.EndsAt.Time
	if p.StartsAt != nil {
		start = *p.StartsAt
		switch {
		case !start.After(now):
			f["starts_at"] = "must be in the future"
		case start.After(now.Add(MaxLeadTime)):
			f["starts_at"] = "is too far in the future"
		}
	}
	if p.EndsAt != nil {
		end = *p.EndsAt
	}

	if p.StartsAt != nil || p.EndsAt != nil {
		field, order := "ends_at", "must be after starts_at"
		if p.EndsAt == nil { // only the start moved, so the start is what clashes
			field, order = "starts_at", "must be before the event's end time"
		}
		if _, taken := f[field]; !taken {
			switch {
			case !end.After(start):
				f[field] = order
			case end.Sub(start) > MaxEventSpan:
				f[field] = "event cannot last longer than 30 days"
			}
		}
	}
	return f
}