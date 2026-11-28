package orders

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
)

func TestRequestHashIgnoresLineOrderButNotContent(t *testing.T) {
	line := func(b byte, qty int) Line { return Line{TicketTypeID: id(b), Quantity: qty} }
	base := CreateInput{EventID: event, Lines: []Line{line(1, 2), line(2, 1)}}

	reordered := CreateInput{EventID: event, Lines: []Line{line(2, 1), line(1, 2)}}
	if RequestHash(base) != RequestHash(reordered) {
		t.Fatal("the same items in a different order are the same request")
	}
	otherQty := CreateInput{EventID: event, Lines: []Line{line(1, 3), line(2, 1)}}
	otherType := CreateInput{EventID: event, Lines: []Line{line(1, 2), line(3, 1)}}
	otherEvent := CreateInput{EventID: id(99), Lines: base.Lines}
	for name, in := range map[string]CreateInput{"quantity": otherQty, "ticket type": otherType, "event": otherEvent} {
		if RequestHash(in) == RequestHash(base) {
			t.Fatalf("a different %s must change the hash", name)
		}
	}
}

func TestValidIdempotencyKey(t *testing.T) {
	for _, ok := range []string{"abcdefgh", "123e4567-e89b-12d3-a456-426614174000", "order_key-001", strings.Repeat("a", 64)} {
		if !ValidIdempotencyKey(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", "short", "has a space", "emoji-key-😀", strings.Repeat("a", 65)} {
		if ValidIdempotencyKey(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func oneLine() CreateInput {
	return CreateInput{UserID: buyer, EventID: event, Lines: []Line{{TicketTypeID: id(2), Quantity: 2}}}
}

func TestFirstRequestClaimsTheKeyAndRecordsTheOrder(t *testing.T) {
	tx := &fakeTx{reservable: map[pgtype.UUID]db.ReserveTicketTypeRow{id(2): rowAt(500000)}}

	view, replayed, err := NewService(&fakeStore{tx: tx}).CreateIdempotent(context.Background(), oneLine(), "order-key-12345")

	if err != nil || replayed || view.Order.TotalKobo != 1000000 {
		t.Fatalf("want a fresh order, got %v replayed=%v err=%v", view.Order.TotalKobo, replayed, err)
	}
	if len(tx.claimed) != 1 || tx.claimed[0].Scope != "create_order" || tx.claimed[0].Key != "order-key-12345" ||
		tx.claimed[0].UserID != buyer || tx.claimed[0].RequestHash != RequestHash(oneLine()) {
		t.Fatalf("the key must be claimed for this buyer and request: %+v", tx.claimed)
	}
	if len(tx.completed) != 1 || tx.completed[0].ResponseStatus != 201 ||
		!strings.Contains(string(tx.completed[0].ResponseBody), uuidx.String(id(100))) {
		t.Fatalf("the key must record the order it produced: %+v", tx.completed)
	}
}

func TestRepeatedRequestReturnsTheOriginalOrder(t *testing.T) {
	original := id(77)
	tx := &fakeTx{
		claimErr: pgx.ErrNoRows,
		prevKey: db.GetIdempotencyKeyRow{
			RequestHash:    RequestHash(oneLine()),
			ResponseStatus: pgtype.Int4{Int32: 201, Valid: true},
			ResponseBody:   []byte(fmt.Sprintf(`{"order_id":%q}`, uuidx.String(original))),
		},
	}
	store := &fakeStore{tx: tx, order: db.Order{ID: original, Status: StatusPending}}

	view, replayed, err := NewService(store).CreateIdempotent(context.Background(), oneLine(), "order-key-12345")

	if err != nil || !replayed || view.Order.ID != original {
		t.Fatalf("want the original order flagged as a replay, got %v replayed=%v err=%v", view.Order.ID, replayed, err)
	}
	if len(tx.reservedInOrder) != 0 || tx.orderParams != nil || len(tx.completed) != 0 {
		t.Fatal("a replay must take no stock and create nothing")
	}
}

func TestKeyReuseWithADifferentRequestIsRefused(t *testing.T) {
	tx := &fakeTx{
		claimErr: pgx.ErrNoRows,
		prevKey: db.GetIdempotencyKeyRow{
			RequestHash:    "a-different-request",
			ResponseStatus: pgtype.Int4{Int32: 201, Valid: true},
			ResponseBody:   []byte(`{"order_id":"x"}`),
		},
	}

	_, _, err := NewService(&fakeStore{tx: tx}).CreateIdempotent(context.Background(), oneLine(), "order-key-12345")

	if !errors.Is(err, ErrKeyReused) || len(tx.reservedInOrder) != 0 {
		t.Fatalf("want ErrKeyReused and no stock taken, got %v", err)
	}
}

func TestAKeyWhoseFirstRequestNeverFinishedIsReportedAsInProgress(t *testing.T) {
	tx := &fakeTx{claimErr: pgx.ErrNoRows, prevKey: db.GetIdempotencyKeyRow{RequestHash: RequestHash(oneLine())}} // ResponseStatus not set

	_, _, err := NewService(&fakeStore{tx: tx}).CreateIdempotent(context.Background(), oneLine(), "order-key-12345")

	if !errors.Is(err, ErrKeyInProgress) {
		t.Fatalf("want ErrKeyInProgress, got %v", err)
	}
}

func TestMalformedKeyNeverOpensATransaction(t *testing.T) {
	store := &fakeStore{tx: &fakeTx{}}

	_, _, err := NewService(store).CreateIdempotent(context.Background(), oneLine(), "short")

	var ve *ValidationError
	if !errors.As(err, &ve) || store.ran {
		t.Fatalf("want a validation error and no transaction, got %v ran=%v", err, store.ran)
	}
}

func TestAFailedOrderDoesNotCompleteTheKey(t *testing.T) {
	tx := &fakeTx{saleErr: pgx.ErrNoRows} // nothing is reservable, so the order fails

	_, _, err := NewService(&fakeStore{tx: tx}).CreateIdempotent(context.Background(), oneLine(), "order-key-12345")

	if err == nil || len(tx.completed) != 0 {
		t.Fatalf("a failed order must not record a result (the rollback also removes the claim), got %v completed=%d", err, len(tx.completed))
	}
}