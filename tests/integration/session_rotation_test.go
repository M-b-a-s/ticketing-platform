//go:build integration

package integration

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/tests/integration/testdb"
)

func newSession(t *testing.T, q *db.Queries, user pgtype.UUID, hash string, expires time.Time) db.CreateSessionRow {
	t.Helper()
	s, err := q.CreateSession(ctx, db.CreateSessionParams{
		UserID:           user,
		RefreshTokenHash: hash,
		ExpiresAt:        pgtype.Timestamptz{Time: expires, Valid: true},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

func rotate(q *db.Queries, oldHash, newHash string) (db.RotateSessionRow, error) {
	return q.RotateSession(ctx, db.RotateSessionParams{OldHash: oldHash, NewHash: newHash})
}

// The claim behind refresh-token rotation: when many requests present the same
// token at once, exactly one wins and everyone else sees "no rows".
func TestRotateSessionOnlyOneConcurrentCallerWins(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	user := createUser(t, q, "racer@example.com")
	newSession(t, q, user, "hash-0", time.Now().Add(time.Hour))

	const racers = 20
	var wins atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := rotate(q, "hash-0", fmt.Sprintf("hash-new-%d", i))
			switch {
			case err == nil:
				wins.Add(1)
			case !errors.Is(err, pgx.ErrNoRows):
				t.Errorf("racer %d: unexpected error %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("exactly one rotation must win, got %d", got)
	}
}

func TestRotateSessionLifecycle(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	user := createUser(t, q, "u@example.com")
	setRole(t, pool, user, "organizer")
	sess := newSession(t, q, user, "hash-0", time.Now().Add(time.Hour))

	// rotation returns the user's CURRENT role from the database
	row, err := rotate(q, "hash-0", "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != sess.ID || row.UserID != user || string(row.Role) != "organizer" {
		t.Fatalf("unexpected rotation result: %+v", row)
	}

	// the old token is remembered as the previous one, and no longer rotates
	if _, err := rotate(q, "hash-0", "hash-2"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a used-up token must not rotate again, got %v", err)
	}
	prev, err := q.GetSessionByPreviousHash(ctx, "hash-0")
	if err != nil || prev.ID != sess.ID {
		t.Fatalf("reuse detection must find the session by its previous hash: %+v %v", prev, err)
	}

	// reuse response: revoke. After that nothing works, including the newest token.
	if err := q.RevokeSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rotate(q, "hash-1", "hash-3"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a revoked session must not rotate, got %v", err)
	}
	if _, err := q.GetSessionByPreviousHash(ctx, "hash-0"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a revoked session must not be reported for reuse again, got %v", err)
	}
}

func TestExpiredSessionCannotRotate(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	user := createUser(t, q, "u@example.com")
	newSession(t, q, user, "hash-old", time.Now().Add(-time.Minute))

	if _, err := rotate(q, "hash-old", "hash-new"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an expired session must not rotate, got %v", err)
	}
}

func TestLogoutRevokesOnlyThatSession(t *testing.T) {
	pool := testdb.New(t)
	q := db.New(pool)
	user := createUser(t, q, "u@example.com")
	newSession(t, q, user, "phone", time.Now().Add(time.Hour))
	newSession(t, q, user, "laptop", time.Now().Add(time.Hour))

	if err := q.RevokeSessionByRefreshHash(ctx, "phone"); err != nil {
		t.Fatal(err)
	}

	if _, err := rotate(q, "phone", "phone-2"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the logged-out session must be dead, got %v", err)
	}
	if _, err := rotate(q, "laptop", "laptop-2"); err != nil {
		t.Fatalf("the other session must be unaffected, got %v", err)
	}
}