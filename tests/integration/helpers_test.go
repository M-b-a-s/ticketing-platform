//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

var ctx = context.Background()

func uuidStr(u pgtype.UUID) string {
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func createUser(t *testing.T, q *db.Queries, email string) pgtype.UUID {
	t.Helper()
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: email, PasswordHash: "not-a-real-hash", FullName: "Test User"})
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u.ID
}

func setRole(t *testing.T, pool *pgxpool.Pool, user pgtype.UUID, role string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE users SET role = $1::user_role WHERE id = $2`, role, user); err != nil {
		t.Fatalf("set role %s: %v", role, err)
	}
}

// ADJUST: add any other NOT NULL columns without defaults that your events table has.
func insertEvent(t *testing.T, pool *pgxpool.Pool, organizer pgtype.UUID, title string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(ctx,
	`INSERT INTO events (organizer_id, title, venue_name, starts_at, ends_at)
	 VALUES ($1, $2, 'Test Venue', now() + interval '30 days', now() + interval '31 days')
	 RETURNING id`,
	organizer, title).Scan(&id)
	if err != nil {
		t.Fatalf("insert event %q (adjust insertEvent to your events columns): %v", title, err)
	}
	return id
}

func addStaff(pool *pgxpool.Pool, event, user pgtype.UUID, role string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO event_staff (event_id, user_id, role) VALUES ($1, $2, $3)`, event, user, role)
	return err
}

func mustAddStaff(t *testing.T, pool *pgxpool.Pool, event, user pgtype.UUID, role string) {
	t.Helper()
	if err := addStaff(pool, event, user, role); err != nil {
		t.Fatalf("add staff (%s): %v", role, err)
	}
}