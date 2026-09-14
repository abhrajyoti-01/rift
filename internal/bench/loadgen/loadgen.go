// Package loadgen is the scenario runner and load generator
// (TECHNICAL_SPEC §8.2): closed + open loop, HDR summaries, raw NDJSON
// samples. Open loop schedules each request at its intended deadline
// regardless of completion — the coordinated-omission fix — and records
// both intended and actual start times.
//
// Phase 2/5 (ROADMAP.md). This file pins the public contract.
package loadgen

import (
	"context"
	"time"
)

// Scenario is one named, versioned load definition (stable IDs in
// bench/scenarios/).
type Scenario struct {
	Name       string // e.g. "lb.http.small.1k"
	Tool       string // "rift" | "hey" | "wrk2" | "nginx-baseline"
	Target     string
	Duration   time.Duration
	Warmup     time.Duration
	Loop       string // "closed" | "open"
	Conns      int
	RatePerSec float64
	Payload    int
	Method     string
	KeepAlive  float64
}

// Runner executes scenarios and emits raw samples.
type Runner struct {
	// Phase 2/5: worker model, HDR histograms, sample writer.
}

// NewRunner builds a runner for the scenario.
func NewRunner(s Scenario) *Runner {
	// Phase 2/5 (ROADMAP.md).
	_ = s
	return &Runner{}
}

// Run executes the scenario until ctx is canceled or the steady-state
// window ends, writing NDJSON samples (intended_start, actual_start,
// latency, status, bytes, conn_id).
func (r *Runner) Run(ctx context.Context) error {
	// Phase 2/5 (ROADMAP.md).
	_, _ = r, ctx
	return nil
}
