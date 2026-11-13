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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

type fakeLoginStore struct {
	user  db.GetUserByEmailRow
	found bool
}

func (f *fakeLoginStore) CreateUser(context.Context, db.CreateUserParams) (db.CreateUserRow, error) {
	return db.CreateUserRow{}, nil
}

func (f *fakeLoginStore) GetUserByEmail(context.Context, string) (db.GetUserByEmailRow, error) {
	if !f.found {
		return db.GetUserByEmailRow{}, pgx.ErrNoRows
	}
	return f.user, nil
}

type fakeSessions struct {
	created []db.CreateSessionParams
}

func (f *fakeSessions) CreateSession(_ context.Context, arg db.CreateSessionParams) (db.CreateSessionRow, error) {
	f.created = append(f.created, arg)
	return db.CreateSessionRow{ID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, UserID: arg.UserID}, nil
}

func existingUser(t *testing.T, password string) *fakeLoginStore {
	t.Helper()
	hash, err := HashPassword(password, testParams)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeLoginStore{found: true, user: db.GetUserByEmailRow{
		ID:           pgtype.UUID{Bytes: [16]byte{9}, Valid: true},
		Email:        "ada@example.com",
		PasswordHash: hash,
		FullName:     "Ada Obi",
		Role:         db.UserRoleCustomer,
	}}
}

func TestLoginSuccess(t *testing.T) {
	users, sessions := existingUser(t, "a-long-enough-password"), &fakeSessions{}
	svc := newSvc(users, sessions)

	res, err := svc.Login(context.Background(), LoginInput{Email: "ada@example.com", Password: "a-long-enough-password"})
	if err != nil {
		t.Fatal(err)
	}

	claims, err := svc.tokens.Verify(res.AccessToken)
	if err != nil || claims.Role != "customer" {
		t.Fatalf("bad access token: claims=%+v err=%v", claims, err)
	}

	if len(sessions.created) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions.created))
	}
	stored := sessions.created[0].RefreshTokenHash
	if stored == res.RefreshToken || stored != HashRefreshToken(res.RefreshToken) {
		t.Fatal("only the hash of the refresh token may be stored")
	}
}

func TestLoginWrongPassword(t *testing.T) {
	sessions := &fakeSessions{}
	svc := newSvc(existingUser(t, "a-long-enough-password"), sessions)

	_, err := svc.Login(context.Background(), LoginInput{Email: "ada@example.com", Password: "wrong-password-123"})

	if !errors.Is(err, ErrInvalidCredentials) || len(sessions.created) != 0 {
		t.Fatalf("want ErrInvalidCredentials and no session, got err=%v sessions=%d", err, len(sessions.created))
	}
}

func TestLoginUnknownEmail(t *testing.T) {
	sessions := &fakeSessions{}
	svc := newSvc(&fakeLoginStore{found: false}, sessions)

	_, err := svc.Login(context.Background(), LoginInput{Email: "ghost@example.com", Password: "a-long-enough-password"})

	if !errors.Is(err, ErrInvalidCredentials) || len(sessions.created) != 0 {
		t.Fatalf("want ErrInvalidCredentials and no session, got err=%v sessions=%d", err, len(sessions.created))
	}
}

func TestLoginRespectsHashConcurrencyCap(t *testing.T) {
	svc := newSvc(&fakeLoginStore{found: false}, &fakeSessions{})
	for i := 0; i < cap(svc.sem); i++ {
		svc.sem <- struct{}{} // all hash slots busy
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := svc.Login(ctx, LoginInput{Email: "x@example.com", Password: "a-long-enough-password"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the call to give up while waiting for a slot, got %v", err)
	}
}

func loginRequestTo(router http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestLoginHandler(t *testing.T) {
	newRouter := func(users UserStore) http.Handler {
		h := NewHandler(newSvc(users, &fakeSessions{}), slog.New(slog.NewTextHandler(io.Discard, nil)))
		r := chi.NewRouter()
		h.Routes(r)
		return r
	}

	t.Run("success", func(t *testing.T) {
		rec := loginRequestTo(newRouter(existingUser(t, "a-long-enough-password")),
			`{"email":"ada@example.com","password":"a-long-enough-password"}`)

		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
		}
		for _, want := range []string{"access_token", "refresh_token", "Bearer"} {
			if !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("missing %q in %s", want, rec.Body)
			}
		}
		if strings.Contains(rec.Body.String(), "password_hash") {
			t.Fatal("response must not contain the password hash")
		}
	})

	t.Run("wrong password and unknown email look identical", func(t *testing.T) {
		wrongPw := loginRequestTo(newRouter(existingUser(t, "a-long-enough-password")),
			`{"email":"ada@example.com","password":"not-the-password"}`)
		unknown := loginRequestTo(newRouter(&fakeLoginStore{}),
			`{"email":"ghost@example.com","password":"not-the-password"}`)

		if wrongPw.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized {
			t.Fatalf("want 401/401, got %d/%d", wrongPw.Code, unknown.Code)
		}
		if wrongPw.Body.String() != unknown.Body.String() {
			t.Fatal("the two failure responses must be byte-identical")
		}
	})
}