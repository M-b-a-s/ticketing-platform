// Package pagination holds the keyset-cursor plumbing shared by list endpoints.
package pagination

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

const (
	DefaultLimit = 20
	MaxLimit     = 100
)

var (
	ErrInvalidCursor = errors.New("pagination: invalid cursor")
	ErrInvalidLimit  = errors.New("pagination: invalid limit")
)

// Cursor is the sort position of the last item on a page: (time, id).
// It is opaque to clients. It isn't signed, and doesn't need to be: a forged
// cursor can only move the caller around inside results they may already see.
type Cursor struct {
	Time time.Time `json:"t"`
	ID   string    `json:"id"`
}

func Encode(c Cursor) string {
	b, _ := json.Marshal(Cursor{Time: c.Time.UTC(), ID: c.ID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func Decode(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Time.IsZero() || c.ID == "" {
		return Cursor{}, ErrInvalidCursor
	}
	return c, nil
}

// ParseLimit: empty means the default, values above the max are clamped,
// anything that isn't a positive integer is an error.
func ParseLimit(raw string) (int, error) {
	if raw == "" {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, ErrInvalidLimit
	}
	if n > MaxLimit {
		n = MaxLimit
	}
	return n, nil
}