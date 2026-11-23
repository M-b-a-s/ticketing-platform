//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/events"
	"github.com/M-b-a-s/ticketing-platform/internal/httpapi"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/health"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
	"github.com/M-b-a-s/ticketing-platform/internal/tickets"
	"github.com/M-b-a-s/ticketing-platform/tests/integration/testdb"
)

// ---------------------------------------------------------------------------
// harness: the REAL router on top of a REAL migrated database
// ---------------------------------------------------------------------------

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *db.Queries
	h    chi.Router
	tm   *auth.TokenManager
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t)
	q := db.New(pool)

	tm, err := auth.NewTokenManager("0123456789abcdef0123456789abcdef", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	light := auth.Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32} // fast hashing for tests
	authSvc, err := auth.NewService(q, q, tm, light, auth.Config{})
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := httpapi.NewRouter(httpapi.Deps{
		Tokens:  tm,
		Health:  health.Liveness(),
		Auth:    auth.NewHandler(authSvc, logger),
		Events:  events.NewHandler(events.NewService(q), logger),
		Tickets: tickets.NewHandler(tickets.NewService(q), logger),
	})
	return &env{t: t, pool: pool, q: q, h: h, tm: tm}
}

// do sends one request. body may be nil, a raw JSON string, or anything json can marshal.
func (e *env) do(token, method, path string, body any) (int, map[string]any) {
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out) // empty bodies (204) leave out nil
	return rec.Code, out
}

// want sends a request and fails the test unless the status matches.
func (e *env) want(label string, status int, token, method, path string, body any) map[string]any {
	e.t.Helper()
	got, out := e.do(token, method, path, body)
	if got != status {
		e.t.Fatalf("%s: %s %s: want %d, got %d (%v)", label, method, path, status, got, out)
	}
	return out
}

func (e *env) str(out map[string]any, key string) string {
	e.t.Helper()
	s, ok := out[key].(string)
	if !ok {
		e.t.Fatalf("response has no string %q: %v", key, out)
	}
	return s
}

func errCode(out map[string]any) string {
	m, _ := out["error"].(map[string]any)
	c, _ := m["code"].(string)
	return c
}

func hasID(out map[string]any, id string) bool {
	items, _ := out["items"].([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && m["id"] == id {
			return true
		}
	}
	return false
}

type account struct {
	id    pgtype.UUID
	idStr string
	token string
}

// signup goes through the real register and login endpoints. Roles are not
// self-service, so a promotion to organizer happens in the database, before login,
// so the token carries it.
func (e *env) signup(email, role string) account {
	e.t.Helper()
	out := e.want("register "+email, http.StatusCreated, "", http.MethodPost, "/v1/auth/register",
		map[string]any{"email": email, "password": "a-long-enough-password", "full_name": "Test User"})
	idStr := e.str(out, "id")
	id, err := uuidx.Parse(idStr)
	if err != nil {
		e.t.Fatal(err)
	}
	if role != auth.RoleCustomer {
		setRole(e.t, e.pool, id, role)
	}
	out = e.want("login "+email, http.StatusOK, "", http.MethodPost, "/v1/auth/login",
		map[string]any{"email": email, "password": "a-long-enough-password"})
	return account{id: id, idStr: idStr, token: e.str(out, "access_token")}
}

// ---------------------------------------------------------------------------
// 1. Route policy: every route is classified, and each policy is enforced
// ---------------------------------------------------------------------------

type routePolicy struct {
	kind string // public | authenticated | role | event-gate | event-manage
	body string // a request body that would be valid, for routes that take one
}

// Every route the API registers MUST appear here. Adding a route without deciding
// who may call it fails TestRoutePolicy.
var apiPolicy = map[string]routePolicy{
	"GET /healthz":                          {kind: "public"},
	"POST /v1/auth/register":                {kind: "public"},
	"POST /v1/auth/login":                   {kind: "public"},
	"POST /v1/auth/refresh":                 {kind: "public"},
	"POST /v1/auth/logout":                  {kind: "public"},
	"GET /v1/auth/me":                       {kind: "authenticated"},
	"GET /v1/events":                        {kind: "public"},
	"GET /v1/events/{eventID}":              {kind: "public"},
	"GET /v1/events/{eventID}/ticket-types": {kind: "public"},

	"GET /v1/manage/events":                        {kind: "authenticated"},
	"POST /v1/manage/events":                       {kind: "role", body: `{"title":"x"}`},
	"GET /v1/manage/events/{eventID}":              {kind: "event-gate"},
	"PATCH /v1/manage/events/{eventID}":            {kind: "event-manage", body: `{"title":"hacked"}`},
	"POST /v1/manage/events/{eventID}/publish":     {kind: "event-manage"},
	"POST /v1/manage/events/{eventID}/cancel":      {kind: "event-manage"},
	"GET /v1/manage/events/{eventID}/ticket-types": {kind: "event-gate"},
	"POST /v1/manage/events/{eventID}/ticket-types": {kind: "event-manage",
		body: `{"name":"Hacked","price_kobo":1,"total_quantity":1}`},
	"GET /v1/manage/events/{eventID}/ticket-types/{ticketTypeID}":    {kind: "event-gate"},
	"PATCH /v1/manage/events/{eventID}/ticket-types/{ticketTypeID}":  {kind: "event-manage", body: `{"price_kobo":1}`},
	"DELETE /v1/manage/events/{eventID}/ticket-types/{ticketTypeID}": {kind: "event-manage"},
}

// expectation says what a caller who is NOT allowed must receive. ok=false means
// the caller is allowed, so there is no denial to check.
func expectation(kind, subject string) (status int, ok bool) {
	if subject == "anonymous" {
		return http.StatusUnauthorized, true
	}
	switch kind {
	case "role": // organizer or platform_admin only
		switch subject {
		case "stranger customer", "gate attendant", "event admin":
			return http.StatusForbidden, true
		}
	case "event-gate": // strangers learn nothing, not even that the event exists
		switch subject {
		case "stranger customer", "other organizer":
			return http.StatusNotFound, true
		}
	case "event-manage":
		switch subject {
		case "stranger customer", "other organizer":
			return http.StatusNotFound, true
		case "gate attendant": // has some access, not enough
			return http.StatusForbidden, true
		}
	}
	return 0, false
}

func TestRoutePolicy(t *testing.T) {
	e := newEnv(t)

	// 1. the policy table and the router must list exactly the same routes
	found := map[string]bool{}
	err := chi.Walk(e.h, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		found[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for route := range found {
		if _, ok := apiPolicy[route]; !ok {
			t.Errorf("route %q has no access policy: add it to apiPolicy deliberately", route)
		}
	}
	for route := range apiPolicy {
		if !found[route] {
			t.Errorf("apiPolicy lists %q but no such route is registered", route)
		}
	}
	if t.Failed() {
		t.FailNow() // do not run the matrix against a mismatched table
	}

	// 2. a small world: an event with a ticket type, and seven kinds of caller
	now := time.Now()
	owner := createUser(t, e.q, "owner@example.com")
	otherOrg := createUser(t, e.q, "other@example.com")
	stranger := createUser(t, e.q, "stranger@example.com")
	gate := createUser(t, e.q, "gate@example.com")
	evAdmin := createUser(t, e.q, "eventadmin@example.com")
	admin := createUser(t, e.q, "admin@example.com")
	setRole(t, e.pool, owner, auth.RoleOrganizer)
	setRole(t, e.pool, otherOrg, auth.RoleOrganizer)
	setRole(t, e.pool, admin, auth.RolePlatformAdmin)

	eventA := insertEventAt(t, e.pool, owner, "Event A", now.Add(48*time.Hour))
	typeA := newTT(t, e.q, eventA, "General")
	mustAddStaff(t, e.pool, eventA, gate, auth.StaffGateAttendant)
	mustAddStaff(t, e.pool, eventA, evAdmin, auth.StaffEventAdmin)

	tok := func(u pgtype.UUID, role string) string {
		token, _, err := e.tm.Issue(uuidx.String(u), uuidx.String(u), role)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	subjects := []struct{ name, token string }{
		{"anonymous", ""},
		{"stranger customer", tok(stranger, auth.RoleCustomer)},
		{"other organizer", tok(otherOrg, auth.RoleOrganizer)},
		{"gate attendant", tok(gate, auth.RoleCustomer)},
		{"event admin", tok(evAdmin, auth.RoleCustomer)},
		{"owner", tok(owner, auth.RoleOrganizer)},
		{"platform admin", tok(admin, auth.RolePlatformAdmin)},
	}

	snapshot := func() (db.Event, db.TicketType) {
		ev, err := e.q.GetEvent(ctx, eventA)
		if err != nil {
			t.Fatal(err)
		}
		tt, err := e.q.GetTicketType(ctx, db.GetTicketTypeParams{ID: typeA.ID, EventID: eventA})
		if err != nil {
			t.Fatal(err)
		}
		return ev, tt
	}
	eventBefore, typeBefore := snapshot()

	// 3. every route x every caller
	keys := make([]string, 0, len(apiPolicy))
	for k := range apiPolicy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fill := strings.NewReplacer("{eventID}", uuidx.String(eventA), "{ticketTypeID}", uuidx.String(typeA.ID))

	for _, key := range keys {
		pol := apiPolicy[key]
		method, template, _ := strings.Cut(key, " ")
		path := fill.Replace(template)

		for _, s := range subjects {
			t.Run(key+" as "+s.name, func(t *testing.T) {
				status, out := e.do(s.token, method, path, pol.body)

				if pol.kind == "public" {
					// public routes are exercised anonymously and must never demand a token
					if s.token == "" {
						if code := errCode(out); code == "missing_token" || code == "forbidden" {
							t.Fatalf("declared public, but answered %d %q", status, code)
						}
					}
					return
				}
				if want, denied := expectation(pol.kind, s.name); denied {
					if status != want {
						t.Fatalf("want %d, got %d (%v)", want, status, out)
					}
					return
				}
				// an allowed caller must be able to read; writes by allowed callers
				// are covered by the journey test, so state stays put here
				if method == http.MethodGet && status != http.StatusOK {
					t.Fatalf("an allowed caller must be able to read, got %d (%v)", status, out)
				}
			})
		}
	}

	// 4. nothing a denied caller sent may have changed anything
	eventAfter, typeAfter := snapshot()
	if !reflect.DeepEqual(eventBefore, eventAfter) || !reflect.DeepEqual(typeBefore, typeAfter) {
		t.Fatalf("a denied request changed data:\nevent before %+v\nevent after  %+v", eventBefore, eventAfter)
	}
}

// ---------------------------------------------------------------------------
// 2. The Phase 3 exit criterion, as a story
// ---------------------------------------------------------------------------

type attempt struct {
	label, method, path string
	body                any
}

func TestOrganizersCannotTouchEachOthersEvents(t *testing.T) {
	e := newEnv(t)
	alice := e.signup("alice@example.com", auth.RoleOrganizer)
	bob := e.signup("bob@example.com", auth.RoleOrganizer)
	carol := e.signup("carol@example.com", auth.RoleCustomer) // will work the gate at Alice's event

	now := time.Now().UTC()
	eventBody := func(title string) map[string]any {
		return map[string]any{
			"title": title, "venue_name": "Eko Hotel",
			"starts_at": now.Add(48 * time.Hour).Format(time.RFC3339),
			"ends_at":   now.Add(52 * time.Hour).Format(time.RFC3339),
		}
	}
	ticketBody := func(name string) map[string]any {
		return map[string]any{
			"name": name, "price_kobo": 500000, "total_quantity": 100,
			"sales_start": now.Add(-time.Hour).Format(time.RFC3339),
			"sales_end":   now.Add(24 * time.Hour).Format(time.RFC3339),
		}
	}

	// Alice builds an event with one ticket type
	out := e.want("alice creates an event", 201, alice.token, "POST", "/v1/manage/events", eventBody("Alice's concert"))
	eventID := e.str(out, "id")
	eventUUID, _ := uuidx.Parse(eventID)
	ev := "/v1/manage/events/" + eventID
	tts := ev + "/ticket-types"

	out = e.want("alice adds a ticket type", 201, alice.token, "POST", tts, ticketBody("General"))
	typeID := e.str(out, "id")
	typeUUID, _ := uuidx.Parse(typeID)
	one := tts + "/" + typeID

	// Alice sees her event; Bob's list has nothing of hers
	if !hasID(e.want("alice lists her events", 200, alice.token, "GET", "/v1/manage/events", nil), eventID) {
		t.Fatal("Alice's own event is missing from her list")
	}
	if hasID(e.want("bob lists his events", 200, bob.token, "GET", "/v1/manage/events", nil), eventID) {
		t.Fatal("Bob can see Alice's event in his list")
	}

	// Bob cannot do anything with Alice's event or its ticket types, and cannot
	// even tell that they exist
	for _, a := range []attempt{
		{"read the event", "GET", ev, nil},
		{"edit the event", "PATCH", ev, map[string]any{"title": "hacked"}},
		{"publish the event", "POST", ev + "/publish", nil},
		{"cancel the event", "POST", ev + "/cancel", nil},
		{"list its ticket types", "GET", tts, nil},
		{"add a ticket type", "POST", tts, ticketBody("Bob's freebie")},
		{"read a ticket type", "GET", one, nil},
		{"reprice a ticket type", "PATCH", one, map[string]any{"price_kobo": 1}},
		{"delete a ticket type", "DELETE", one, nil},
	} {
		e.want("bob tries to "+a.label, 404, bob.token, a.method, a.path, a.body)
	}

	// The IDOR attempt: Bob passes the guard for HIS OWN event, but names Alice's ticket type
	out = e.want("bob creates his own event", 201, bob.token, "POST", "/v1/manage/events", eventBody("Bob's gig"))
	bobTTs := "/v1/manage/events/" + e.str(out, "id") + "/ticket-types"
	for _, a := range []attempt{
		{"read Alice's ticket type through his own event", "GET", bobTTs + "/" + typeID, nil},
		{"reprice it", "PATCH", bobTTs + "/" + typeID, map[string]any{"price_kobo": 1}},
		{"delete it", "DELETE", bobTTs + "/" + typeID, nil},
	} {
		e.want("bob tries to "+a.label, 404, bob.token, a.method, a.path, a.body)
	}

	// Carol works the gate: she may look, and nothing else
	mustAddStaff(t, e.pool, eventUUID, carol.id, auth.StaffGateAttendant)
	out = e.want("carol reads the event", 200, carol.token, "GET", ev, nil)
	if out["access"] != "gate" {
		t.Fatalf("carol should see gate access, got %v", out["access"])
	}
	e.want("carol lists ticket types", 200, carol.token, "GET", tts, nil)
	e.want("carol reads a ticket type", 200, carol.token, "GET", one, nil)
	for _, a := range []attempt{
		{"edit the event", "PATCH", ev, map[string]any{"title": "hacked"}},
		{"publish the event", "POST", ev + "/publish", nil},
		{"cancel the event", "POST", ev + "/cancel", nil},
		{"add a ticket type", "POST", tts, ticketBody("Carol's special")},
		{"reprice a ticket type", "PATCH", one, map[string]any{"price_kobo": 1}},
		{"delete a ticket type", "DELETE", one, nil},
	} {
		e.want("carol tries to "+a.label, 403, carol.token, a.method, a.path, a.body)
	}

	// While it is a draft, the public cannot see it
	e.want("a draft is not public", 404, "", "GET", "/v1/events/"+eventID, nil)
	e.want("a draft's ticket types are not public", 404, "", "GET", "/v1/events/"+eventID+"/ticket-types", nil)
	if hasID(e.want("public list", 200, "", "GET", "/v1/events", nil), eventID) {
		t.Fatal("a draft appears in the public list")
	}

	// Alice publishes; now the world can see and shop
	out = e.want("alice publishes", 200, alice.token, "POST", ev+"/publish", nil)
	if out["status"] != "published" {
		t.Fatalf("want published, got %v", out["status"])
	}
	e.want("a published event is public", 200, "", "GET", "/v1/events/"+eventID, nil)
	if !hasID(e.want("public list", 200, "", "GET", "/v1/events", nil), eventID) {
		t.Fatal("a published event is missing from the public list")
	}
	out = e.want("public ticket types", 200, "", "GET", "/v1/events/"+eventID+"/ticket-types", nil)
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("want one public ticket type, got %v", items)
	}
	first := items[0].(map[string]any)
	if first["sales_status"] != "on_sale" {
		t.Fatalf("want on_sale, got %v", first["sales_status"])
	}
	if _, leaked := first["available_quantity"]; leaked {
		t.Fatal("the public ticket type leaks its stock count")
	}

	// Live events keep their ticket types, and Bob is still locked out
	out = e.want("alice cannot delete from a live event", 409, alice.token, "DELETE", one, nil)
	if errCode(out) != "event_published" {
		t.Fatalf("want event_published, got %v", out)
	}
	e.want("bob still cannot cancel", 404, bob.token, "POST", ev+"/cancel", nil)

	// Alice cancels: the public sees it cancelled, with nothing on sale, and it is frozen
	out = e.want("alice cancels", 200, alice.token, "POST", ev+"/cancel", nil)
	if out["status"] != "cancelled" {
		t.Fatalf("want cancelled, got %v", out["status"])
	}
	out = e.want("a cancelled event is still viewable", 200, "", "GET", "/v1/events/"+eventID, nil)
	if out["status"] != "cancelled" {
		t.Fatalf("want cancelled, got %v", out["status"])
	}
	out = e.want("nothing is on sale", 200, "", "GET", "/v1/events/"+eventID+"/ticket-types", nil)
	if items, _ := out["items"].([]any); len(items) != 0 {
		t.Fatalf("a cancelled event must have nothing on sale, got %v", items)
	}
	if hasID(e.want("public list", 200, "", "GET", "/v1/events", nil), eventID) {
		t.Fatal("a cancelled event appears in the public list")
	}
	out = e.want("a cancelled event is frozen", 409, alice.token, "PATCH", ev, map[string]any{"title": "too late"})
	if errCode(out) != "event_not_editable" {
		t.Fatalf("want event_not_editable, got %v", out)
	}

	// The database agrees: none of Bob's or Carol's attempts changed anything
	gotEvent, err := e.q.GetEvent(ctx, eventUUID)
	if err != nil || gotEvent.Title != "Alice's concert" || gotEvent.Status != "cancelled" {
		t.Fatalf("the event was tampered with: %+v %v", gotEvent, err)
	}
	gotType, err := e.q.GetTicketType(ctx, db.GetTicketTypeParams{ID: typeUUID, EventID: eventUUID})
	if err != nil || gotType.PriceKobo != 500000 || gotType.Name != "General" {
		t.Fatalf("the ticket type was tampered with: %+v %v", gotType, err)
	}
}