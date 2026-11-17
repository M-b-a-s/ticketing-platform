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

func ptr[T any](v T) *T { return &v }

func currentEvent() db.Event {
	return db.Event{
		Status:   StatusDraft,
		StartsAt: pgtype.Timestamptz{Time: fixedNow.Add(48 * time.Hour), Valid: true},
		EndsAt:   pgtype.Timestamptz{Time: fixedNow.Add(52 * time.Hour), Valid: true},
	}
}

func TestValidatePatch(t *testing.T) {
	start, end := fixedNow.Add(48*time.Hour), fixedNow.Add(52*time.Hour)

	cases := []struct {
		name    string
		patch   EventPatch
		current func(*db.Event)
		field   string // "" means the patch must be valid
	}{
		{"title only", EventPatch{Title: ptr("New title")}, nil, ""},
		{"empty title", EventPatch{Title: ptr("")}, nil, "title"},
		{"long venue", EventPatch{VenueName: ptr(strings.Repeat("a", MaxVenueLen+1))}, nil, "venue_name"},
		{"clearing the description is allowed", EventPatch{Description: ptr("")}, nil, ""},
		{"long description", EventPatch{Description: ptr(strings.Repeat("a", MaxDescriptionLen+1))}, nil, "description"},
		{"start in the past", EventPatch{StartsAt: ptr(fixedNow.Add(-time.Hour))}, nil, "starts_at"},
		{"start after the current end", EventPatch{StartsAt: ptr(end.Add(time.Hour))}, nil, "starts_at"},
		{"end before the current start", EventPatch{EndsAt: ptr(start.Add(-time.Hour))}, nil, "ends_at"},
		{"extend the end within the span", EventPatch{EndsAt: ptr(end.Add(2 * time.Hour))}, nil, ""},
		{"span too long", EventPatch{EndsAt: ptr(start.Add(MaxEventSpan + time.Hour))}, nil, "ends_at"},
		{"move both times", EventPatch{StartsAt: ptr(start.Add(24 * time.Hour)), EndsAt: ptr(end.Add(24 * time.Hour))}, nil, ""},
		{
			"title edit on an event that already began",
			EventPatch{Title: ptr("Typo fixed")},
			func(e *db.Event) {
				e.StartsAt = pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}
				e.EndsAt = pgtype.Timestamptz{Time: fixedNow.Add(time.Hour), Valid: true}
			},
			"",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cur := currentEvent()
			if c.current != nil {
				c.current(&cur)
			}
			f := ValidatePatch(cur, c.patch, fixedNow)

			if c.field == "" && len(f) != 0 {
				t.Fatalf("expected a valid patch, got %v", f)
			}
			if c.field != "" && f[c.field] == "" {
				t.Fatalf("expected an error on %q, got %v", c.field, f)
			}
		})
	}
}

func TestEditableAndEmptyPatch(t *testing.T) {
	for status, want := range map[string]bool{
		StatusDraft: true, StatusPublished: true, StatusCancelled: false, StatusCompleted: false,
	} {
		if Editable(status) != want {
			t.Errorf("Editable(%q) should be %v", status, want)
		}
	}
	if !(EventPatch{}).IsEmpty() || (EventPatch{Title: ptr("x")}).IsEmpty() {
		t.Fatal("IsEmpty is wrong")
	}
}