//go:build integration

package integration

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/tests/integration/testdb"
)

// world builds the cast used by both tests below.
type world struct {
	q                                         *db.Queries
	owner, otherOrg, admin, gate, stranger    pgtype.UUID
	eventA, eventB                            pgtype.UUID
}

func newWorld(t *testing.T) world {
	t.Helper()
	pool := testdb.New(t)
	q := db.New(pool)

	w := world{
		q:        q,
		owner:    createUser(t, q, "owner@example.com"),
		otherOrg: createUser(t, q, "other@example.com"),
		admin:    createUser(t, q, "admin@example.com"),
		gate:     createUser(t, q, "gate@example.com"),
		stranger: createUser(t, q, "stranger@example.com"),
	}
	setRole(t, pool, w.owner, auth.RoleOrganizer)
	setRole(t, pool, w.otherOrg, auth.RoleOrganizer)

	w.eventA = insertEvent(t, pool, w.owner, "Event A")
	w.eventB = insertEvent(t, pool, w.otherOrg, "Event B")

	mustAddStaff(t, pool, w.eventA, w.admin, auth.StaffEventAdmin)       // also proves the CHECK accepts our constants
	mustAddStaff(t, pool, w.eventA, w.gate, auth.StaffGateAttendant)
	mustAddStaff(t, pool, w.eventA, w.otherOrg, auth.StaffGateAttendant) // owns B, gate attendant on A

	// keep the pool reachable for constraint tests
	t.Cleanup(func() {})
	return w
}

func TestGetEventAccessAgainstPostgres(t *testing.T) {
	w := newWorld(t)

	cases := []struct {
		name      string
		user      pgtype.UUID
		event     pgtype.UUID
		wantOwner bool
		wantStaff string
	}{
		{"owner on own event", w.owner, w.eventA, true, ""},
		{"owner on someone else's event", w.owner, w.eventB, false, ""},
		{"event_admin on A", w.admin, w.eventA, false, auth.StaffEventAdmin},
		{"gate_attendant on A", w.gate, w.eventA, false, auth.StaffGateAttendant},
		{"staff of A has nothing on B", w.admin, w.eventB, false, ""},
		{"stranger on an existing event", w.stranger, w.eventA, false, ""},
		{"organizer of B, gate attendant on A: on A", w.otherOrg, w.eventA, false, auth.StaffGateAttendant},
		{"organizer of B, gate attendant on A: on B", w.otherOrg, w.eventB, true, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row, err := w.q.GetEventAccess(ctx, db.GetEventAccessParams{UserID: c.user, EventID: c.event})
			if err != nil {
				t.Fatalf("an existing event must return a row, got %v", err)
			}
			if row.IsOwner != c.wantOwner || row.StaffRole != c.wantStaff {
				t.Fatalf("want owner=%v staff=%q, got owner=%v staff=%q",
					c.wantOwner, c.wantStaff, row.IsOwner, row.StaffRole)
			}
		})
	}

	t.Run("nonexistent event returns no rows", func(t *testing.T) {
		missing := pgtype.UUID{Bytes: [16]byte{0xde, 0xad}, Valid: true}
		_, err := w.q.GetEventAccess(ctx, db.GetEventAccessParams{UserID: w.owner, EventID: missing})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("want pgx.ErrNoRows, got %v", err)
		}
	})
}

func TestEventStaffConstraints(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	owner := createUser(t, q, "owner@example.com")
	staff := createUser(t, q, "staff@example.com")
	event := insertEvent(t, pool, owner, "Event")

	pgCode := func(err error) string {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return pgErr.Code
		}
		return ""
	}

	if code := pgCode(addStaff(pool, event, staff, "superuser")); code != "23514" { // check_violation
		t.Fatalf("an unknown staff role must violate the CHECK constraint, got code %q", code)
	}

	mustAddStaff(t, pool, event, staff, auth.StaffGateAttendant)
	if code := pgCode(addStaff(pool, event, staff, auth.StaffEventAdmin)); code != "23505" { // unique_violation
		t.Fatalf("a second staff row for the same user and event must be rejected, got code %q", code)
	}
}

// The real middleware on top of the real query: the Phase 2 grid, but with Postgres underneath.
func TestEventGuardsWithRealQuery(t *testing.T) {
	w := newWorld(t)
	tm, err := auth.NewTokenManager("0123456789abcdef0123456789abcdef", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	ok := func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusOK) }
	authn := auth.Authenticate(tm)
	r := chi.NewRouter()
	r.With(authn, auth.RequireEventAccess(w.q, auth.AccessManage, "eventID")).Get("/events/{eventID}/manage", ok)
	r.With(authn, auth.RequireEventAccess(w.q, auth.AccessGate, "eventID")).Get("/events/{eventID}/checkin", ok)

	call := func(user pgtype.UUID, role, action string, event pgtype.UUID) int {
		tok, _, _ := tm.Issue(uuidStr(user), uuidStr(user), role)
		req := httptest.NewRequest(http.MethodGet, "/events/"+uuidStr(event)+"/"+action, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	missing := pgtype.UUID{Bytes: [16]byte{0xde, 0xad}, Valid: true}

	checks := []struct {
		name   string
		user   pgtype.UUID
		role   string
		action string
		event  pgtype.UUID
		want   int
	}{
		{"owner manages own event", w.owner, auth.RoleOrganizer, "manage", w.eventA, 200},
		{"other organizer cannot manage it", w.otherOrg, auth.RoleOrganizer, "manage", w.eventA, 403}, // they ARE gate staff on A: some access, not enough
		{"stranger organizer is told it doesn't exist", w.stranger, auth.RoleOrganizer, "manage", w.eventA, 404},
		{"event_admin manages", w.admin, auth.RoleCustomer, "manage", w.eventA, 200},
		{"gate_attendant cannot manage", w.gate, auth.RoleCustomer, "manage", w.eventA, 403},
		{"gate_attendant checks in", w.gate, auth.RoleCustomer, "checkin", w.eventA, 200},
		{"gate staff of A has no access to B", w.gate, auth.RoleCustomer, "checkin", w.eventB, 404},
		{"platform_admin manages anything", w.stranger, auth.RolePlatformAdmin, "manage", w.eventB, 200},
		{"nonexistent event is a 404 even for admins", w.stranger, auth.RolePlatformAdmin, "manage", missing, 404},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if got := call(c.user, c.role, c.action, c.event); got != c.want {
				t.Fatalf("want %d, got %d", c.want, got)
			}
		})
	}
}