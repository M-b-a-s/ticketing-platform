package pagination

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	in := Cursor{Time: time.Date(2026, 12, 1, 18, 30, 0, 123456000, time.UTC), ID: "abc"}

	out, err := Decode(Encode(in))
	if err != nil || !out.Time.Equal(in.Time) || out.ID != in.ID {
		t.Fatalf("round trip failed: %+v %v", out, err)
	}
}

func TestDecodeRejectsBadCursors(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

	for name, raw := range map[string]string{
		"empty":        "",
		"not base64":   "!!!not-base64!!!",
		"not json":     enc("hello"),
		"missing id":   enc(`{"t":"2026-01-01T00:00:00Z"}`),
		"missing time": enc(`{"id":"x"}`),
	} {
		if _, err := Decode(raw); err != ErrInvalidCursor {
			t.Errorf("%s: want ErrInvalidCursor, got %v", name, err)
		}
	}
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", DefaultLimit, false},
		{"5", 5, false},
		{"100", 100, false},
		{"5000", MaxLimit, false},
		{"0", 0, true},
		{"-3", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := ParseLimit(c.raw)
		if (err != nil) != c.wantErr || (!c.wantErr && got != c.want) {
			t.Errorf("ParseLimit(%q) = %d, %v", c.raw, got, err)
		}
	}
}