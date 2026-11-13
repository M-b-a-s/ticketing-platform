package auth

import (
	"context"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

const (
	MinPasswordLen = 10
	MaxNameLen     = 100
)

// UserStore defines the interface for user storage operations.
type UserStore interface {
	CreateUser(ctx context.Context, arg db.CreateUserParams) (db.CreateUserRow, error)
}

// Service provides authentication-related operations.
type Service struct {
	users UserStore
	params Params
}

// NewService creates a new instance of the Service with the provided UserStore and Params.
func NewService(users UserStore, params Params) *Service {
	return &Service{users: users, params: params}
}

type RegisterInput struct {
	Email    string
	Password string
	FullName string
}

// Validate checks the validity of the RegisterInput fields and returns a ValidationError if any field is invalid.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string { return "auth: validation failed" }

// normalizeAndValidate trims whitespace from the input fields and validates them according to the defined rules.
func (in *RegisterInput) normalizeAndValidate() error {
	in.Email = strings.TrimSpace(in.Email)
	in.FullName = strings.TrimSpace(in.FullName)

	fields := map[string]string{}

	if addr, err := mail.ParseAddress(in.Email); err != nil || addr.Address != in.Email {
		fields["email"] = "must be a valid email address"
	}
	if n := utf8.RuneCountInString(in.Password); n < MinPasswordLen {
		fields["password"] = "must be at least 10 characters"
	} else if len(in.Password) > MaxPasswordLen {
		fields["password"] = "must be at most 128 bytes"
	}
	if in.FullName == "" || utf8.RuneCountInString(in.FullName) > MaxNameLen {
		fields["full_name"] = "is required and must be at most 100 characters"
	}

	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

// Register creates a new user in the database after validating the input and hashing the password. It returns the created user or an error if the operation fails.
func (s *Service) Register(ctx context.Context, in RegisterInput) (db.CreateUserRow, error) {
	if err := in.normalizeAndValidate(); err != nil {
		return db.CreateUserRow{}, err
	}

	hash, err := HashPassword(in.Password, s.params)
	if err != nil {
		return db.CreateUserRow{}, err
	}

	user, err := s.users.CreateUser(ctx, db.CreateUserParams{
		Email:        in.Email,
		PasswordHash: hash,
		FullName:     in.FullName,
	})
	if err != nil {
		if isEmailTaken(err) {
			return db.CreateUserRow{}, ErrEmailTaken
		}
		return db.CreateUserRow{}, err
	}
	return user, nil
}