package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Buckets are the fixed histogram bucket sets used by the application.
type Buckets struct {
	LatencyLAN    []float64
	LatencyWAN    []float64
	BytesTransfer []float64
	Ratio         []float64
	BatchSize     []float64
}

// DefaultBuckets returns production-ready histogram buckets.
func DefaultBuckets() *Buckets {
	return &Buckets{
		LatencyLAN:    prometheus.ExponentialBuckets(0.00005, 2, 16),
		LatencyWAN:    prometheus.ExponentialBuckets(0.001, 2, 16),
		BytesTransfer: prometheus.ExponentialBuckets(64, 4, 12),
		Ratio:         []float64{0.1, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99},
		BatchSize:     prometheus.ExponentialBuckets(1, 2, 12),
	}
}

// Registry is the per-process Prometheus registry with build info and
// process collectors registered unconditionally.
type Registry struct {
	reg *prometheus.Registry
}

// NewRegistry builds the registry, registering rift_build_info{version,
// commit, goversion}.
func NewRegistry(version, commit, goversion string) *Registry {
	reg := prometheus.NewRegistry()
	
	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rift_build_info",
			Help: "Build information for the RIFT binary",
		},
		[]string{"version", "commit", "goversion"},
	)
	buildInfo.WithLabelValues(version, commit, goversion).Set(1)
	reg.MustRegister(buildInfo)
	
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	reg.MustRegister(prometheus.NewGoCollector())
	
	return &Registry{reg: reg}
}

// Handler exposes the registry over HTTP for the admin plane only.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// Registry returns the underlying Prometheus registry for custom metric registration.
func (r *Registry) Prometheus() *prometheus.Registry {
	return r.reg
}
