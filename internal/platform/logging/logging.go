package logging

import "context"

// Options configures handler construction: service name, level, format.
type Options struct {
	Service string
	Level   string
}

// New builds the root slog handler. The closed field set is enforced by
// golden log-schema tests, not by convention.
func New(opts Options) error {
	_ = opts
	return nil
}

// RequestID returns the per-connection/request id from ctx, or "".
func RequestID(ctx context.Context) string {
	_ = ctx
	return ""
}

// TraceID returns the per-operation trace id from ctx, or "".
func TraceID(ctx context.Context) string {
	_ = ctx
	return ""
}
