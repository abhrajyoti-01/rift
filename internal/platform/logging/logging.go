package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	traceIDKey
)

// Options configures handler construction: service name, level, format.
type Options struct {
	Service string
	Level   string
	Format  string
}

// New builds the root slog handler with structured logging support.
func New(opts Options) error {
	level := parseLevel(opts.Level)
	
	var handler slog.Handler
	if opts.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		})
	}
	
	slog.SetDefault(slog.New(handler))
	return nil
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// WithRequestID returns a context with the request ID attached.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// WithTraceID returns a context with the trace ID attached.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey, id)
}

// RequestID returns the per-connection/request id from ctx, or "".
func RequestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// TraceID returns the per-operation trace id from ctx, or "".
func TraceID(ctx context.Context) string {
	if id, ok := ctx.Value(traceIDKey).(string); ok {
		return id
	}
	return ""
}
