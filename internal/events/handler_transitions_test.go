package events

import (
	"net/http"
	"strings"
	"testing"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

func TestPublishAndCancelGuards(t *testing.T) {
	cases := []struct {
		name   string
		role   string
		access db.GetEventAccessRow
		want   int
	}{
		{"anonymous", "", db.GetEventAccessRow{}, 401},
		{"owner", auth.RoleOrganizer, db.GetEventAccessRow{IsOwner: true}, 200},
		{"event_admin", auth.RoleCustomer, db.GetEventAccessRow{StaffRole: auth.StaffEventAdmin}, 200},
		{"gate attendant", auth.RoleCustomer, db.GetEventAccessRow{StaffRole: auth.StaffGateAttendant}, 403},
		{"other organizer", auth.RoleOrganizer, db.GetEventAccessRow{}, 404},
		{"platform_admin", auth.RolePlatformAdmin, db.GetEventAccessRow{}, 200},
	}

	for _, action := range []string{"publish", "cancel"} {
		for _, c := range cases {
			t.Run(action+"/"+c.name, func(t *testing.T) {
				store := storeWith(c.access)
				store.count = 1
				r, tm := newTestRouter(t, store)

				rec := do(r, tm, c.role, http.MethodPost, managePath+"/"+action, "")

				if rec.Code != c.want {
					t.Fatalf("want %d, got %d: %s", c.want, rec.Code, rec.Body)
				}
				if wrote := store.publishCalled || store.cancelCalled; wrote != (c.want == 200) {
					t.Fatalf("write happened=%v, which is wrong for this case", wrote)
				}
			})
		}
	}
}

func TestPublishResponses(t *testing.T) {
	owner := db.GetEventAccessRow{IsOwner: true}

	t.Run("success shows the new status", func(t *testing.T) {
		store := storeWith(owner)
		store.count = 1
		r, tm := newTestRouter(t, store)

		rec := do(r, tm, auth.RoleOrganizer, http.MethodPost, managePath+"/publish", "")

		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"published"`) {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
	})

	t.Run("no ticket types is a 422 naming the field", func(t *testing.T) {
		store := storeWith(owner) // count stays 0
		r, tm := newTestRouter(t, store)

		rec := do(r, tm, auth.RoleOrganizer, http.MethodPost, managePath+"/publish", "")

		if rec.Code != 422 || !strings.Contains(rec.Body.String(), "ticket_types") || store.publishCalled {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
	})

	t.Run("repeating publish is a harmless 200", func(t *testing.T) {
		store := storeWith(owner)
		store.event.Status = StatusPublished
		r, tm := newTestRouter(t, store)

		rec := do(r, tm, auth.RoleOrganizer, http.MethodPost, managePath+"/publish", "")

		if rec.Code != 200 || store.publishCalled {
			t.Fatalf("got %d called=%v", rec.Code, store.publishCalled)
		}
	})
}

func TestIllegalTransitionsAre409(t *testing.T) {
	owner := db.GetEventAccessRow{IsOwner: true}
	cases := []struct{ name, status, action string }{
		{"publish a cancelled event", StatusCancelled, "publish"},
		{"publish a completed event", StatusCompleted, "publish"},
		{"cancel a completed event", StatusCompleted, "cancel"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := storeWith(owner)
			store.event.Status = c.status
			store.count = 1
			r, tm := newTestRouter(t, store)

			rec := do(r, tm, auth.RoleOrganizer, http.MethodPost, managePath+"/"+c.action, "")

			if rec.Code != 409 || !strings.Contains(rec.Body.String(), "invalid_status_transition") {
				t.Fatalf("got %d: %s", rec.Code, rec.Body)
			}
		})
	}
}