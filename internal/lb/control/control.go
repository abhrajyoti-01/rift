// Package control is the LB control plane (TECHNICAL_SPEC §4.8): admin API,
// config reload, atomic snapshot swap. SIGHUP and POST /v1/admin/reload
// hit the same validate-then-swap path; a config that fails validation
// reaches the data plane through neither. Reload never drops established
// connections (G8): pools swap atomically, removed backends drain by idle
// grace.
//
// Phase 2 (ROADMAP.md). This file pins the public contract.
package control

import (
	"context"
	"net/http"

	lbmodel "github.com/rift/rift/internal/lb/model"
)

// Server owns the snapshot store and the admin API.
type Server struct {
	// Phase 2: snapshot atomic.Pointer, reload serialization, admin mux.
}

// New builds the control server over the config source path.
func New(configPath string) *Server {
	// Phase 2 (ROADMAP.md).
	_ = configPath
	return &Server{}
}

// Snapshot returns the current immutable snapshot — the data plane's only
// config read path.
func (s *Server) Snapshot() *lbmodel.Snapshot {
	// Phase 2 (ROADMAP.md).
	_ = s
	return nil
}

// Reload validates the config file, builds a new Snapshot, swaps
// atomically, logs the diff, and drains removed backends. Reloads are
// serialized; an in-flight reload yields 409.
func (s *Server) Reload(ctx context.Context) error {
	// Phase 2 (ROADMAP.md).
	_, _ = s, ctx
	return nil
}

// Handler is the admin-plane HTTP handler (loopback bind enforced by
// httpx; routable bind without allow_remote is refuse-to-start).
func (s *Server) Handler() http.Handler {
	// Phase 2 (ROADMAP.md).
	_ = s
	return nil
}
