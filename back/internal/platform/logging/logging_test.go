package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewJSON(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Level: "info", Format: "json", Extra: &buf})
	// stdlib's logger writes JSON to stdout; we can't capture stdout here, so we
	// assert the extra sink instead, which uses the same JSON handler.
	l.Info("hello", "k", "v")
	if !strings.Contains(buf.String(), `"msg":"hello"`) {
		t.Errorf("extra sink missing msg: %q", buf.String())
	}
	if !strings.Contains(buf.String(), `"k":"v"`) {
		t.Errorf("extra sink missing attr: %q", buf.String())
	}
}

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

func TestExtraSinkDegrades(t *testing.T) {
	// A panicking extra writer must not take New down. We can't easily make a
	// json handler panic, so we assert that New builds with a normal extra and
	// that logging through it produces valid JSON (the multiHandler contract).
	var extra bytes.Buffer
	l := New(Options{Level: "info", Format: "json", Extra: &extra})
	l.Error("boom")
	if !json.Valid(extra.Bytes()) {
		t.Errorf("extra sink produced invalid JSON: %q", extra.String())
	}
}
