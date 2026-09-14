// Package metrics owns the registry, bucket sets, and label-set
// enforcement (OBSERVABILITY_SPEC §3, TECHNICAL_SPEC §10). Metric names and
// closed label sets are declared as code constants — never from runtime
// data — and a unit test walks registered collectors to assert the sets.
//
// Phase 1 (ROADMAP.md). This file pins the public contract.
package metrics

import "net/http"

// Buckets are the fixed histogram bucket sets (OBSERVABILITY_SPEC §3.3),
// named so every histogram cites its set.
type Buckets struct {
	LatencyLAN  []float64 // .00005 .. 2.5 — LB request, pick, media first-byte, DNS query
	LatencyWAN  []float64 // .05 .. 10 — TLS handshake
	BytesTransfer []float64 // 4k .. 1G — media stream bytes
	Ratio       []float64 // .5 .. 1 — reuse ratio, readahead hit
	BatchSize   []float64 // 1 .. 4096 — hub ingest batch
}

// Registry is the per-process Prometheus registry with build info and
// process collectors registered unconditionally.
type Registry struct {
	// Phase 1: prometheus.Registry + rift_build_info.
}

// NewRegistry builds the registry, registering rift_build_info{version,
// commit, goversion} — a benchmark result without build labels is not
// reproducible.
func NewRegistry(version, commit, goversion string) *Registry {
	// Phase 1 (ROADMAP.md).
	_, _, _ = version, commit, goversion
	return &Registry{}
}

// Handler exposes the registry over HTTP for the admin plane only.
func (r *Registry) Handler() http.Handler {
	// Phase 1 (ROADMAP.md).
	_ = r
	return nil
}
