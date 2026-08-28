package api

import (
	"testing"
	"time"
)

func TestParseLogWindow(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0}, // absent means the whole ring, which is what the window IS
		{"5m", 5 * time.Minute},
		{"1h", time.Hour},
		{"24h", 24 * time.Hour},
		{"3d", 0}, // not in the enum: ignored rather than guessed
		{"garbage", 0},
	}
	for _, c := range cases {
		if got := parseLogWindow(c.in); got != c.want {
			t.Fatalf("parseLogWindow(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
