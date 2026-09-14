// Package probe is the TLS monitoring prober (TECHNICAL_SPEC §6):
// InsecureSkipVerify handshake + VerifyConnection chain capture, manual
// chain build, x509.Verify with configured roots, VerifyHostname — every
// failure becomes a Finding from the closed code set, never an abort.
// verify: strict flips the probe to failing mode for compliance checks.
//
// Phase 3 (ROADMAP.md). This file pins the public contract.
package tlsprobe

import (
	"context"

	tlsmodel "github.com/rift/rift/internal/tlsmon/model"
)

// Prober probes configured targets through netx.Guard (SSRF deny-set at
// the dialer, AD-10).
type Prober struct {
	// Phase 3: targets, root pool, findings evaluation.
}

// New builds a prober.
func New() *Prober {
	// Phase 3 (ROADMAP.md).
	return &Prober{}
}

// Probe performs one live TLS inspection of target and returns the Report.
func (p *Prober) Probe(ctx context.Context, target string) (*tlsmodel.Report, error) {
	// Phase 3 (ROADMAP.md).
	_, _ = p, target
	return nil, nil
}
