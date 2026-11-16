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
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
)

const callerID = "00000000-0000-0000-0000-000000000009"

func newTestRouter(t *testing.T, store Store) (http.Handler, *auth.TokenManager) {
	t.Helper()
	tm, err := auth.NewTokenManager("0123456789abcdef0123456789abcdef", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(newTestService(store), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Route("/v1/manage", func(r chi.Router) {
		r.Use(auth.Authenticate(tm))
		h.ManageRoutes(r)
	})
	return r, tm
}

func do(r http.Handler, tm *auth.TokenManager, role, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if role != "" {
		tok, _, _ := tm.Issue(callerID, callerID, role)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func createBody() string {
	return fmt.Sprintf(`{"title":"Test Concert","venue_name":"Test Venue","starts_at":%q,"ends_at":%q}`,
		fixedNow.Add(48*time.Hour).Format(time.RFC3339), fixedNow.Add(52*time.Hour).Format(time.RFC3339))
}

func TestCreateEventOwnerComesFromTheToken(t *testing.T) {
	store := &fakeStore{}
	r, tm := newTestRouter(t, store)

	rec := do(r, tm, auth.RoleOrganizer, http.MethodPost, "/v1/manage/events", createBody())

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body)
	}
	if uuidx.String(store.got.OrganizerID) != callerID {
		t.Fatalf("owner must be the authenticated caller, got %s", uuidx.String(store.got.OrganizerID))
	}
	if !strings.HasPrefix(rec.Header().Get("Location"), "/v1/manage/events/") || !strings.Contains(rec.Body.String(), `"status":"draft"`) {
		t.Fatalf("unexpected response: %v %s", rec.Header(), rec.Body)
	}
}

func TestCreateEventRoleGate(t *testing.T) {
	cases := []struct {
		role string
		want int
	}{
		{"", http.StatusUnauthorized},
		{auth.RoleCustomer, http.StatusForbidden},
		{auth.RoleOrganizer, http.StatusCreated},
		{auth.RolePlatformAdmin, http.StatusCreated},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("role=%q", c.role), func(t *testing.T) {
			store := &fakeStore{}
			r, tm := newTestRouter(t, store)

			rec := do(r, tm, c.role, http.MethodPost, "/v1/manage/events", createBody())

			if rec.Code != c.want || store.called != (c.want == http.StatusCreated) {
				t.Fatalf("want %d (store called=%v), got %d called=%v", c.want, c.want == http.StatusCreated, rec.Code, store.called)
			}
		})
	}
}

func TestCreateEventRejectsClientSuppliedOwnerAndStatus(t *testing.T) {
	for _, extra := range []string{`"organizer_id":"` + callerID + `"`, `"status":"published"`} {
		store := &fakeStore{}
		r, tm := newTestRouter(t, store)
		body := strings.Replace(createBody(), "{", "{"+extra+",", 1)

		rec := do(r, tm, auth.RoleOrganizer, http.MethodPost, "/v1/manage/events", body)

		if rec.Code != http.StatusBadRequest || store.called {
			t.Fatalf("%s: want 400 and no store call, got %d called=%v", extra, rec.Code, store.called)
		}
	}
}

func TestCreateEventValidationAndBadTimestamps(t *testing.T) {
	store := &fakeStore{}
	r, tm := newTestRouter(t, store)

	rec := do(r, tm, auth.RoleOrganizer, http.MethodPost, "/v1/manage/events",
		fmt.Sprintf(`{"title":"","venue_name":"V","starts_at":%q,"ends_at":%q}`,
			fixedNow.Add(time.Hour).Format(time.RFC3339), fixedNow.Add(2*time.Hour).Format(time.RFC3339)))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "title") {
		t.Fatalf("want 422 naming title, got %d: %s", rec.Code, rec.Body)
	}

	rec = do(r, tm, auth.RoleOrganizer, http.MethodPost, "/v1/manage/events",
		`{"title":"T","venue_name":"V","starts_at":"tomorrow","ends_at":"later"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a non-RFC3339 timestamp must be a 400, got %d", rec.Code)
	}
	if store.called {
		t.Fatal("the store must not be called for bad input")
	}
}

func TestCreateEventStoresUTC(t *testing.T) {
	store := &fakeStore{}
	r, tm := newTestRouter(t, store)
	lagos := time.FixedZone("WAT", 3600)
	start := fixedNow.Add(48 * time.Hour)
	body := fmt.Sprintf(`{"title":"T","venue_name":"V","starts_at":%q,"ends_at":%q}`,
		start.In(lagos).Format(time.RFC3339), start.Add(3*time.Hour).In(lagos).Format(time.RFC3339))

	rec := do(r, tm, auth.RoleOrganizer, http.MethodPost, "/v1/manage/events", body)

	if rec.Code != http.StatusCreated || !store.got.StartsAt.Time.Equal(start) {
		t.Fatalf("want 201 with the same instant, got %d: %s", rec.Code, rec.Body)
	}
	if _, offset := store.got.StartsAt.Time.Zone(); offset != 0 {
		t.Fatal("times must be stored in UTC")
	}
}

func TestListManagedPagination(t *testing.T) {
	mk := func(b byte, hours int) db.Event {
		return db.Event{
			ID:       pgtype.UUID{Bytes: [16]byte{b}, Valid: true},
			Title:    fmt.Sprintf("event %d", b),
			StartsAt: pgtype.Timestamptz{Time: fixedNow.Add(time.Duration(hours) * time.Hour), Valid: true},
		}
	}
	store := &fakeStore{rows: []db.ListManagedEventsRow{
		{Event: mk(3, 30), IsOwner: true},
		{Event: mk(2, 20), StaffRole: "gate_attendant"},
		{Event: mk(1, 10), IsOwner: true}, // the extra row that proves another page exists
	}}
	r, tm := newTestRouter(t, store)

	rec := do(r, tm, auth.RoleOrganizer, http.MethodGet, "/v1/manage/events?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if store.listGot.PageSize != 3 || store.listGot.AfterStartsAt.Valid {
		t.Fatalf("page one must ask for limit+1 rows and no cursor, got %+v", store.listGot)
	}

	var page managedListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("want 2 items and a next cursor, got %d items, cursor=%v", len(page.Items), page.NextCursor)
	}
	if page.Items[0].MyRole != "owner" || page.Items[1].MyRole != "gate_attendant" {
		t.Fatalf("unexpected roles: %q, %q", page.Items[0].MyRole, page.Items[1].MyRole)
	}

	// following the cursor passes the LAST VISIBLE item's position to the query
	do(r, tm, auth.RoleOrganizer, http.MethodGet, "/v1/manage/events?limit=2&cursor="+*page.NextCursor, "")
	if store.listGot.AfterID != store.rows[1].Event.ID || !store.listGot.AfterStartsAt.Time.Equal(store.rows[1].Event.StartsAt.Time) {
		t.Fatalf("the cursor must carry the last returned item's position, got %+v", store.listGot)
	}
}

func TestListManagedLastPageHasNoCursor(t *testing.T) {
	store := &fakeStore{rows: []db.ListManagedEventsRow{{Event: db.Event{Title: "only one"}, IsOwner: true}}}
	r, tm := newTestRouter(t, store)

	rec := do(r, tm, auth.RoleOrganizer, http.MethodGet, "/v1/manage/events", "")

	if !strings.Contains(rec.Body.String(), `"next_cursor":null`) {
		t.Fatalf("the last page must report a null next_cursor, got %s", rec.Body)
	}
}

func TestListManagedAccessAndQueryValidation(t *testing.T) {
	r, tm := newTestRouter(t, &fakeStore{})

	if rec := do(r, tm, "", http.MethodGet, "/v1/manage/events", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: want 401, got %d", rec.Code)
	}
	if rec := do(r, tm, auth.RoleCustomer, http.MethodGet, "/v1/manage/events", ""); rec.Code != http.StatusOK {
		t.Fatalf("a customer who staffs events must be able to list them, got %d", rec.Code)
	}
	for _, q := range []string{"?limit=0", "?limit=abc", "?cursor=garbage"} {
		if rec := do(r, tm, auth.RoleOrganizer, http.MethodGet, "/v1/manage/events"+q, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", q, rec.Code)
		}
	}
}