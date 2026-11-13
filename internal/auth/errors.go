package auth

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

var ErrEmailTaken = errors.New("auth: email already registered")
var ErrInvalidCredentials = errors.New("auth: invalid email or password")

const (
	pgUniqueViolation = "23505"
	emailUniqueName   = "users_email_uniq"
)

func isEmailTaken(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgUniqueViolation &&
		pgErr.ConstraintName == emailUniqueName
}