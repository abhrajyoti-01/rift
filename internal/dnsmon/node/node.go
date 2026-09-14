// Package node is the DNS monitoring node (TECHNICAL_SPEC §5.5):
// scheduler, fixed-capacity ring (drop-oldest counted, never silent),
// batch shipper over mTLS with retry/backoff, bounded offline spool. A
// monitor degrades by reporting gaps, not by lying (ARCHITECTURE §13).
//
// Phase 3 (ROADMAP.md). This file pins the public contract.
package dnsnode

import (
	"context"
	"time"

	"github.com/rift/rift/internal/dnsmon/resolver"
)

// NodeConfig configures one monitoring node.
type NodeConfig struct {
	NodeID    string
	Location string
	Resolvers []resolver.EngineConfig
	Targets   int // placeholder count; typed targets land in Phase 3
	Interval  time.Duration // per target, default 60s
	RingCap   int           // default 65536; hard memory ceiling
	ShipEvery time.Duration // batch age trigger, default 5s
	ShipSize  int           // batch size trigger, default 512
	HubURL    string
	SpoolMax  int64 // offline spool bytes; default 64 MiB; hard cap
}

// Node is the monitoring node process.
type Node struct {
	// Phase 3: scheduler, ring, shipper, spool.
}

// New builds a node from config.
func New(cfg NodeConfig) *Node {
	// Phase 3 (ROADMAP.md).
	_ = cfg
	return &Node{}
}

// Run schedules probes and ships observations until ctx is canceled.
func (n *Node) Run(ctx context.Context) error {
	// Phase 3 (ROADMAP.md).
	_, _ = n, ctx
	return nil
}
