//go:build integration

package integration

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/tests/integration/testdb"
)

func insertEventRaw(t *testing.T, pool *pgxpool.Pool, organizer pgtype.UUID, title, status string, publishedAt *time.Time, starts, ends time.Time) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO events (organizer_id, title, venue_name, starts_at, ends_at, status, published_at)
		 VALUES ($1, $2, 'Venue', $3, $4, $5, $6) RETURNING id`,
		organizer, title, starts, ends, status, publishedAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert event %q: %v", title, err)
	}
	return id
}

func TestPublicEventVisibilityAndListing(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")

	now := time.Now()
	soon := now.Add(48 * time.Hour).Truncate(time.Second)
	pub := &now
	mk := func(title, status string, published *time.Time, starts time.Time) pgtype.UUID {
		return insertEventRaw(t, pool, owner, title, status, published, starts, starts.Add(2*time.Hour))
	}

	draft := mk("draft", "draft", nil, soon)
	a := mk("a", "published", pub, soon.Add(1*time.Hour))
	b := mk("b", "published", pub, soon.Add(2*time.Hour)) // b and c share a start time:
	c := mk("c", "published", pub, soon.Add(2*time.Hour)) // only the id tie-break keeps paging stable
	ended := mk("ended", "published", pub, now.Add(-5*time.Hour))
	cancelledAfterPublish := mk("cancelled-public", "cancelled", pub, soon.Add(4*time.Hour))
	cancelledDraft := mk("cancelled-draft", "cancelled", nil, soon.Add(5*time.Hour))
	completed := mk("completed", "completed", pub, now.Add(-6*time.Hour))

	list := func(pageSize int, after *db.Event) []db.Event {
		p := db.ListPublicEventsParams{PageSize: int32(pageSize)}
		if after != nil {
			p.AfterStartsAt, p.AfterID = after.StartsAt, after.ID
		}
		rows, err := q.ListPublicEvents(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	t.Run("the list holds only upcoming published events", func(t *testing.T) {
		all := list(100, nil)

		got := map[pgtype.UUID]bool{}
		for _, e := range all {
			got[e.ID] = true
		}
		if len(all) != 3 || !got[a] || !got[b] || !got[c] {
			t.Fatalf("want exactly a, b and c, got %d events", len(all))
		}
		for name, id := range map[string]pgtype.UUID{
			"draft": draft, "ended": ended, "cancelled": cancelledAfterPublish,
			"cancelled draft": cancelledDraft, "completed": completed,
		} {
			if got[id] {
				t.Fatalf("%s must not be listed", name)
			}
		}
		for i := 1; i < len(all); i++ { // soonest first, ties by id ascending
			prev, cur := all[i-1], all[i]
			if cur.StartsAt.Time.Before(prev.StartsAt.Time) ||
				(cur.StartsAt.Time.Equal(prev.StartsAt.Time) && bytes.Compare(prev.ID.Bytes[:], cur.ID.Bytes[:]) >= 0) {
				t.Fatalf("rows %d and %d are out of order", i-1, i)
			}
		}
	})

	t.Run("paging two at a time matches the one-shot list", func(t *testing.T) {
		all := list(100, nil)

		var paged []pgtype.UUID
		var after *db.Event
		for page := 0; page < 10; page++ {
			rows := list(3, after) // page size 2, plus the extra row the service asks for
			hasMore := len(rows) > 2
			if hasMore {
				rows = rows[:2]
			}
			for _, e := range rows {
				paged = append(paged, e.ID)
			}
			if !hasMore {
				break
			}
			last := rows[len(rows)-1]
			after = &last
		}

		if len(paged) != len(all) {
			t.Fatalf("paging returned %d, one-shot returned %d", len(paged), len(all))
		}
		for i := range all {
			if paged[i] != all[i].ID {
				t.Fatalf("order differs at position %d", i)
			}
		}
	})

	t.Run("detail", func(t *testing.T) {
		cases := []struct {
			name   string
			id     pgtype.UUID
			status string // "" means it must not be visible
		}{
			{"draft", draft, ""},
			{"published", a, "published"},
			{"published and already over", ended, "published"},
			{"cancelled after being public", cancelledAfterPublish, "cancelled"},
			{"cancelled before ever being public", cancelledDraft, ""},
			{"completed", completed, "completed"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				ev, err := q.GetPublicEvent(ctx, c.id)
				if c.status == "" {
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("must not be visible, got %v", err)
					}
					return
				}
				if err != nil || ev.Status != c.status {
					t.Fatalf("want status %q, got %q (%v)", c.status, ev.Status, err)
				}
			})
		}
	})
}

// The same rules, driven through the real publish and cancel queries.
func TestPublishingMakesAnEventPublic(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")

	t.Run("draft, then published, then cancelled", func(t *testing.T) {
		ev, err := q.CreateEvent(ctx, newEventParams(owner))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.GetPublicEvent(ctx, ev.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a draft must be invisible, got %v", err)
		}

		insertTicketType(t, pool, ev.ID, "General")
		published, err := q.PublishEvent(ctx, ev.ID)
		if err != nil || !published.PublishedAt.Valid {
			t.Fatalf("publishing must stamp published_at: %+v %v", published, err)
		}
		if _, err := q.GetPublicEvent(ctx, ev.ID); err != nil {
			t.Fatalf("a published event must be visible, got %v", err)
		}

		if _, err := q.CancelEvent(ctx, ev.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := q.GetPublicEvent(ctx, ev.ID); err != nil || got.Status != "cancelled" {
			t.Fatalf("a cancelled event that was public must still show as cancelled, got %q %v", got.Status, err)
		}
	})

	t.Run("a draft cancelled before it was ever public stays hidden", func(t *testing.T) {
		ev, err := q.CreateEvent(ctx, newEventParams(owner))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.CancelEvent(ctx, ev.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.GetPublicEvent(ctx, ev.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("must stay hidden, got %v", err)
		}
	})
}