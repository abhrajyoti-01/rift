// Package env produces the environment card (TECHNICAL_SPEC §8.1): host,
// kernel, cgroups, NIC, disk ceilings. `rift bench report` refuses to
// render a report without a card — a number without its environment is
// not a fact (FR-46, T-98).
//
// Phase 0/5 (ROADMAP.md). This file pins the public contract.
package benchenv

// Card is the machine-generated reproducibility record. Reports are
// regenerated from samples + card, never from memory (G6).
type Card struct {
	// Phase 0: OS/kernel, CPU, governor, GOMAXPROCS, cgroups, NIC, disk,
	// ulimit, THP, mitigations, WSL2 flags, measured ceilings.
}

// Probe collects the host environment into a Card.
func Probe() (*Card, error) {
	// Phase 0 (ROADMAP.md).
	return nil, nil
}
