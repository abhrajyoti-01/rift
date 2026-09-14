// Package logging sets up slog JSON output with the closed field set
// (ts, level, msg, svc, comp, rid, tid, dur_ms plus per-site fields,
// OBSERVABILITY_SPEC §2). rid is per connection/request; tid is per logical
// operation, propagated node→hub via X-Rift-Tid (AD-13).
//
// Cardinality rule: no unbounded value in any log field. DNS names capped
// at 253 chars, no raw URLs from user input, no byte dumps — log fields
// that accept attacker input are a disk-exhaustion primitive.
//
// Phase 1 (ROADMAP.md). This file pins the public contract.
package logging

import "context"

// Options configures handler construction: service name, level, format.
type Options struct {
	Service string
	Level   string
}

// New builds the root slog handler. The closed field set is enforced by
// golden log-schema tests (OBSERVABILITY_SPEC §7), not by convention.
func New(opts Options) error {
	// Phase 1 (ROADMAP.md).
	_ = opts
	return nil
}

// RequestID returns the per-connection/request id from ctx, or "".
func RequestID(ctx context.Context) string {
	// Phase 1 (ROADMAP.md).
	_ = ctx
	return ""
}

// TraceID returns the per-operation trace id from ctx, or "".
func TraceID(ctx context.Context) string {
	// Phase 1 (ROADMAP.md).
	_ = ctx
	return ""
}
