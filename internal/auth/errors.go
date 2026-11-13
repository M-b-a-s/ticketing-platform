package auth

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

var ErrEmailTaken = errors.New("auth: email already registered")

const (
	// Postgres error code for unique violation.
	pgUniqueViolation = "23505"
	emailUniqueConstraint = "users_email_uniq"
)

func IsEmailTaken(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == emailUniqueConstraint
}