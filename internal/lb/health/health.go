// Package health holds active health checks (TECHNICAL_SPEC §4.3): one
// scheduler goroutine per pool on an injected ticker, checks on a bounded
// Executor, rise/fall thresholds applied before Backend.Health is written.
// All timing is injected (AR-8): rise/fall transitions are tested under
// testing/synctest, never with real sleeps.
//
// Phase 2 (ROADMAP.md). This file pins the public contract.
package lbhealth

import (
	"context"
	"time"

	lbmodel "github.com/rift/rift/internal/lb/model"
)

// Config sets the active-check schedule and thresholds. Zero fields adopt
// the documented defaults (5s interval, 2s timeout, rise 2, fall 3).
type Config struct {
	Interval time.Duration
	Timeout  time.Duration
	Rise     int
	Fall     int
	Kind     string // "tcp" | "http"
}

// CheckResult is one check outcome. Detail is for logs only — never a
// metric label (closed label set).
type CheckResult struct {
	Healthy bool
	Latency time.Duration
	Detail  string
}

// Checker performs one check against one backend.
type Checker interface {
	Check(ctx context.Context, b *lbmodel.Backend) CheckResult
}

// TCPChecker reports healthy on connect success.
type TCPChecker struct{ Timeout time.Duration }

// Check dials b.Addr and reports connect success within the timeout.
func (c TCPChecker) Check(ctx context.Context, b *lbmodel.Backend) CheckResult {
	// Phase 2 (ROADMAP.md).
	_, _ = ctx, b
	return CheckResult{}
}

// HTTPChecker GETs a path and expects a status within a set.
type HTTPChecker struct {
	Timeout    time.Duration
	Method, Path, HostHeader string
	Expect     []int // e.g. {200, 204}; empty = {200}
}

// Check performs the HTTP probe and matches the status set.
func (c HTTPChecker) Check(ctx context.Context, b *lbmodel.Backend) CheckResult {
	// Phase 2 (ROADMAP.md).
	_, _ = ctx, b
	return CheckResult{}
}

// New builds the Checker from Config.
func New(cfg Config) Checker {
	// Phase 2 (ROADMAP.md).
	_ = cfg
	return TCPChecker{}
}
