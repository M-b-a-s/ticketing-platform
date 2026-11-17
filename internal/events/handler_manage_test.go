package events

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

const managePath = "/v1/manage/events/00000000-0000-0000-0000-000000000005"

func storeWith(access db.GetEventAccessRow) *fakeStore {
	ev := currentEvent()
	ev.ID = pgtype.UUID{Bytes: [16]byte{5}, Valid: true}
	ev.Title = "Original"
	return &fakeStore{event: ev, access: access}
}

func TestSingleEventGuards(t *testing.T) {
	owner := db.GetEventAccessRow{IsOwner: true}
	eventAdmin := db.GetEventAccessRow{StaffRole: auth.StaffEventAdmin}
	gate := db.GetEventAccessRow{StaffRole: auth.StaffGateAttendant}
	stranger := db.GetEventAccessRow{}

	cases := []struct {
		name   string
		role   string
		access db.GetEventAccessRow
		method string
		want   int
	}{
		{"anonymous GET", "", stranger, http.MethodGet, 401},
		{"anonymous PATCH", "", stranger, http.MethodPatch, 401},
		{"owner GET", auth.RoleOrganizer, owner, http.MethodGet, 200},
		{"owner PATCH", auth.RoleOrganizer, owner, http.MethodPatch, 200},
		{"event_admin GET", auth.RoleCustomer, eventAdmin, http.MethodGet, 200},
		{"event_admin PATCH", auth.RoleCustomer, eventAdmin, http.MethodPatch, 200},
		{"gate attendant GET", auth.RoleCustomer, gate, http.MethodGet, 200},
		{"gate attendant PATCH", auth.RoleCustomer, gate, http.MethodPatch, 403},
		{"other organizer GET", auth.RoleOrganizer, stranger, http.MethodGet, 404},
		{"other organizer PATCH", auth.RoleOrganizer, stranger, http.MethodPatch, 404},
		{"platform_admin GET", auth.RolePlatformAdmin, stranger, http.MethodGet, 200},
		{"platform_admin PATCH", auth.RolePlatformAdmin, stranger, http.MethodPatch, 200},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := storeWith(c.access)
			r, tm := newTestRouter(t, store)
			body := ""
			if c.method == http.MethodPatch {
				body = `{"title":"Renamed"}`
			}

			rec := do(r, tm, c.role, c.method, managePath, body)

			if rec.Code != c.want {
				t.Fatalf("want %d, got %d: %s", c.want, rec.Code, rec.Body)
			}
			if wrote := store.updateCalled; wrote != (c.method == http.MethodPatch && c.want == 200) {
				t.Fatalf("update called=%v, which is wrong for this case", wrote)
			}
		})
	}
}

func TestMissingEventIs404ForEveryone(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		store := storeWith(db.GetEventAccessRow{})
		store.accessErr = pgx.ErrNoRows
		r, tm := newTestRouter(t, store)

		if rec := do(r, tm, auth.RolePlatformAdmin, method, managePath, `{"title":"x"}`); rec.Code != 404 {
			t.Fatalf("%s on a nonexistent event: want 404, got %d", method, rec.Code)
		}
	}
}

func TestGetReportsTheCallersAccessLevel(t *testing.T) {
	r, tm := newTestRouter(t, storeWith(db.GetEventAccessRow{IsOwner: true}))
	if rec := do(r, tm, auth.RoleOrganizer, http.MethodGet, managePath, ""); !strings.Contains(rec.Body.String(), `"access":"manage"`) {
		t.Fatalf("owner should see access=manage: %s", rec.Body)
	}

	r, tm = newTestRouter(t, storeWith(db.GetEventAccessRow{StaffRole: auth.StaffGateAttendant}))
	if rec := do(r, tm, auth.RoleCustomer, http.MethodGet, managePath, ""); !strings.Contains(rec.Body.String(), `"access":"gate"`) {
		t.Fatalf("gate attendant should see access=gate: %s", rec.Body)
	}
}

func TestPatchRequestRules(t *testing.T) {
	owner := db.GetEventAccessRow{IsOwner: true}
	past := fixedNow.Add(-time.Hour).Format(time.RFC3339)

	cases := []struct {
		name   string
		body   string
		status string // event status in the database; "" keeps the draft
		want   int
		expect string
	}{
		{"cannot patch status", `{"status":"published"}`, "", 400, ""},
		{"cannot patch the owner", `{"organizer_id":"00000000-0000-0000-0000-000000000009"}`, "", 400, ""},
		{"bad timestamp", `{"starts_at":"soon"}`, "", 400, ""},
		{"empty patch", `{}`, "", 422, "body"},
		{"invalid title", `{"title":""}`, "", 422, "title"},
		{"start in the past", fmt.Sprintf(`{"starts_at":%q}`, past), "", 422, "starts_at"},
		{"cancelled events are frozen", `{"title":"x"}`, StatusCancelled, 409, "event_not_editable"},
		{"valid rename", `{"title":"Renamed"}`, "", 200, "Renamed"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := storeWith(owner)
			if c.status != "" {
				store.event.Status = c.status
			}
			r, tm := newTestRouter(t, store)

			rec := do(r, tm, auth.RoleOrganizer, http.MethodPatch, managePath, c.body)

			if rec.Code != c.want || (c.expect != "" && !strings.Contains(rec.Body.String(), c.expect)) {
				t.Fatalf("want %d containing %q, got %d: %s", c.want, c.expect, rec.Code, rec.Body)
			}
			if c.want != 200 && store.updateCalled {
				t.Fatal("a rejected patch must not reach the database")
			}
		})
	}
}