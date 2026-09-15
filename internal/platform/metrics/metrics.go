package metrics

import "net/http"

// Buckets are the fixed histogram bucket sets used by the application.
type Buckets struct {
	LatencyLAN    []float64 // .00005 .. 2.5 — LB request, pick, media first-byte, DNS query
	LatencyWAN    []float64 // .05 .. 10 — TLS handshake
	BytesTransfer []float64 // 4k .. 1G — media stream bytes
	Ratio         []float64 // .5 .. 1 — reuse ratio, readahead hit
	BatchSize     []float64 // 1 .. 4096 — hub ingest batch
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
