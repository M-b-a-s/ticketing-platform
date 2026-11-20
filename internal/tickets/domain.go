// Package tickets holds ticket types: what can be bought for an event, at what
// price, how many, and when.
package tickets

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

const (
	CurrencyNGN        = "NGN"
	MaxNameLen         = 60
	MaxPriceKobo       = 1_000_000_000 // N10,000,000
	MaxQuantity        = 100_000
	DefaultMaxPerOrder = 10
	MaxPerOrderCap     = 20
)

type TicketTypeInput struct {
	Name          string
	PriceKobo     *int64 // nil means "not provided"; 0 is a valid (free) price
	Currency      string
	TotalQuantity int
	MaxPerOrder   int // 0 means the default
	SalesStart    time.Time
	SalesEnd      time.Time
}

func (in *TicketTypeInput) Normalize() {
	in.Name = strings.TrimSpace(in.Name)
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Currency == "" {
		in.Currency = CurrencyNGN
	}
	if in.MaxPerOrder == 0 {
		in.MaxPerOrder = DefaultMaxPerOrder
	}
}

func checkName(f map[string]string, name string) {
	if n := utf8.RuneCountInString(name); n == 0 || n > MaxNameLen {
		f["name"] = "is required and must be at most 60 characters"
	}
}

func priceRange() string { return fmt.Sprintf("must be between 0 and %d kobo", MaxPriceKobo) }

// Validate checks a new ticket type against the event it belongs to.
func (in TicketTypeInput) Validate(eventEnds, now time.Time) map[string]string {
	f := map[string]string{}

	checkName(f, in.Name)
	if in.PriceKobo == nil {
		f["price_kobo"] = "is required"
	} else if *in.PriceKobo < 0 || *in.PriceKobo > MaxPriceKobo {
		f["price_kobo"] = priceRange()
	}
	if in.Currency != CurrencyNGN {
		f["currency"] = "only NGN is supported"
	}
	if in.TotalQuantity < 1 || in.TotalQuantity > MaxQuantity {
		f["total_quantity"] = fmt.Sprintf("must be between 1 and %d", MaxQuantity)
	}
	if in.MaxPerOrder < 1 || in.MaxPerOrder > MaxPerOrderCap {
		f["max_per_order"] = fmt.Sprintf("must be between 1 and %d", MaxPerOrderCap)
	}

	if in.SalesStart.IsZero() {
		f["sales_start"] = "is required"
	}
	switch {
	case in.SalesEnd.IsZero():
		f["sales_end"] = "is required"
	case !in.SalesEnd.After(now):
		f["sales_end"] = "must be in the future"
	case in.SalesEnd.After(eventEnds):
		f["sales_end"] = "must not be after the event ends"
	case !in.SalesStart.IsZero() && !in.SalesEnd.After(in.SalesStart):
		f["sales_end"] = "must be after sales_start"
	}
	return f
}

// TicketTypePatch is a partial update: nil means "leave this field unchanged".
// Currency is deliberately not patchable.
type TicketTypePatch struct {
	Name          *string
	PriceKobo     *int64
	TotalQuantity *int
	MaxPerOrder   *int
	SalesStart    *time.Time
	SalesEnd      *time.Time
}

func (p TicketTypePatch) IsEmpty() bool {
	return p.Name == nil && p.PriceKobo == nil && p.TotalQuantity == nil &&
		p.MaxPerOrder == nil && p.SalesStart == nil && p.SalesEnd == nil
}

func (p *TicketTypePatch) Normalize() {
	if p.Name != nil {
		v := strings.TrimSpace(*p.Name)
		p.Name = &v
	}
}

// ValidatePatch checks only what the patch touches, against the ticket type as it
// stands. "Sold" includes reserved tickets: a reservation already took stock.
func ValidatePatch(current db.TicketType, p TicketTypePatch, eventEnds time.Time) map[string]string {
	f := map[string]string{}
	sold := int(current.TotalQuantity - current.AvailableQuantity)

	if p.Name != nil {
		checkName(f, *p.Name)
	}
	if p.PriceKobo != nil {
		switch {
		case *p.PriceKobo < 0 || *p.PriceKobo > MaxPriceKobo:
			f["price_kobo"] = priceRange()
		case sold > 0 && *p.PriceKobo != current.PriceKobo:
			f["price_kobo"] = "cannot change after tickets have been sold or reserved"
		}
	}
	if p.TotalQuantity != nil {
		switch {
		case *p.TotalQuantity < 0 || *p.TotalQuantity > MaxQuantity:
			f["total_quantity"] = fmt.Sprintf("must be between 0 and %d", MaxQuantity)
		case *p.TotalQuantity < sold:
			f["total_quantity"] = fmt.Sprintf("cannot be lower than the %d already sold or reserved", sold)
		}
	}
	if p.MaxPerOrder != nil && (*p.MaxPerOrder < 1 || *p.MaxPerOrder > MaxPerOrderCap) {
		f["max_per_order"] = fmt.Sprintf("must be between 1 and %d", MaxPerOrderCap)
	}

	start, end := current.SalesStart.Time, current.SalesEnd.Time
	if p.SalesStart != nil {
		start = *p.SalesStart
	}
	if p.SalesEnd != nil {
		end = *p.SalesEnd
	}
	if p.SalesStart != nil || p.SalesEnd != nil {
		field := "sales_end"
		if p.SalesEnd == nil { // only the start moved, so the start is what clashes
			field = "sales_start"
		}
		switch {
		case !end.After(start):
			f[field] = "sales must end after they start"
		case end.After(eventEnds):
			f[field] = "sales must end by the time the event ends"
		}
	}
	return f
}

// SalesStatus is what the public sees instead of raw stock counts.
func SalesStatus(t db.TicketType, now time.Time) string {
	switch {
	case !now.Before(t.SalesEnd.Time):
		return "ended"
	case t.AvailableQuantity <= 0:
		return "sold_out"
	case now.Before(t.SalesStart.Time):
		return "upcoming"
	default:
		return "on_sale"
	}
}