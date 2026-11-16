package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/httpx"
)

// ---------------------------------------------------------------------------
// Part 1: the full role x guard grid
// ---------------------------------------------------------------------------

const (
	uCustomer = "00000000-0000-0000-0000-000000000001" // plain customer, no event ties
	uOwnerA   = "00000000-0000-0000-0000-000000000002" // organizer who owns event A
	uOrgB     = "00000000-0000-0000-0000-000000000003" // organizer who owns event B
	uAdminA   = "00000000-0000-0000-0000-000000000004" // customer staffed as event_admin on A
	uGateA    = "00000000-0000-0000-0000-000000000005" // customer staffed as gate_attendant on A
	uPlatform = "00000000-0000-0000-0000-000000000006" // platform_admin

	eventA = "00000000-0000-0000-0000-0000000000a1"
	eventB = "00000000-0000-0000-0000-0000000000b1"
)

// matrixDB behaves like the real GetEventAccess query: it is keyed by BOTH the
// caller and the event, so tests prove access to one event never leaks to another.
type matrixDB struct {
	events map[string]bool
	rows   map[[2]string]db.GetEventAccessRow // {userID, eventID}
}

func newMatrixDB() *matrixDB {
	return &matrixDB{
		events: map[string]bool{eventA: true, eventB: true},
		rows: map[[2]string]db.GetEventAccessRow{
			{uOwnerA, eventA}: {IsOwner: true},
			{uOrgB, eventB}:   {IsOwner: true},
			{uAdminA, eventA}: {StaffRole: StaffEventAdmin},
			{uGateA, eventA}:  {StaffRole: StaffGateAttendant},
		},
	}
}

func (m *matrixDB) GetEventAccess(_ context.Context, arg db.GetEventAccessParams) (db.GetEventAccessRow, error) {
	event := uuidString(arg.EventID)
	if !m.events[event] {
		return db.GetEventAccessRow{}, pgx.ErrNoRows
	}
	return m.rows[[2]string{uuidString(arg.UserID), event}], nil // zero row = no relationship
}

// matrixRouter has one route per kind of guard we use.
func matrixRouter(tm *TokenManager, store EventAccessStore) http.Handler {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	authn := Authenticate(tm)

	r := chi.NewRouter()
	r.With(authn).Get("/authed", ok)
	r.With(authn, RequireRole(RoleOrganizer, RolePlatformAdmin)).Get("/org", ok)
	r.With(authn, RequireRole(RolePlatformAdmin)).Get("/admin", ok)
	r.With(authn, RequireEventAccess(store, AccessManage, "eventID")).Get("/events/{eventID}/manage", ok)
	r.With(authn, RequireEventAccess(store, AccessGate, "eventID")).Get("/events/{eventID}/checkin", ok)
	return r
}

// The order of this list is the column order of every `want` row below.
var matrixProbes = []string{
	"/authed",                         // any signed-in user
	"/org",                            // organizer or platform_admin
	"/admin",                          // platform_admin only
	"/events/" + eventA + "/manage",   // manage event A
	"/events/" + eventA + "/checkin",  // check in at event A
	"/events/" + eventB + "/manage",   // manage event B
	"/events/" + eventB + "/checkin",  // check in at event B
}

func TestAuthorizationMatrix(t *testing.T) {
	tm := newTestManager(t)
	router := matrixRouter(tm, newMatrixDB())

	issue := func(user, role string) func() string {
		return func() string { tok, _, _ := tm.Issue(user, testSessionID, role); return tok }
	}
	all := func(code int) [7]int { return [7]int{code, code, code, code, code, code, code} }

	expiredIssuer := newTestManager(t)
	expiredIssuer.now = func() time.Time { return time.Now().Add(-time.Hour) }
	foreign, _ := NewTokenManager("ffffffffffffffffffffffffffffffff", 15*time.Minute)

	subjects := []struct {
		name  string
		token func() string
		//                 authed org admin mgrA gateA mgrB gateB
		want [7]int
	}{
		{"anonymous", func() string { return "" }, all(401)},

		{"customer, no ties", issue(uCustomer, RoleCustomer), [7]int{200, 403, 403, 404, 404, 404, 404}},
		{"organizer owning A", issue(uOwnerA, RoleOrganizer), [7]int{200, 200, 403, 200, 200, 404, 404}},
		{"organizer owning B", issue(uOrgB, RoleOrganizer), [7]int{200, 200, 403, 404, 404, 200, 200}},
		{"event_admin on A", issue(uAdminA, RoleCustomer), [7]int{200, 403, 403, 200, 200, 404, 404}},
		{"gate_attendant on A", issue(uGateA, RoleCustomer), [7]int{200, 403, 403, 403, 200, 404, 404}},
		{"platform_admin", issue(uPlatform, RolePlatformAdmin), all(200)},

		// the owner of A after being demoted: ownership no longer counts
		{"demoted owner of A (now customer)", issue(uOwnerA, RoleCustomer), [7]int{200, 403, 403, 404, 404, 404, 404}},

		// tokens that must never work, whatever they claim
		{"expired token", func() string { tok, _, _ := expiredIssuer.Issue(uPlatform, testSessionID, RolePlatformAdmin); return tok }, all(401)},
		{"forged admin token (other secret)", func() string { tok, _, _ := foreign.Issue(uPlatform, testSessionID, RolePlatformAdmin); return tok }, all(401)},
		{"validly signed but unknown role", issue(uCustomer, "superuser"), all(401)},
	}

	for _, s := range subjects {
		t.Run(s.name, func(t *testing.T) {
			token := s.token()
			for i, path := range matrixProbes {
				if got := getAs(router, path, token); got != s.want[i] {
					t.Errorf("GET %s: want %d, got %d", path, s.want[i], got)
				}
			}
		})
	}
}

func TestStaffRemovalTakesEffectImmediately(t *testing.T) {
	tm := newTestManager(t)
	store := newMatrixDB()
	router := matrixRouter(tm, store)
	tok, _, _ := tm.Issue(uGateA, testSessionID, RoleCustomer)
	checkin := "/events/" + eventA + "/checkin"

	if got := getAs(router, checkin, tok); got != 200 {
		t.Fatalf("assigned gate attendant should get in, got %d", got)
	}

	delete(store.rows, [2]string{uGateA, eventA}) // removed from event_staff

	if got := getAs(router, checkin, tok); got != 404 {
		t.Fatalf("access must end as soon as the staff row is gone, even with a still-valid token; got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Part 2: deny by default for the auth routes themselves
// ---------------------------------------------------------------------------

// Every route registered by Handler.Routes must appear here, on purpose.
// Adding a route without deciding who may call it fails TestEveryAuthRouteIsClassified.
var authRoutePolicy = map[string]string{
	"POST /auth/register": "public",
	"POST /auth/login":    "public",
	"POST /auth/refresh":  "public",
	"POST /auth/logout":   "public",
	"GET /auth/me":        "authenticated",
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func callRoute(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEveryAuthRouteIsClassified(t *testing.T) {
	r := chi.NewRouter()
	NewHandler(newSvc(&fakeStore{}, nil), quietLogger()).Routes(r)

	found := map[string]bool{}
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		found[method+" "+route] = true
		return nil
	})

	for route := range found {
		if _, ok := authRoutePolicy[route]; !ok {
			t.Errorf("route %q has no access policy: add it to authRoutePolicy as public or authenticated", route)
		}
	}
	for route := range authRoutePolicy {
		if !found[route] {
			t.Errorf("authRoutePolicy lists %q but no such route is registered", route)
		}
	}
}

func TestAuthenticatedAuthRoutesEnforceAuthentication(t *testing.T) {
	svc := newSvc(&fakeStore{}, nil)
	r := chi.NewRouter()
	NewHandler(svc, quietLogger()).Routes(r)

	expiredIssuer := newTestManager(t)
	expiredIssuer.now = func() time.Time { return time.Now().Add(-time.Hour) }
	expired, _, _ := expiredIssuer.Issue(testUserID, testSessionID, RoleCustomer)

	for route, policy := range authRoutePolicy {
		if policy != "authenticated" {
			continue
		}
		method, path, _ := strings.Cut(route, " ")

		for name, token := range map[string]string{"anonymous": "", "garbage": "not.a.jwt", "expired": expired} {
			if got := callRoute(r, method, path, token, "").Code; got != http.StatusUnauthorized {
				t.Errorf("%s as %s: want 401, got %d", route, name, got)
			}
		}
		for _, role := range []string{RoleCustomer, RoleOrganizer, RolePlatformAdmin} {
			tok, _, _ := svc.tokens.Issue(testUserID, testSessionID, role)
			if got := callRoute(r, method, path, tok, "").Code; got != http.StatusOK {
				t.Errorf("%s as %s: want 200, got %d", route, role, got)
			}
		}
	}
}

func TestPublicAuthRoutesNeverRequireAToken(t *testing.T) {
	r := chi.NewRouter()
	NewHandler(newSvc(&fakeStore{}, nil), quietLogger()).Routes(r)

	for route, policy := range authRoutePolicy {
		if policy != "public" {
			continue
		}
		method, path, _ := strings.Cut(route, " ")

		rec := callRoute(r, method, path, "", `{}`)

		var eb httpx.ErrorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &eb) // empty bodies (204) simply leave Code empty
		if eb.Error.Code == "missing_token" {
			t.Errorf("%s is declared public but demanded a token", route)
		}
	}
}