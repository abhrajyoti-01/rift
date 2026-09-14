// Package probe implements recursive-view and authoritative-view probing
// (TECHNICAL_SPEC §5.3). The recursive view queries configured public
// resolvers (RD=1); the authoritative view discovers the zone NS set and
// queries NS servers directly (RD=0), capped at max_ns_probe per zone for
// FD safety (UD-5). Client-facing view is documented-only in v1 — a stated
// limitation, not a gap to paper over.
//
// Phase 3 (ROADMAP.md). This file pins the public contract.
package probe

import (
	"context"

	dnsmodel "github.com/rift/rift/internal/dnsmon/model"
)

// Prober produces observations for its configured view.
type Prober struct {
	// Phase 3: engines, targets, schedule wiring.
}

// New builds a prober for the configured view and targets.
func New(view dnsmodel.View) *Prober {
	// Phase 3 (ROADMAP.md).
	_ = view
	return &Prober{}
}

// ProbeAll queries every configured target once and returns the
// observations. Failures become ErrClass-carrying observations, not drops
// — loss is counted, never silent.
func (p *Prober) ProbeAll(ctx context.Context) []dnsmodel.Observation {
	// Phase 3 (ROADMAP.md).
	_, _ = p, ctx
	return nil
}
