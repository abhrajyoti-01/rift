// Package hub is the DNS aggregation hub (TECHNICAL_SPEC §5.6): mTLS
// ingest, sharded bounded memory window, append-only JSONL segment
// persistence, query API, alert evaluator. Single hub = documented SPOF
// (PRD §4 non-goal: no aggregator HA in v1).
//
// Phase 3 (ROADMAP.md). This file pins the public contract.
package hub

import (
	"context"
	"time"
)

// HubConfig configures the hub.
type HubConfig struct {
	IngestAddr  string // ":9001" TLS+mTLS
	QueryAddr   string // ":9002" TLS
	WindowKeys  int    // per (target,view); default 4096
	SegmentBytes int64 // JSONL rotation size; default 128 MiB
	DataDir     string
	MinResponding int
	ConvergenceWindow time.Duration
}

// Hub is the aggregation hub process.
type Hub struct {
	// Phase 3: sharded window map, segment writer, classifier, alerts.
}

// New builds a hub from config.
func New(cfg HubConfig) *Hub {
	// Phase 3 (ROADMAP.md).
	_ = cfg
	return &Hub{}
}

// Run serves ingest and query planes until ctx is canceled.
func (h *Hub) Run(ctx context.Context) error {
	// Phase 3 (ROADMAP.md).
	_, _ = h, ctx
	return nil
}
