// Package resolver is the per-resolver query engine (TECHNICAL_SPEC §5.2):
// bounded concurrency per resolver, per-attempt timeouts, per-resolver
// circuit breaker. One slow authoritative server must not consume the
// node's whole outbound budget — the cap plus breaker is the amplification
// defense (SECURITY_SPEC §3.2).
//
// Phase 3 (ROADMAP.md). This file pins the public contract.
package resolver

import (
	"context"
	"net/netip"
	"time"

	"github.com/rift/rift/internal/dnsmon/wire"
)

// EngineConfig configures one engine bound to one upstream resolver.
type EngineConfig struct {
	Resolver    netip.AddrPort // e.g. 1.1.1.1:53
	Concurrency int            // per-resolver worker cap, default 8
	Timeout     time.Duration  // per attempt, default 2s
	EDNSUDPSize uint16         // default 1232
}

// Engine queries one upstream resolver.
type Engine struct {
	// Phase 3: pool.Executor, per-resolver stats atomics, circuit.Breaker.
}

// NewEngine builds an engine. now may be nil in production; tests inject
// a fake clock (AR-8).
func NewEngine(cfg EngineConfig, now func() time.Time) *Engine {
	// Phase 3 (ROADMAP.md).
	_, _ = cfg, now
	return &Engine{}
}

// Query returns the final message (post TCP fallback if TC was set), the
// measured resolver latency, and a classified error. It never retries
// internally — scheduling owns retry.
func (e *Engine) Query(ctx context.Context, q wire.Question) (wire.Message, time.Duration, error) {
	// Phase 3 (ROADMAP.md).
	_, _ = e, q
	return wire.Message{}, 0, nil
}
