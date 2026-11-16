package events

import (
	"strings"
	"testing"
	"time"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/jackc/pgx/v5/pgtype"
)

var fixedNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func validInput() EventInput {
	return EventInput{
		Title:     "Test Concert",
		VenueName: "Test Venue",
		StartsAt:  fixedNow.Add(48 * time.Hour),
		EndsAt:    fixedNow.Add(52 * time.Hour),
	}
}

func TestValidateAcceptsGoodInput(t *testing.T) {
	if f := validInput().Validate(fixedNow); len(f) != 0 {
		t.Fatalf("expected no errors, got %v", f)
	}

	in := validInput()
	in.Title = strings.Repeat("é", MaxTitleLen) // 120 characters, 240 bytes: length counts characters
	if f := in.Validate(fixedNow); len(f) != 0 {
		t.Fatalf("multibyte title within the limit must pass, got %v", f)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		field  string
		mutate func(*EventInput)
	}{
		{"empty title", "title", func(in *EventInput) { in.Title = "" }},
		{"long title", "title", func(in *EventInput) { in.Title = strings.Repeat("a", MaxTitleLen+1) }},
		{"empty venue", "venue_name", func(in *EventInput) { in.VenueName = "" }},
		{"long description", "description", func(in *EventInput) { in.Description = strings.Repeat("a", MaxDescriptionLen+1) }},
		{"zero start", "starts_at", func(in *EventInput) { in.StartsAt = time.Time{} }},
		{"start in the past", "starts_at", func(in *EventInput) { in.StartsAt = fixedNow.Add(-time.Hour) }},
		{"start too far ahead", "starts_at", func(in *EventInput) { in.StartsAt = fixedNow.Add(MaxLeadTime + time.Hour) }},
		{"zero end", "ends_at", func(in *EventInput) { in.EndsAt = time.Time{} }},
		{"end before start", "ends_at", func(in *EventInput) { in.EndsAt = in.StartsAt.Add(-time.Hour) }},
		{"end equals start", "ends_at", func(in *EventInput) { in.EndsAt = in.StartsAt }},
		{"event too long", "ends_at", func(in *EventInput) { in.EndsAt = in.StartsAt.Add(MaxEventSpan + time.Hour) }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validInput()
			c.mutate(&in)
			if f := in.Validate(fixedNow); f[c.field] == "" {
				t.Fatalf("expected an error on %q, got %v", c.field, f)
			}
		})
	}
}

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{StatusDraft, StatusPublished, true},
		{StatusDraft, StatusCancelled, true},
		{StatusDraft, StatusCompleted, false},
		{StatusPublished, StatusCancelled, true},
		{StatusPublished, StatusCompleted, true},
		{StatusPublished, StatusDraft, false},
		{StatusCancelled, StatusPublished, false},
		{StatusCompleted, StatusCancelled, false},
		{"nonsense", StatusPublished, false},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.want {
			t.Errorf("%s -> %s: want %v, got %v", c.from, c.to, c.want, got)
		}
	}
}

func TestValidateForPublish(t *testing.T) {
	upcoming := db.Event{StartsAt: pgtype.Timestamptz{Time: fixedNow.Add(time.Hour), Valid: true}}
	past := db.Event{StartsAt: pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}}

	if f := ValidateForPublish(upcoming, fixedNow, 1); len(f) != 0 {
		t.Fatalf("a ready event should pass, got %v", f)
	}
	if f := ValidateForPublish(past, fixedNow, 1); f["starts_at"] == "" {
		t.Fatalf("a past event must not publish, got %v", f)
	}
	if f := ValidateForPublish(upcoming, fixedNow, 0); f["ticket_types"] == "" {
		t.Fatalf("an event without ticket types must not publish, got %v", f)
	}
}