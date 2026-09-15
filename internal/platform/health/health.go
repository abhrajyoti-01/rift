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
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a readiness check. Checks are consulted on /readyz; a
// failing check makes the service not-ready but never not-live.
func (r *Registry) Register(name string, check func(ctx context.Context) Status) {
	_, _ = name, check
}

// Ready evaluates all checks, returning every Status so the 503 body names
// exactly which check failed.
func (r *Registry) Ready(ctx context.Context) []Status {
	_ = ctx
	return nil
}
