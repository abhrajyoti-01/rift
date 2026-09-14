// Package health implements the liveness/readiness state machine and check
// fan-in (OBSERVABILITY_SPEC §3.7). Liveness = process responsive on the
// admin plane. Readiness = AND of component checks (e.g. LB: config valid
// ∧ listeners bound ∧ ≥1 backend up). They differ on purpose: with zero
// backends the LB stays live — so you can read its config and metrics —
// but not ready — so it leaves the pool.
//
// Phase 1 (ROADMAP.md). This file pins the public contract.
package health

import "context"

// Status is one readiness check's outcome.
type Status struct {
	Name   string
	Ready  bool
	Detail string
}

// Registry collects readiness checks from components and reports the AND.
type Registry struct {
	// Phase 1: checks map + mutex (cold path).
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	// Phase 1 (ROADMAP.md).
	return &Registry{}
}

// Register adds a readiness check. Checks are consulted on /readyz; a
// failing check makes the service not-ready but never not-live.
func (r *Registry) Register(name string, check func(ctx context.Context) Status) {
	// Phase 1 (ROADMAP.md).
	_, _ = name, check
}

// Ready evaluates all checks, returning every Status so the 503 body names
// exactly which check failed.
func (r *Registry) Ready(ctx context.Context) []Status {
	// Phase 1 (ROADMAP.md).
	_ = ctx
	return nil
}
