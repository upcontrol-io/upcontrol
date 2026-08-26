// Package logging builds the process's *slog.Logger: JSON to stdout in prod,
// text in dev.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Options configures the logger.
type Options struct {
	Level  string // debug|info|warn|error
	Format string // json|text
}

// New builds a *slog.Logger from Options.
func New(o Options) *slog.Logger {
	level := parseLevel(o.Level)
	// JSON handler for prod, text for dev.
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	if strings.EqualFold(o.Format, "text") {
		h = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
