package auth

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	testUserID    = "00000000-0000-0000-0000-000000000009"
	testSessionID = "00000000-0000-0000-0000-000000000001"
)

// protected wraps a handler that records whether it ran and what identity it saw.
func protected(tm *TokenManager) (http.Handler, *bool, *Identity) {
	called, seen := false, Identity{}
	h := Authenticate(tm)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		seen, _ = IdentityFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	return h, &called, &seen
}

func requestWith(h http.Handler, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthenticateAcceptsValidToken(t *testing.T) {
	tm := newTestManager(t)
	h, called, seen := protected(tm)
	tok, _, _ := tm.Issue(testUserID, testSessionID, "organizer")

	rec := requestWith(h, "Bearer "+tok)

	if rec.Code != http.StatusOK || !*called {
		t.Fatalf("want 200 and handler called, got %d called=%v", rec.Code, *called)
	}
	if seen.UserID != testUserID || seen.SessionID != testSessionID || seen.Role != "organizer" {
		t.Fatalf("unexpected identity: %+v", *seen)
	}
}

func TestAuthenticateRejects(t *testing.T) {
	tm := newTestManager(t)

	expiredIssuer := newTestManager(t)
	expiredIssuer.now = func() time.Time { return time.Now().Add(-time.Hour) }
	expired, _, _ := expiredIssuer.Issue(testUserID, testSessionID, "customer")

	foreign, _ := NewTokenManager("ffffffffffffffffffffffffffffffff", 15*time.Minute)
	foreignTok, _, _ := foreign.Issue(testUserID, testSessionID, "customer")

	badSubject, _, _ := tm.Issue("not-a-uuid", testSessionID, "customer")
	refreshToken, _, _ := NewRefreshToken()
	valid, _, _ := tm.Issue(testUserID, testSessionID, "customer")

	cases := map[string]string{
		"no header":            "",
		"wrong scheme":         "Basic " + valid,
		"scheme only":          "Bearer",
		"empty token":          "Bearer    ",
		"garbage":              "Bearer not.a.jwt",
		"expired":              "Bearer " + expired,
		"other secret":         "Bearer " + foreignTok,
		"non-uuid subject":     "Bearer " + badSubject,
		"refresh token as JWT": "Bearer " + refreshToken,
		"token without scheme": valid,
	}

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			h, called, _ := protected(tm)

			rec := requestWith(h, header)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d", rec.Code)
			}
			if *called {
				t.Fatal("the protected handler must not run")
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 must carry a WWW-Authenticate header")
			}
		})
	}
}

func TestAuthenticateAcceptsLowercaseScheme(t *testing.T) {
	tm := newTestManager(t)
	h, _, _ := protected(tm)
	tok, _, _ := tm.Issue(testUserID, testSessionID, "customer")

	if rec := requestWith(h, "bearer "+tok); rec.Code != http.StatusOK {
		t.Fatalf("auth schemes are case-insensitive, got %d", rec.Code)
	}
}

func TestIdentityFromWithoutMiddleware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := IdentityFrom(req.Context()); ok {
		t.Fatal("no identity should exist outside Authenticate")
	}
}

func TestIdentityUserUUID(t *testing.T) {
	u, err := Identity{UserID: testUserID}.UserUUID()
	if err != nil || !u.Valid || uuidString(u) != testUserID {
		t.Fatalf("round trip failed: %v %v", u, err)
	}
}

func TestMeEndpoint(t *testing.T) {
	svc := newSvc(&fakeStore{}, nil)
	r := chi.NewRouter()
	NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes(r)

	tok, _, _ := svc.tokens.Issue(testUserID, testSessionID, "customer")

	call := func(header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := call("Bearer " + tok)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testUserID) {
		t.Fatalf("want 200 with the user id, got %d: %s", rec.Code, rec.Body)
	}
	if rec := call(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without a token, got %d", rec.Code)
	}
}