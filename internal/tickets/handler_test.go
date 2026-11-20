package tickets

import (
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
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
)

const callerID = "00000000-0000-0000-0000-000000000009"

func newRouter(t *testing.T, store Store) (http.Handler, *auth.TokenManager) {
	t.Helper()
	tm, err := auth.NewTokenManager("0123456789abcdef0123456789abcdef", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(newTestService(store), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		h.PublicRoutes(r)
		r.Route("/manage", func(r chi.Router) {
			r.Use(auth.Authenticate(tm))
			h.ManageRoutes(r)
		})
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

var (
	listPath = "/v1/manage/events/" + uuidx.String(eventA) + "/ticket-types"
	onePath  = listPath + "/" + uuidx.String(typeID)
)

func createJSON() string {
	return fmt.Sprintf(`{"name":"VIP","price_kobo":1500000,"total_quantity":50,"sales_start":%q,"sales_end":%q}`,
		fixedNow.Format(time.RFC3339), fixedNow.Add(7*24*time.Hour).Format(time.RFC3339))
}

func TestManageGuards(t *testing.T) {
	calls := []struct {
		name, method, path, body string
		manage                   bool
		success                  int
	}{
		{"list", http.MethodGet, listPath, "", false, 200},
		{"create", http.MethodPost, listPath, createJSON(), true, 201},
		{"get", http.MethodGet, onePath, "", false, 200},
		{"patch", http.MethodPatch, onePath, `{"name":"Renamed"}`, true, 200},
		{"delete", http.MethodDelete, onePath, "", true, 204},
	}
	subjects := []struct {
		name         string
		role         string
		access       db.GetEventAccessRow
		view, manage int // 0 means the call's own success code
	}{
		{"anonymous", "", db.GetEventAccessRow{}, 401, 401},
		{"owner", auth.RoleOrganizer, db.GetEventAccessRow{IsOwner: true}, 0, 0},
		{"event_admin", auth.RoleCustomer, db.GetEventAccessRow{StaffRole: auth.StaffEventAdmin}, 0, 0},
		{"gate attendant", auth.RoleCustomer, db.GetEventAccessRow{StaffRole: auth.StaffGateAttendant}, 0, 403},
		{"other organizer", auth.RoleOrganizer, db.GetEventAccessRow{}, 404, 404},
		{"platform_admin", auth.RolePlatformAdmin, db.GetEventAccessRow{}, 0, 0},
	}

	for _, s := range subjects {
		for _, c := range calls {
			t.Run(s.name+"/"+c.name, func(t *testing.T) {
				store := fixture()
				store.access = s.access
				r, tm := newRouter(t, store)

				want := s.view
				if c.manage {
					want = s.manage
				}
				if want == 0 {
					want = c.success
				}

				rec := do(r, tm, s.role, c.method, c.path, c.body)

				if rec.Code != want {
					t.Fatalf("want %d, got %d: %s", want, rec.Code, rec.Body)
				}
				if wrote := store.createCalled || store.updateCalled || store.deleteCalled; wrote && want != c.success {
					t.Fatal("a refused request must not write")
				}
			})
		}
	}
}

// The event in the URL is the one the guard checked, so a ticket type from a
// DIFFERENT event must be unreachable through it.
func TestTicketTypeOfAnotherEventIsUnreachable(t *testing.T) {
	for _, c := range []struct{ method, body string }{
		{http.MethodGet, ""}, {http.MethodPatch, `{"name":"x"}`}, {http.MethodDelete, ""},
	} {
		store := fixture()
		store.types[0].EventID = eventB // belongs elsewhere
		store.access = db.GetEventAccessRow{IsOwner: true}
		r, tm := newRouter(t, store)

		rec := do(r, tm, auth.RoleOrganizer, c.method, onePath, c.body)

		if rec.Code != http.StatusNotFound || store.updateCalled || store.deleteCalled {
			t.Fatalf("%s: want 404 and no write, got %d", c.method, rec.Code)
		}
	}
}

func TestRequestRules(t *testing.T) {
	cases := []struct {
		name         string
		method, path string
		body         string
		mutate       func(*fakeStore)
		want         int
		expect       string
	}{
		{"unknown field on create", http.MethodPost, listPath, `{"event_id":"x","name":"VIP"}`, nil, 400, ""},
		{"stock cannot be patched directly", http.MethodPatch, onePath, `{"available_quantity":5}`, nil, 400, ""},
		{"currency cannot be patched", http.MethodPatch, onePath, `{"currency":"USD"}`, nil, 400, ""},
		{"empty patch", http.MethodPatch, onePath, `{}`, nil, 422, "body"},
		{"missing price", http.MethodPost, listPath, `{"name":"VIP","total_quantity":5}`, nil, 422, "price_kobo"},
		{"fractional price is not JSON we accept", http.MethodPost, listPath, `{"name":"VIP","price_kobo":10.5}`, nil, 400, ""},
		{"frozen event", http.MethodPost, listPath, createJSON(), func(s *fakeStore) { s.event.Status = "cancelled" }, 409, "event_not_editable"},
		{"duplicate name", http.MethodPost, listPath, createJSON(), func(s *fakeStore) {
			s.createErr = &pgconn.PgError{Code: "23505", ConstraintName: "ticket_types_event_name_uniq"}
		}, 409, "name_taken"},
		{"delete on a published event", http.MethodDelete, onePath, "", func(s *fakeStore) { s.event.Status = statusPublished }, 409, "event_published"},
		{"delete after sales", http.MethodDelete, onePath, "", func(s *fakeStore) { s.types[0].AvailableQuantity = 90 }, 409, "ticket_type_has_sales"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := fixture()
			store.access = db.GetEventAccessRow{IsOwner: true}
			if c.mutate != nil {
				c.mutate(store)
			}
			r, tm := newRouter(t, store)

			rec := do(r, tm, auth.RoleOrganizer, c.method, c.path, c.body)

			if rec.Code != c.want || (c.expect != "" && !strings.Contains(rec.Body.String(), c.expect)) {
				t.Fatalf("want %d containing %q, got %d: %s", c.want, c.expect, rec.Code, rec.Body)
			}
		})
	}
}

func TestManageResponseShowsStock(t *testing.T) {
	store := fixture()
	store.types[0].AvailableQuantity = 70
	store.access = db.GetEventAccessRow{IsOwner: true}
	r, tm := newRouter(t, store)

	rec := do(r, tm, auth.RoleOrganizer, http.MethodGet, onePath, "")

	for _, want := range []string{`"total_quantity":100`, `"available_quantity":70`, `"sold_or_reserved":30`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("missing %s in %s", want, rec.Body)
		}
	}
}

func TestPublicList(t *testing.T) {
	publicPath := "/v1/events/" + uuidx.String(eventA) + "/ticket-types"
	get := func(store *fakeStore) *httptest.ResponseRecorder {
		r, _ := newRouter(t, store)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, publicPath, nil)) // no token, on purpose
		return rec
	}

	t.Run("shows status and price, never stock counts", func(t *testing.T) {
		tt := existing(100, 100)
		tt.SalesStart, tt.SalesEnd = tstz(fixedNow.Add(-time.Hour)), tstz(fixedNow.Add(time.Hour))
		rec := get(&fakeStore{publicEvent: db.Event{Status: statusPublished}, publicTypes: []db.TicketType{tt}})

		body := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(body, `"sales_status":"on_sale"`) || !strings.Contains(body, `"price_kobo":500000`) {
			t.Fatalf("unexpected response: %d %s", rec.Code, body)
		}
		for _, secret := range []string{"available_quantity", "total_quantity", "sold_or_reserved"} {
			if strings.Contains(body, secret) {
				t.Fatalf("the public shape must not expose %s: %s", secret, body)
			}
		}
		if rec.Header().Get("Cache-Control") != publicCacheControl {
			t.Fatalf("missing cache header: %q", rec.Header().Get("Cache-Control"))
		}
	})

	t.Run("an event that was never public is a 404", func(t *testing.T) {
		if rec := get(&fakeStore{publicErr: pgx.ErrNoRows}); rec.Code != 404 {
			t.Fatalf("got %d", rec.Code)
		}
	})

	t.Run("a cancelled event has an empty list", func(t *testing.T) {
		rec := get(&fakeStore{publicEvent: db.Event{Status: "cancelled"}})
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
	})
}