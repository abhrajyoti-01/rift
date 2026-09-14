// Package l7 is the HTTP reverse proxy (TECHNICAL_SPEC §4.6): stdlib-owned
// framing via httputil.ReverseProxy with three owned seams — Transport,
// BufferPool, ModifyResponse/ErrorHandler. The retry gate is the closed
// method-keyed table (§4.7): unknown methods never retry; POST and PATCH
// never retry post-write — retrying a POST whose bytes reached the backend
// is a data-corruption bug, not an availability feature (AD-15).
//
// Phase 2 (ROADMAP.md). This file pins the public contract.
package l7

import (
	"net/http"
	"time"

	lbmodel "github.com/rift/rift/internal/lb/model"
)

// Config derives the Transport and forwarding policy from pool config.
type Config struct {
	MaxIdleConnsPerHost int           // default 32 per backend
	MaxIdleConns        int           // global
	IdleConnTimeout     time.Duration
	ExpectContinueTimeout time.Duration
	MaxBodyBytes        int64         // early 413; default 64 MiB
	MaxAttempts         int           // retries (not attempts) for the closed table
	Forwarded           string        // "" | "rfc7239"
	TrustedProxies      []string      // empty = trust nobody for attribution
}

// Proxy is the L7 data plane.
type Proxy struct {
	// Phase 2: ReverseProxy + owned Transport + BufferPool + hooks.
}

// New builds the proxy over the pool snapshot source.
func New(cfg Config, snap func() *lbmodel.Snapshot) *Proxy {
	// Phase 2 (ROADMAP.md).
	_, _ = cfg, snap
	return &Proxy{}
}

// Handler returns the http.Handler to mount on the data-plane listener.
func (p *Proxy) Handler() http.Handler {
	// Phase 2 (ROADMAP.md).
	_ = p
	return nil
}
