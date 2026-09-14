// Package harness implements the bench subcommands (TECHNICAL_SPEC §8):
// run, compare (tolerance bands vs baselines), report (refuses without
// environment card), and the CI regression gate. Exit code 124 is the
// harness timeout, set here and never by errs.
//
// Phase 5 (ROADMAP.md). This file pins the public contract.
package harness

import "context"

// Run executes a named scenario from bench/scenarios/, emitting samples
// and a card stamp.
func Run(ctx context.Context, scenarioID string) error {
	// Phase 5 (ROADMAP.md).
	_, _ = ctx, scenarioID
	return nil
}

// Compare evaluates samples against bench/baselines/perf-baseline.json
// tolerance bands; out-of-band fails the build, improvement beyond band
// prompts re-baselining.
func Compare(ctx context.Context, against string) error {
	// Phase 5 (ROADMAP.md).
	_, _ = ctx, against
	return nil
}

// Report renders a benchmark report from a samples directory; it refuses to
// run without a fresh environment card (FR-46, T-98).
func Report(ctx context.Context, samplesDir string) error {
	// Phase 5 (ROADMAP.md).
	_, _ = ctx, samplesDir
	return nil
}
