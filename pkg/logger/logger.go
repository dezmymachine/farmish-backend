// Package logger builds the service's structured JSON logger and carries a
// request-scoped logger through context.Context.
package logger

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// New returns a JSON slog.Logger writing to w at the given level
// ("debug", "info", "warn", "error"; anything else means info).
func New(w io.Writer, level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: ParseLevel(level)}))
}

// ParseLevel maps a level name to a slog.Level, defaulting to info.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type ctxKey struct{}

// WithContext returns a copy of ctx carrying l.
func WithContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the logger stored in ctx, or slog.Default().
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
