// Package logging builds the process's *slog.Logger: JSON to stdout in prod,
// text in dev; a MultiHandler fans to a second sink and degrades silently.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options configures the logger.
type Options struct {
	Level  string // debug|info|warn|error
	Format string // json|text
	// Extra is an optional second writer; writes are best-effort, a panicking
	// handler is recovered and dropped for that record. Nil disables it.
	Extra io.Writer
}

// New builds a *slog.Logger from Options.
func New(o Options) *slog.Logger {
	level := parseLevel(o.Level)
	stdoutW, extraW := io.Writer(os.Stdout), o.Extra
	// JSON handler for prod, text for dev; both share the level so the extra
	// sink sees the same filtering.
	jsonH := slog.NewJSONHandler(stdoutW, &slog.HandlerOptions{Level: level})
	var primary slog.Handler = jsonH
	if strings.EqualFold(o.Format, "text") {
		primary = slog.NewTextHandler(stdoutW, &slog.HandlerOptions{Level: level})
	}
	if extraW == nil {
		return slog.New(primary)
	}
	extraH := slog.NewJSONHandler(extraW, &slog.HandlerOptions{Level: level})
	return slog.New(&multiHandler{primary: primary, extra: extraH})
}

// multiHandler writes each record to a primary and an optional extra handler.
// The extra handler never propagates an error: logging is best-effort.
type multiHandler struct{ primary, extra slog.Handler }

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// Enable if EITHER handler would; the extra sink may want debug while prod
	// filters to info.
	return m.primary.Enabled(ctx, level) || m.extra.Enabled(ctx, level)
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	// Primary first; if it fails the process is in real trouble.
	if err := m.primary.Handle(ctx, r); err != nil {
		return err
	}
	// Extra is best-effort: recover from a panic (a bad custom handler) and
	// never let logging take the request down.
	defer func() { _ = recover() }()
	_ = m.extra.Handle(ctx, r)
	return nil
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &multiHandler{primary: m.primary.WithAttrs(attrs), extra: m.extra.WithAttrs(attrs)}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	return &multiHandler{primary: m.primary.WithGroup(name), extra: m.extra.WithGroup(name)}
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
