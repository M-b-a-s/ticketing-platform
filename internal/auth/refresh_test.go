package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// loggedIn returns a service with one logged-in user and their first refresh token.
func loggedIn(t *testing.T) (*Service, *fakeSessions, string) {
	t.Helper()
	sessions := &fakeSessions{}
	svc := newSvc(existingUser(t, "a-long-enough-password"), sessions)

	res, err := svc.Login(context.Background(), LoginInput{Email: "ada@example.com", Password: "a-long-enough-password"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, sessions, res.RefreshToken
}

func TestRefreshRotatesToken(t *testing.T) {
	svc, sessions, oldToken := loggedIn(t)

	res, err := svc.Refresh(context.Background(), oldToken)
	if err != nil {
		t.Fatal(err)
	}

	if res.RefreshToken == oldToken {
		t.Fatal("refresh must issue a new refresh token")
	}
	s := sessions.sessions[0]
	if s.hash != HashRefreshToken(res.RefreshToken) || s.prev != HashRefreshToken(oldToken) {
		t.Fatal("session must hold the new hash and remember the old one")
	}
	if claims, err := svc.tokens.Verify(res.AccessToken); err != nil || claims.Role != "customer" {
		t.Fatalf("bad access token: %+v %v", claims, err)
	}
}

func TestRefreshChainKeepsWorking(t *testing.T) {
	svc, _, token := loggedIn(t)

	for i := 0; i < 3; i++ {
		res, err := svc.Refresh(context.Background(), token)
		if err != nil {
			t.Fatalf("refresh #%d failed: %v", i+1, err)
		}
		token = res.RefreshToken
	}
}

func TestRefreshReuseRevokesSession(t *testing.T) {
	svc, sessions, oldToken := loggedIn(t)

	fresh, err := svc.Refresh(context.Background(), oldToken) // legitimate rotation
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Refresh(context.Background(), oldToken) // replay of the old token
	if !errors.Is(err, ErrRefreshReuse) || !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected reuse error, got %v", err)
	}
	if !sessions.sessions[0].revoked {
		t.Fatal("reuse must revoke the session")
	}

	// the newest token, which the legitimate client holds, is dead too
	if _, err := svc.Refresh(context.Background(), fresh.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked session must reject its newest token, got %v", err)
	}
}

func TestRefreshUnknownToken(t *testing.T) {
	svc, _, _ := loggedIn(t)

	_, err := svc.Refresh(context.Background(), "made-up-token")
	if !errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrRefreshReuse) {
		t.Fatalf("expected plain invalid token, got %v", err)
	}
}

func TestRefreshExpiredSession(t *testing.T) {
	svc, sessions, token := loggedIn(t)
	sessions.sessions[0].expired = true

	if _, err := svc.Refresh(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired session must be rejected, got %v", err)
	}
}

func TestRefreshEmptyToken(t *testing.T) {
	svc, _, _ := loggedIn(t)

	if _, err := svc.Refresh(context.Background(), ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestRefreshHandler(t *testing.T) {
	svc, _, token := loggedIn(t)
	r := chi.NewRouter()
	NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes(r)

	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	if rec := call(`{"refresh_token":"` + token + `"}`); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "refresh_token") {
		t.Fatalf("want 200 with new tokens, got %d: %s", rec.Code, rec.Body)
	}

	// the first token is now stale: replaying it is a 401
	if rec := call(`{"refresh_token":"` + token + `"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 on reuse, got %d", rec.Code)
	}
}