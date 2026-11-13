package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

const testEventID = "00000000-0000-0000-0000-0000000000aa"

type fakeAccessStore struct {
	row   db.GetEventAccessRow
	err   error
	calls int
	got   db.GetEventAccessParams
}

func (f *fakeAccessStore) GetEventAccess(_ context.Context, arg db.GetEventAccessParams) (db.GetEventAccessRow, error) {
	f.calls++
	f.got = arg
	return f.row, f.err
}

func accessRouter(tm *TokenManager, store EventAccessStore) http.Handler {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	r := chi.NewRouter()
	r.With(Authenticate(tm), RequireEventAccess(store, AccessManage, "eventID")).Get("/events/{eventID}/manage", ok)
	r.With(Authenticate(tm), RequireEventAccess(store, AccessGate, "eventID")).Get("/events/{eventID}/checkin", ok)
	return r
}

func getAs(h http.Handler, path, token string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestEventAccessMatrix(t *testing.T) {
	tm := newTestManager(t)

	cases := []struct {
		name      string
		role      string
		isOwner   bool
		staffRole string
		route     string // "manage" or "checkin"
		want      int
	}{
		{"admin, unrelated, manage", RolePlatformAdmin, false, "", "manage", 200},
		{"admin, unrelated, checkin", RolePlatformAdmin, false, "", "checkin", 200},
		{"owner organizer, manage", RoleOrganizer, true, "", "manage", 200},
		{"owner organizer, checkin", RoleOrganizer, true, "", "checkin", 200},
		{"other organizer, manage", RoleOrganizer, false, "", "manage", 404},
		{"other organizer, checkin", RoleOrganizer, false, "", "checkin", 404},
		{"demoted owner (now customer), manage", RoleCustomer, true, "", "manage", 404},
		{"customer, unrelated, checkin", RoleCustomer, false, "", "checkin", 404},
		{"customer as event_admin, manage", RoleCustomer, false, StaffEventAdmin, "manage", 200},
		{"customer as event_admin, checkin", RoleCustomer, false, StaffEventAdmin, "checkin", 200},
		{"customer as gate_attendant, checkin", RoleCustomer, false, StaffGateAttendant, "checkin", 200},
		{"customer as gate_attendant, manage", RoleCustomer, false, StaffGateAttendant, "manage", 403},
		{"other organizer as gate_attendant, manage", RoleOrganizer, false, StaffGateAttendant, "manage", 403},
		{"other organizer as gate_attendant, checkin", RoleOrganizer, false, StaffGateAttendant, "checkin", 200},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeAccessStore{row: db.GetEventAccessRow{IsOwner: c.isOwner, StaffRole: c.staffRole}}
			tok, _, _ := tm.Issue(testUserID, testSessionID, c.role)

			code := getAs(accessRouter(tm, store), "/events/"+testEventID+"/"+c.route, tok)

			if code != c.want {
				t.Fatalf("want %d, got %d", c.want, code)
			}
		})
	}
}

func TestEventAccessQueriesWithCallerAndURLEvent(t *testing.T) {
	tm := newTestManager(t)
	store := &fakeAccessStore{row: db.GetEventAccessRow{IsOwner: true}}
	tok, _, _ := tm.Issue(testUserID, testSessionID, RoleOrganizer)

	getAs(accessRouter(tm, store), "/events/"+testEventID+"/manage", tok)

	if uuidString(store.got.UserID) != testUserID || uuidString(store.got.EventID) != testEventID {
		t.Fatalf("lookup used the wrong IDs: user=%s event=%s",
			uuidString(store.got.UserID), uuidString(store.got.EventID))
	}
}

func TestEventAccessEdgeCases(t *testing.T) {
	tm := newTestManager(t)
	tok, _, _ := tm.Issue(testUserID, testSessionID, RolePlatformAdmin)

	t.Run("event does not exist", func(t *testing.T) {
		store := &fakeAccessStore{err: pgx.ErrNoRows}
		if code := getAs(accessRouter(tm, store), "/events/"+testEventID+"/manage", tok); code != 404 {
			t.Fatalf("want 404, got %d", code)
		}
	})

	t.Run("malformed event id never reaches the database", func(t *testing.T) {
		store := &fakeAccessStore{}
		if code := getAs(accessRouter(tm, store), "/events/not-a-uuid/manage", tok); code != 404 || store.calls != 0 {
			t.Fatalf("want 404 and no query, got %d calls=%d", code, store.calls)
		}
	})

	t.Run("database failure", func(t *testing.T) {
		store := &fakeAccessStore{err: errors.New("connection reset")}
		if code := getAs(accessRouter(tm, store), "/events/"+testEventID+"/manage", tok); code != 500 {
			t.Fatalf("want 500, got %d", code)
		}
	})

	t.Run("no token", func(t *testing.T) {
		store := &fakeAccessStore{}
		if code := getAs(accessRouter(tm, store), "/events/"+testEventID+"/manage", ""); code != 401 || store.calls != 0 {
			t.Fatalf("want 401 and no query, got %d calls=%d", code, store.calls)
		}
	})
}

func TestRequireEventAccessRejectsNoneLevel(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	RequireEventAccess(&fakeAccessStore{}, AccessNone, "eventID")
}