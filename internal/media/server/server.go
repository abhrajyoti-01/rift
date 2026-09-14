// Package server is the media delivery server (TECHNICAL_SPEC §7.2):
// range handler, admission limits, per-stream accounting, index builder.
// Memory per stream is one readahead buffer (buffered mode) or socket
// buffers only (sendfile mode) — bounded by config, independent of file
// size (FR-36).
//
// Phase 4 (ROADMAP.md). This file pins the public contract.
package medserver

import (
	"context"
	"time"
)

// Config configures the media server.
type Config struct {
	Bind                string
	Root                string
	MaxStreams          int // global concurrent; default 64
	MaxStreamsPerClient int // default 4
	ReadHeaderTimeout, IdleTimeout time.Duration
	WriteTimeout time.Duration // per whole stream; seek resets
	IOMode       string         // "sendfile" (default) | "buffered"
	Readahead    int            // bytes, buffered mode; default 1 MiB
	RateLimitPerClient string  // e.g. "120/s"
}

// Server is the media data plane.
type Server struct {
	// Phase 4: handler, admission semaphores, per-stream accounting.
}

// New builds a server from config.
func New(cfg Config) *Server {
	// Phase 4 (ROADMAP.md).
	_ = cfg
	return &Server{}
}

// Run serves until ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	// Phase 4 (ROADMAP.md).
	_, _ = s, ctx
	return nil
}
