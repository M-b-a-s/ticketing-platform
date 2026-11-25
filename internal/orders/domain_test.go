package orders

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func id(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }

func TestValidate(t *testing.T) {
	line := func(b byte, qty int) Line { return Line{TicketTypeID: id(b), Quantity: qty} }

	cases := []struct {
		name  string
		lines []Line
		field string // "" means valid
	}{
		{"one line", []Line{line(1, 2)}, ""},
		{"two lines", []Line{line(1, 2), line(2, 3)}, ""},
		{"no lines", nil, "items"},
		{"too many lines", []Line{line(1, 1), line(2, 1), line(3, 1), line(4, 1), line(5, 1), line(6, 1)}, "items"},
		{"zero quantity", []Line{line(1, 0)}, "items[0].quantity"},
		{"negative quantity", []Line{line(1, -2)}, "items[0].quantity"},
		{"duplicate ticket type", []Line{line(1, 1), line(1, 2)}, "items[1].ticket_type_id"},
		{"missing ticket type", []Line{{Quantity: 1}}, "items[0].ticket_type_id"},
		{"too many tickets", []Line{line(1, 12), line(2, 9)}, "items"},
		{"exactly the ticket limit", []Line{line(1, 10), line(2, 10)}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := CreateInput{Lines: c.lines}.Validate()
			if c.field == "" && len(f) != 0 {
				t.Fatalf("expected a valid order, got %v", f)
			}
			if c.field != "" && f[c.field] == "" {
				t.Fatalf("expected an error on %q, got %v", c.field, f)
			}
		})
	}
}

func TestSortedLinesIsStableAcrossInputOrders(t *testing.T) {
	in := []Line{{TicketTypeID: id(9), Quantity: 1}, {TicketTypeID: id(2), Quantity: 1}, {TicketTypeID: id(5), Quantity: 1}}

	got := sortedLines(in)

	if got[0].TicketTypeID != id(2) || got[1].TicketTypeID != id(5) || got[2].TicketTypeID != id(9) {
		t.Fatalf("lines must be ordered by ticket type id, got %v", got)
	}
	if in[0].TicketTypeID != id(9) {
		t.Fatal("the caller's slice must not be reordered")
	}
}