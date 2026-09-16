package health

import (
	"context"
	"sync"
)

// Status is one readiness check's outcome.
type Status struct {
	Name   string
	Ready  bool
	Detail string
}

// CheckFunc is a health check function.
type CheckFunc func(ctx context.Context) Status

// Registry collects readiness checks from components and reports the AND.
type Registry struct {
	mu     sync.RWMutex
	checks map[string]CheckFunc
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		checks: make(map[string]CheckFunc),
	}
}

// Register adds a readiness check. Checks are consulted on /readyz; a
// failing check makes the service not-ready but never not-live.
func (r *Registry) Register(name string, check CheckFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks[name] = check
}

// Ready evaluates all checks, returning every Status so the 503 body names
// exactly which check failed.
func (r *Registry) Ready(ctx context.Context) []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	
	statuses := make([]Status, 0, len(r.checks))
	for name, check := range r.checks {
		status := check(ctx)
		status.Name = name
		statuses = append(statuses, status)
	}
	return statuses
}

// IsReady returns true if all checks pass.
func (r *Registry) IsReady(ctx context.Context) bool {
	statuses := r.Ready(ctx)
	for _, s := range statuses {
		if !s.Ready {
			return false
		}
	}
	return true
}
