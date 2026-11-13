package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

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

func validInput() RegisterInput {
	return RegisterInput{Email: "ada@example.com", Password: "a-long-enough-password", FullName: "Ada Obi"}
}

func TestRegisterSuccess(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store, testParams)

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

			_, err := NewService(store, testParams).Register(context.Background(), in)

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

	_, err := NewService(store, testParams).Register(context.Background(), validInput())
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}