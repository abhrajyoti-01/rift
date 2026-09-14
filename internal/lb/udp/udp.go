// Package udp is the L4 UDP session proxy (TECHNICAL_SPEC §4.5). Backend
// affinity is per-session (AD-14); max_sessions is a hard admission gate —
// an unbounded session table is a memory bomb reachable by one sendto loop.
// UDP has no congestion signal: drop-with-counter is the only
// backpressure, and this package says so rather than pretending flow
// control exists.
//
// Phase 2 (ROADMAP.md). This file pins the public contract.
package udp

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	lbmodel "github.com/rift/rift/internal/lb/model"
)

// SessionID is the hex(srcaddr)|hex(dstaddr) 5-tuple-normalized session key.
type SessionID string

// Session is one client↔backend flow: one connected upstream socket, one
// reply goroutine, atomically maintained idle and accounting state.
type Session struct {
	ID       SessionID
	Upstream *net.UDPConn
	LastSeen atomic.Int64 // unix nanos
	UpPkts, UpBytes, DownPkts, DownBytes atomic.Uint64
}

// Config sets session-table behavior.
type Config struct {
	MaxSessions int           // hard admission gate; default 4096
	SessionTTL  time.Duration // default 120s
	SweepEvery  time.Duration // default 30s
	DialTimeout time.Duration
}

// Server is the UDP proxy: one read loop on the shared listener socket.
type Server struct {
	// Phase 2: session map, sweeper, picker.
}

// New builds a Server.
func New(cfg Config, snap func() *lbmodel.Snapshot) *Server {
	// Phase 2 (ROADMAP.md).
	_, _ = cfg, snap
	return &Server{}
}

// Serve reads packets from conn until ctx is canceled, dispatching by
// session table, creating per-session upstream sockets and reply
// goroutines.
func (s *Server) Serve(ctx context.Context, conn *net.UDPConn) error {
	// Phase 2 (ROADMAP.md).
	_, _ = ctx, conn
	return nil
}
