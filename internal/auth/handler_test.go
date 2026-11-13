package auth

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func newTestRouter(store UserStore) http.Handler {
	h := NewHandler(NewService(store, testParams), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	h.Routes(r)
	return r
}

func post(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

const validBody = `{"email":"ada@example.com","password":"a-long-enough-password","full_name":"Ada Obi"}`

func TestRegisterHandlerCreated(t *testing.T) {
	rec := post(t, newTestRouter(&fakeStore{}), validBody)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("response must never contain password data: %s", rec.Body)
	}
}

func TestRegisterHandlerRejectsRoleField(t *testing.T) {
	body := `{"email":"ada@example.com","password":"a-long-enough-password","full_name":"Ada","role":"admin"}`
	store := &fakeStore{}

	rec := post(t, newTestRouter(store), body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	if store.called {
		t.Fatal("store must not be called")
	}
}

func TestRegisterHandlerValidation(t *testing.T) {
	rec := post(t, newTestRouter(&fakeStore{}), `{"email":"nope","password":"short","full_name":""}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", rec.Code)
	}
	for _, f := range []string{"email", "password", "full_name"} {
		if !strings.Contains(rec.Body.String(), f) {
			t.Fatalf("expected field %q in error body: %s", f, rec.Body)
		}
	}
}

func TestRegisterHandlerDuplicateEmail(t *testing.T) {
	store := &fakeStore{err: &pgconn.PgError{Code: "23505", ConstraintName: "users_email_uniq"}}

	rec := post(t, newTestRouter(store), validBody)

	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rec.Code)
	}
}

func TestRegisterHandlerMalformedJSON(t *testing.T) {
	rec := post(t, newTestRouter(&fakeStore{}), `{not json`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}