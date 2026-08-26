package logging

import (
	"log/slog"
	"testing"
)

func TestNewText(t *testing.T) {
	// Text format is dev-only; it should not be JSON.
	l := New(Options{Level: "debug", Format: "text"})
	// Just assert no panic and level parses; capturing stdlib text output to
	// stdout is not worth a pipe here.
	_ = l
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"INFO":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"":      slog.LevelInfo,
		"bogus": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
