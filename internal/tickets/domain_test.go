package tickets

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
)

var (
	fixedNow  = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	eventEnds = fixedNow.Add(10 * 24 * time.Hour)
)

func ptr[T any](v T) *T { return &v }

func tstz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func validInput() TicketTypeInput {
	in := TicketTypeInput{
		Name: "General", PriceKobo: ptr(int64(500000)), TotalQuantity: 100,
		SalesStart: fixedNow, SalesEnd: fixedNow.Add(7 * 24 * time.Hour),
	}
	in.Normalize()
	return in
}

func existing(total, available int32) db.TicketType {
	return db.TicketType{
		Name: "General", PriceKobo: 500000, TotalQuantity: total, AvailableQuantity: available, MaxPerOrder: 10,
		SalesStart: tstz(fixedNow), SalesEnd: tstz(fixedNow.Add(7 * 24 * time.Hour)),
	}
}

func TestNormalizeAppliesDefaults(t *testing.T) {
	in := TicketTypeInput{Name: "  VIP  ", Currency: " ngn "}
	in.Normalize()

	if in.Name != "VIP" || in.Currency != "NGN" || in.MaxPerOrder != DefaultMaxPerOrder {
		t.Fatalf("unexpected normalization: %+v", in)
	}
}

func TestValidateAcceptsGoodInput(t *testing.T) {
	if f := validInput().Validate(eventEnds, fixedNow); len(f) != 0 {
		t.Fatalf("expected no errors, got %v", f)
	}
	free := validInput()
	free.PriceKobo = ptr(int64(0))
	if f := free.Validate(eventEnds, fixedNow); len(f) != 0 {
		t.Fatalf("a free ticket is valid, got %v", f)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		field  string
		mutate func(*TicketTypeInput)
	}{
		{"empty name", "name", func(in *TicketTypeInput) { in.Name = "" }},
		{"long name", "name", func(in *TicketTypeInput) { in.Name = strings.Repeat("a", MaxNameLen+1) }},
		{"missing price", "price_kobo", func(in *TicketTypeInput) { in.PriceKobo = nil }},
		{"negative price", "price_kobo", func(in *TicketTypeInput) { in.PriceKobo = ptr(int64(-1)) }},
		{"absurd price", "price_kobo", func(in *TicketTypeInput) { in.PriceKobo = ptr(int64(MaxPriceKobo + 1)) }},
		{"foreign currency", "currency", func(in *TicketTypeInput) { in.Currency = "USD" }},
		{"zero quantity", "total_quantity", func(in *TicketTypeInput) { in.TotalQuantity = 0 }},
		{"huge quantity", "total_quantity", func(in *TicketTypeInput) { in.TotalQuantity = MaxQuantity + 1 }},
		{"per-order cap", "max_per_order", func(in *TicketTypeInput) { in.MaxPerOrder = MaxPerOrderCap + 1 }},
		{"missing sales_start", "sales_start", func(in *TicketTypeInput) { in.SalesStart = time.Time{} }},
		{"missing sales_end", "sales_end", func(in *TicketTypeInput) { in.SalesEnd = time.Time{} }},
		{"sales already over", "sales_end", func(in *TicketTypeInput) { in.SalesEnd = fixedNow.Add(-time.Hour) }},
		{"sales outlast the event", "sales_end", func(in *TicketTypeInput) { in.SalesEnd = eventEnds.Add(time.Hour) }},
		{"sales end before start", "sales_end", func(in *TicketTypeInput) { in.SalesStart = in.SalesEnd.Add(time.Hour) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validInput()
			c.mutate(&in)
			if f := in.Validate(eventEnds, fixedNow); f[c.field] == "" {
				t.Fatalf("expected an error on %q, got %v", c.field, f)
			}
		})
	}
}

func TestValidatePatch(t *testing.T) {
	end := fixedNow.Add(7 * 24 * time.Hour)

	cases := []struct {
		name    string
		current db.TicketType
		patch   TicketTypePatch
		field   string // "" means the patch must be valid
	}{
		{"rename", existing(100, 100), TicketTypePatch{Name: ptr("VIP")}, ""},
		{"empty name", existing(100, 100), TicketTypePatch{Name: ptr("")}, "name"},
		{"price change while unsold", existing(100, 100), TicketTypePatch{PriceKobo: ptr(int64(600000))}, ""},
		{"price change after sales", existing(100, 70), TicketTypePatch{PriceKobo: ptr(int64(600000))}, "price_kobo"},
		{"same price after sales", existing(100, 70), TicketTypePatch{PriceKobo: ptr(int64(500000))}, ""},
		{"total below sold", existing(100, 70), TicketTypePatch{TotalQuantity: ptr(20)}, "total_quantity"},
		{"total equal to sold closes sales", existing(100, 70), TicketTypePatch{TotalQuantity: ptr(30)}, ""},
		{"raise the total", existing(100, 70), TicketTypePatch{TotalQuantity: ptr(150)}, ""},
		{"total over the cap", existing(100, 100), TicketTypePatch{TotalQuantity: ptr(MaxQuantity + 1)}, "total_quantity"},
		{"per-order zero", existing(100, 100), TicketTypePatch{MaxPerOrder: ptr(0)}, "max_per_order"},
		{"sales outlast the event", existing(100, 100), TicketTypePatch{SalesEnd: ptr(eventEnds.Add(time.Hour))}, "sales_end"},
		{"sales end before start", existing(100, 100), TicketTypePatch{SalesEnd: ptr(fixedNow.Add(-time.Hour))}, "sales_end"},
		{"close sales early", existing(100, 100), TicketTypePatch{SalesEnd: ptr(fixedNow.Add(time.Hour))}, ""},
		{"start moved past the end", existing(100, 100), TicketTypePatch{SalesStart: ptr(end.Add(time.Hour))}, "sales_start"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := ValidatePatch(c.current, c.patch, eventEnds)
			if c.field == "" && len(f) != 0 {
				t.Fatalf("expected a valid patch, got %v", f)
			}
			if c.field != "" && f[c.field] == "" {
				t.Fatalf("expected an error on %q, got %v", c.field, f)
			}
		})
	}
}

func TestSalesStatus(t *testing.T) {
	mk := func(available int32, start, end time.Time) db.TicketType {
		return db.TicketType{AvailableQuantity: available, SalesStart: tstz(start), SalesEnd: tstz(end)}
	}
	hour := time.Hour

	cases := []struct {
		name string
		tt   db.TicketType
		want string
	}{
		{"on sale", mk(5, fixedNow.Add(-hour), fixedNow.Add(hour)), "on_sale"},
		{"upcoming", mk(5, fixedNow.Add(hour), fixedNow.Add(2*hour)), "upcoming"},
		{"ended", mk(5, fixedNow.Add(-2*hour), fixedNow.Add(-hour)), "ended"},
		{"sold out", mk(0, fixedNow.Add(-hour), fixedNow.Add(hour)), "sold_out"},
		{"ended beats sold out", mk(0, fixedNow.Add(-2*hour), fixedNow.Add(-hour)), "ended"},
	}
	for _, c := range cases {
		if got := SalesStatus(c.tt, fixedNow); got != c.want {
			t.Errorf("%s: want %s, got %s", c.name, c.want, got)
		}
	}
}