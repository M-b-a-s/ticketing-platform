package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

type fakeStore struct {
	err    error
	called bool
	got    db.CreateUserParams
}

func (f *fakeStore) CreateUser(_ context.Context, arg db.CreateUserParams) (db.CreateUserRow, error) {
	f.called = true
	f.got = arg
	return db.CreateUserRow{Email: arg.Email, FullName: arg.FullName}, f.err
}

func (f *fakeStore) GetUserByEmail(context.Context, string) (db.GetUserByEmailRow, error) {
	return db.GetUserByEmailRow{}, pgx.ErrNoRows
}

func validInput() RegisterInput {
	return RegisterInput{Email: "ada@example.com", Password: "a-long-enough-password", FullName: "Ada Obi"}
}

func newSvc(users UserStore, sessions SessionStore) *Service {
	tm, err := NewTokenManager(testSecret, 15*time.Minute)
	if err != nil {
		panic(err)
	}
	svc, err := NewService(users, sessions, tm, testParams, Config{RefreshTTL: time.Hour, MaxConcurrentHashes: 2})
	if err != nil {
		panic(err)
	}
	return svc
}

func TestRegisterSuccess(t *testing.T) {
	store := &fakeStore{}
	svc := newSvc(store, nil)

	if _, err := svc.Register(context.Background(), validInput()); err != nil {
		t.Fatal(err)
	}
	if store.got.PasswordHash == "" || store.got.PasswordHash == "a-long-enough-password" {
		t.Fatal("password must be stored hashed")
	}
}

func TestRegisterValidation(t *testing.T) {
	cases := map[string]func(*RegisterInput){
		"email":     func(in *RegisterInput) { in.Email = "not-an-email" },
		"password":  func(in *RegisterInput) { in.Password = "short" },
		"full_name": func(in *RegisterInput) { in.FullName = "   " },
	}
	for field, mutate := range cases {
		t.Run(field, func(t *testing.T) {
			store := &fakeStore{}
			in := validInput()
			mutate(&in)

			_, err := newSvc(store, nil).Register(context.Background(), in)

			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Fields[field] == "" {
				t.Fatalf("expected validation error on %q, got %v", field, err)
			}
			if store.called {
				t.Fatal("store must not be called when validation fails")
			}
		})
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	store := &fakeStore{err: &pgconn.PgError{Code: "23505", ConstraintName: "users_email_uniq"}}

	_, err := newSvc(store, nil).Register(context.Background(), validInput())
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}