// Package httpx is the HTTP server factory with timeout sets, middleware,
// and TLS profiles (TECHNICAL_SPEC §4.6 posture). The CL+TE pre-forward
// reject and the closed-header hygiene rules live here as middleware —
// framing stays stdlib-owned (AD-3); this package adds the boundary checks
// the proxy boundary requires.
//
// Phase 1 (ROADMAP.md). This file pins the public contract.
package httpx

import (
	"net/http"
	"time"
)

// ServerOptions carries the mandatory timeout set; zero fields are rejected
// at construction — a server without timeouts is a slowloris invitation.
type ServerOptions struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}

// NewServer builds an http.Server with the timeout set and middleware
// applied. The admin plane binds loopback by default; routable binds without
// explicit allow_remote are a refuse-to-start condition (SECURITY_SPEC §3.4).
func NewServer(opts ServerOptions, handler http.Handler) *http.Server {
	// Phase 1 (ROADMAP.md).
	_ = opts
	_ = handler
	return nil
}

// Middleware applies boundary hygiene: CL+TE co-presence rejection
// (smuggling), header caps, request-id injection.
func Middleware(next http.Handler) http.Handler {
	// Phase 1 (ROADMAP.md).
	_ = next
	return nil
}
