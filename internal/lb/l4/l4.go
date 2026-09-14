// Package l4 is the TCP forwarding proxy (TECHNICAL_SPEC §4.4). One
// goroutine per accepted connection — the work unit is the socket; a
// worker pool in front of a blocking Read strictly loses latency.
// Half-close handling is load-bearing: client EOF → CloseWrite upstream,
// keep draining, else large responses behind a request-body-fin truncate.
//
// Phase 2 (ROADMAP.md). This file pins the public contract.
package l4

import (
	"context"
	"net"
	"time"

	lbmodel "github.com/rift/rift/internal/lb/model"
)

// Config sets forwarding behavior. Splice selects the dst.ReadFrom(src)
// path when E1 confirms splice is reached on the reference host; the
// pooled 8KiB copy is the always-compiled fallback and the default until
// then (UD-2).
type Config struct {
	DialTimeout  time.Duration // default 3s
	IdleTimeout  time.Duration // default 60s, per-direction
	CopyDeadline time.Duration // default 30s, reset per Read/Write
	MaxConns     int           // per listener; admission gate
	MaxRetries   int           // dial-phase repicks, distinct backends
	Splice       bool
}

// Forwarder proxies one listener's connections to its pool.
type Forwarder struct {
	// Phase 2: snapshot source, admission counter, metrics.
}

// New builds a Forwarder. snap must read the atomic.Pointer[Snapshot] in
// lb/control — the data plane never takes a lock for a pick.
func New(cfg Config, snap func() *lbmodel.Snapshot) *Forwarder {
	// Phase 2 (ROADMAP.md).
	_, _ = cfg, snap
	return &Forwarder{}
}

// Serve accepts on l until ctx is canceled, one goroutine per connection.
func (f *Forwarder) Serve(ctx context.Context, l net.Listener) error {
	// Phase 2 (ROADMAP.md).
	_, _ = ctx, l
	return nil
}
