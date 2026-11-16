//go:build integration

package integration

import (
	"bytes"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/tests/integration/testdb"
)

func insertEventAt(t *testing.T, pool *pgxpool.Pool, organizer pgtype.UUID, title string, startsAt time.Time) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO events (organizer_id, title, venue_name, starts_at, ends_at)
		 VALUES ($1, $2, 'Venue', $3, $4) RETURNING id`,
		organizer, title, startsAt, startsAt.Add(2*time.Hour)).Scan(&id)
	if err != nil {
		t.Fatalf("insert event %q: %v", title, err)
	}
	return id
}

func TestListManagedEventsKeysetPagination(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	me := createUser(t, q, "me@example.com")
	other := createUser(t, q, "other@example.com")
	base := time.Now().Add(48 * time.Hour).Truncate(time.Second)

	mine := []pgtype.UUID{
		insertEventAt(t, pool, me, "m1", base.Add(1*time.Hour)),
		insertEventAt(t, pool, me, "m2", base.Add(2*time.Hour)), // m2, m3, m4 share a start time,
		insertEventAt(t, pool, me, "m3", base.Add(2*time.Hour)), // so only the id tie-break
		insertEventAt(t, pool, me, "m4", base.Add(2*time.Hour)), // keeps paging stable
		insertEventAt(t, pool, me, "m5", base.Add(3*time.Hour)),
	}
	staffed := insertEventAt(t, pool, other, "staffed", base.Add(4*time.Hour))
	mustAddStaff(t, pool, staffed, me, "gate_attendant")
	unrelated := insertEventAt(t, pool, other, "unrelated", base.Add(5*time.Hour))

	list := func(pageSize int, after *db.Event) []db.ListManagedEventsRow {
		p := db.ListManagedEventsParams{UserID: me, PageSize: int32(pageSize)}
		if after != nil {
			p.AfterStartsAt, p.AfterID = after.StartsAt, after.ID
		}
		rows, err := q.ListManagedEvents(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	// reference: everything in one page
	all := list(100, nil)

	if len(all) != 6 {
		t.Fatalf("want my 5 events plus the 1 I staff, got %d", len(all))
	}
	seen := map[pgtype.UUID]db.ListManagedEventsRow{}
	for _, r := range all {
		seen[r.Event.ID] = r
	}
	if _, ok := seen[unrelated]; ok {
		t.Fatal("an unrelated event must never appear")
	}
	for _, id := range mine {
		if r, ok := seen[id]; !ok || !r.IsOwner {
			t.Fatalf("my own event missing or not flagged as owned: %v", id)
		}
	}
	if r := seen[staffed]; r.IsOwner || r.StaffRole != "gate_attendant" {
		t.Fatalf("staffed event should show my staff role, got owner=%v role=%q", r.IsOwner, r.StaffRole)
	}

	// newest start first, ties broken by id descending
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1].Event, all[i].Event
		if cur.StartsAt.Time.After(prev.StartsAt.Time) ||
			(cur.StartsAt.Time.Equal(prev.StartsAt.Time) && bytes.Compare(prev.ID.Bytes[:], cur.ID.Bytes[:]) <= 0) {
			t.Fatalf("rows %d and %d are out of order", i-1, i)
		}
	}

	// walk it two at a time, exactly as the service does (page size 2, query limit 3)
	var paged []pgtype.UUID
	var after *db.Event
	for page := 0; page < 10; page++ {
		rows := list(3, after)
		hasMore := len(rows) > 2
		if hasMore {
			rows = rows[:2]
		}
		for _, r := range rows {
			paged = append(paged, r.Event.ID)
		}
		if !hasMore {
			break
		}
		last := rows[len(rows)-1].Event
		after = &last
	}

	if len(paged) != len(all) {
		t.Fatalf("paging returned %d events, one-shot returned %d", len(paged), len(all))
	}
	for i := range all {
		if paged[i] != all[i].Event.ID {
			t.Fatalf("paged and one-shot order differ at position %d", i)
		}
	}
}