// Package orders turns a buyer's wish into a reservation: stock is taken atomically,
// and a pending order holds it until it is paid for or expires.
package orders

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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