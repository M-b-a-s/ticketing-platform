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

func TestLogoutRevokesSession(t *testing.T) {
	svc, sessions, token := loggedIn(t)

	if err := svc.Logout(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if !sessions.sessions[0].revoked {
		t.Fatal("logout must revoke the session")
	}

	// the token no longer works, and it is a plain invalid token, not a "reuse" alarm
	_, err := svc.Refresh(context.Background(), token)
	if !errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrRefreshReuse) {
		t.Fatalf("expected plain invalid token after logout, got %v", err)
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	svc, _, token := loggedIn(t)

	for _, tok := range []string{token, token, "never-existed", ""} {
		if err := svc.Logout(context.Background(), tok); err != nil {
			t.Fatalf("logout(%q) should not error, got %v", tok, err)
		}
	}
}

func TestLogoutOnlyAffectsThatSession(t *testing.T) {
	svc, sessions, firstToken := loggedIn(t)

	second, err := svc.Login(context.Background(), LoginInput{Email: "ada@example.com", Password: "a-long-enough-password"})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Logout(context.Background(), firstToken); err != nil {
		t.Fatal(err)
	}

	if !sessions.sessions[0].revoked || sessions.sessions[1].revoked {
		t.Fatal("logging out one device must not log out the others")
	}
	if _, err := svc.Refresh(context.Background(), second.RefreshToken); err != nil {
		t.Fatalf("the other session should still refresh: %v", err)
	}
}

func TestLogoutHandler(t *testing.T) {
	svc, _, token := loggedIn(t)
	r := chi.NewRouter()
	NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes(r)

	call := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call(`{"refresh_token":"` + token + `"}`); code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", code)
	}
	if code := call(`{"refresh_token":"garbage"}`); code != http.StatusNoContent {
		t.Fatalf("unknown token should also be 204, got %d", code)
	}
	if code := call(`{bad json`); code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", code)
	}
}