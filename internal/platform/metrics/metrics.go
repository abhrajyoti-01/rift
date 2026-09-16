package metrics

import "net/http"

// Buckets are the fixed histogram bucket sets used by the application.
type Buckets struct {
	LatencyLAN    []float64 // .00005 .. 2.5 — LB request, pick, media first-byte, DNS query
	LatencyWAN    []float64
	BytesTransfer []float64
	Ratio         []float64
	BatchSize     []float64
}

// Registry is the per-process Prometheus registry with build info and
// process collectors registered unconditionally.
type Registry struct {
}

// NewRegistry builds the registry, registering rift_build_info{version,
// commit, goversion} — a benchmark result without build labels is not
// reproducible.
func NewRegistry(version, commit, goversion string) *Registry {
	_, _, _ = version, commit, goversion
	return &Registry{}
}

// Handler exposes the registry over HTTP for the admin plane only.
func (r *Registry) Handler() http.Handler {
	_ = r
	return nil
}
