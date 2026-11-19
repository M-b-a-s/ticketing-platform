package events

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

func newPublicRouter(store Store) http.Handler {
	h := NewHandler(newTestService(store), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) { h.PublicRoutes(r) })
	return r
}

func publicGet(r http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil)) // no Authorization header, on purpose
	return rec
}

func publishedEvent(b byte, hoursAhead int) db.Event {
	return db.Event{
		ID:          pgtype.UUID{Bytes: [16]byte{b}, Valid: true},
		OrganizerID: pgtype.UUID{Bytes: [16]byte{9}, Valid: true},
		Title:       fmt.Sprintf("event %d", b),
		VenueName:   "Venue",
		StartsAt:    pgtype.Timestamptz{Time: fixedNow.Add(time.Duration(hoursAhead) * time.Hour), Valid: true},
		EndsAt:      pgtype.Timestamptz{Time: fixedNow.Add(time.Duration(hoursAhead+2) * time.Hour), Valid: true},
		Status:      StatusPublished,
	}
}

func TestPublicListShapeAndPagination(t *testing.T) {
	store := &fakeStore{publicRows: []db.Event{publishedEvent(1, 10), publishedEvent(2, 20), publishedEvent(3, 30)}}
	r := newPublicRouter(store)

	rec := publicGet(r, "/v1/events?limit=2")

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 without any token, got %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != publicCacheControl {
		t.Fatalf("missing cache header, got %q", rec.Header().Get("Cache-Control"))
	}
	if strings.Contains(rec.Body.String(), "organizer") {
		t.Fatalf("the public shape must not expose the organizer: %s", rec.Body)
	}
	if store.publicListGot.PageSize != 3 || store.publicListGot.AfterStartsAt.Valid {
		t.Fatalf("page one asks for limit+1 rows and no cursor, got %+v", store.publicListGot)
	}

	var page publicListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("want 2 items and a next cursor, got %d, %v", len(page.Items), page.NextCursor)
	}

	publicGet(r, "/v1/events?limit=2&cursor="+*page.NextCursor)
	last := store.publicRows[1]
	if store.publicListGot.AfterID != last.ID || !store.publicListGot.AfterStartsAt.Time.Equal(last.StartsAt.Time) {
		t.Fatalf("the cursor must carry the last returned item's position, got %+v", store.publicListGot)
	}
}

func TestPublicListEmptyIsAnEmptyArray(t *testing.T) {
	rec := publicGet(newPublicRouter(&fakeStore{}), "/v1/events")

	if !strings.Contains(rec.Body.String(), `"items":[]`) || !strings.Contains(rec.Body.String(), `"next_cursor":null`) {
		t.Fatalf("want an empty array and a null cursor, got %s", rec.Body)
	}
}

func TestPublicListRejectsBadQuery(t *testing.T) {
	r := newPublicRouter(&fakeStore{})
	for _, q := range []string{"?limit=0", "?limit=abc", "?cursor=garbage"} {
		if rec := publicGet(r, "/v1/events"+q); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", q, rec.Code)
		}
	}
}

func TestPublicDetail(t *testing.T) {
	t.Run("a published event", func(t *testing.T) {
		rec := publicGet(newPublicRouter(&fakeStore{publicEvent: publishedEvent(5, 10)}),
			"/v1/events/00000000-0000-0000-0000-000000000005")

		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"title":"event 5"`) ||
			strings.Contains(rec.Body.String(), "organizer") || rec.Header().Get("Cache-Control") != publicCacheControl {
			t.Fatalf("unexpected response: %d %v %s", rec.Code, rec.Header(), rec.Body)
		}
	})

	t.Run("a cancelled event still shows, as cancelled", func(t *testing.T) {
		ev := publishedEvent(5, 10)
		ev.Status = StatusCancelled

		rec := publicGet(newPublicRouter(&fakeStore{publicEvent: ev}), "/v1/events/00000000-0000-0000-0000-000000000005")

		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"cancelled"`) {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
	})

	t.Run("an event that was never public is a 404 that is not cached", func(t *testing.T) {
		rec := publicGet(newPublicRouter(&fakeStore{publicGetErr: pgx.ErrNoRows}), "/v1/events/00000000-0000-0000-0000-000000000005")

		if rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "" {
			t.Fatalf("got %d with cache header %q", rec.Code, rec.Header().Get("Cache-Control"))
		}
	})

	t.Run("a malformed id is a 404", func(t *testing.T) {
		if rec := publicGet(newPublicRouter(&fakeStore{}), "/v1/events/not-a-uuid"); rec.Code != http.StatusNotFound {
			t.Fatalf("got %d", rec.Code)
		}
	})
}