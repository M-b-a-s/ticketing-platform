// Package orders turns a buyer's wish into a reservation: stock is taken atomically,
// and a pending order holds it until it is paid for or expires.
package orders

import (
	"bytes"
	"fmt"
	"sort"
	"time"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/uuidx"
)

const (
	HoldDuration       = 10 * time.Minute
	MaxLineItems       = 5
	MaxTicketsPerOrder = 20
	MaxPendingPerEvent = 3
)

type Line struct {
	TicketTypeID pgtype.UUID
	Quantity     int
}

type CreateInput struct {
	UserID  pgtype.UUID // always the authenticated caller, never taken from the request body
	EventID pgtype.UUID
	Lines   []Line
}

// Order statuses must match the orders_status_valid CHECK constraint.
const (
	StatusPending   = "pending"
	StatusPaid      = "paid"
	StatusFulfilled = "fulfilled"
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"
	StatusRefunded  = "refunded"
)

// Item is one line of an order as the buyer sees it.
type Item struct {
	TicketTypeID  pgtype.UUID
	Name          string
	Quantity      int32
	UnitPriceKobo int64 // the price at the moment of reservation
}

// View is an order together with its lines.
type View struct {
	Order db.Order
	Items []Item
}

func (in CreateInput) Validate() map[string]string {
	f := map[string]string{}

	if len(in.Lines) == 0 || len(in.Lines) > MaxLineItems {
		f["items"] = fmt.Sprintf("must contain between 1 and %d ticket types", MaxLineItems)
	}

	seen := map[pgtype.UUID]bool{}
	total := 0
	for i, l := range in.Lines {
		key := fmt.Sprintf("items[%d]", i)
		switch {
		case !l.TicketTypeID.Valid:
			f[key+".ticket_type_id"] = "is required"
		case seen[l.TicketTypeID]:
			f[key+".ticket_type_id"] = "appears more than once; combine the quantities"
		}
		seen[l.TicketTypeID] = true

		if l.Quantity < 1 {
			f[key+".quantity"] = "must be at least 1"
		} else {
			total += l.Quantity
		}
	}
	if _, taken := f["items"]; !taken && total > MaxTicketsPerOrder {
		f["items"] = fmt.Sprintf("at most %d tickets per order", MaxTicketsPerOrder)
	}
	return f
}

// sortedLines returns a copy ordered by ticket type id. Every order reserves in the
// same global order, so two buyers wanting A+B and B+A can never deadlock.
func sortedLines(lines []Line) []Line {
	out := append([]Line(nil), lines...)
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i].TicketTypeID.Bytes[:], out[j].TicketTypeID.Bytes[:]) < 0
	})
	return out
}

const idempotencyKeyRule = "must be 8 to 64 characters: letters, digits, hyphen or underscore"

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func ValidIdempotencyKey(key string) bool { return idempotencyKeyPattern.MatchString(key) }

// RequestHash fingerprints WHAT is being bought: the event and the lines, in a fixed
// order, so the same items listed in a different order are still the same request.
// The buyer is not part of it, because keys are already scoped per buyer.
func RequestHash(in CreateInput) string {
	var b strings.Builder
	b.WriteString(uuidx.String(in.EventID))
	for _, l := range sortedLines(in.Lines) {
		fmt.Fprintf(&b, "|%s:%d", uuidx.String(l.TicketTypeID), l.Quantity)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}