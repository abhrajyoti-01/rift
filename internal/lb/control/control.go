package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lbmodel "github.com/abhrajyoti-01/rift/internal/lb/model"
	"github.com/abhrajyoti-01/rift/internal/platform/config"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// Server owns the snapshot store and the admin API.
type Server struct {
	configPath string

	// snapshot is the single config read path for the whole data plane.
	snapshot atomic.Pointer[lbmodel.Snapshot]

	mu      sync.Mutex // serializes reloads
	version uint64
	// removeQueued tracks backend addrs that were removed by a reload and
	// are draining: established connections keep serving until idle grace.
	draining map[string]time.Time

	// adminToken is required for the mutating admin endpoints when the
	// admin plane is bound to a routable address. Empty means token auth is
	// not configured, which is only acceptable on loopback.
	adminToken string
}

// SetAdminToken configures the bearer token required for reload when the
// admin plane is reachable beyond loopback.
func (s *Server) SetAdminToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adminToken = token
}

// New builds the control server over the config source path.
func New(configPath string) *Server {
	return &Server{
		configPath: configPath,
		draining:   make(map[string]time.Time),
	}
}

// Load initializes the snapshot from the config file. Call before serving;
// a config that fails validation refuses to start (exit 2) rather than
// starting degraded.
func (s *Server) Load() error {
	schema, _, err := config.Load(s.configPath)
	if err != nil {
		return err
	}
	snap, err := BuildSnapshot(schema)
	if err != nil {
		return err
	}
	s.snapshot.Store(snap)
	s.version = snap.Version
	return nil
}

// BuildSnapshot converts a validated config schema into the immutable
// data-plane snapshot. Pool picker kinds pass through; backends get live
// atomic health/conn state.
func BuildSnapshot(schema *config.Schema) (*lbmodel.Snapshot, error) {
	snap := &lbmodel.Snapshot{
		Version:   1,
		Listeners: nil,
		Pools:     make(map[lbmodel.PoolID]*lbmodel.Pool),
	}
	for i := range schema.LB.Pools {
		cp := &schema.LB.Pools[i]
		pool := &lbmodel.Pool{ID: lbmodel.PoolID(cp.ID), Picker: cp.Picker}
		for _, b := range cp.Backends {
			pool.Backends = append(pool.Backends, &lbmodel.Backend{
				ID:     b.ID,
				Addr:   b.Addr,
				Weight: b.Weight,
			})
		}
		snap.Pools[pool.ID] = pool
	}
	for i := range schema.LB.Listeners {
		cl := &schema.LB.Listeners[i]
		var tls *lbmodel.TLSConfig
		if cl.TLS != nil {
			tls = &lbmodel.TLSConfig{
				CertFile:   cl.TLS.CertFile,
				KeyFile:    cl.TLS.KeyFile,
				MinVersion: cl.TLS.MinVersion,
			}
		}
		snap.Listeners = append(snap.Listeners, lbmodel.ListenerSpec{
			ID:       lbmodel.ListenerID(cl.ID),
			Proto:    cl.Proto,
			Bind:     cl.Bind,
			Pool:     lbmodel.PoolID(cl.Pool),
			MaxConns: cl.MaxConns,
			TLS:      tls,
		})
	}
	return snap, nil
}

// Snapshot returns the current immutable snapshot — the data plane's only
// config read path.
func (s *Server) Snapshot() *lbmodel.Snapshot { return s.snapshot.Load() }

// Reload validates the config file, builds a new Snapshot, swaps
// atomically, and records removed backends for draining. Reloads are
// serialized; an in-flight reload yields 409 via the admin handler.
func (s *Server) Reload(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	schema, _, err := config.Load(s.configPath)
	if err != nil {
		return err
	}
	next, err := BuildSnapshot(schema)
	if err != nil {
		return err
	}

	old := s.snapshot.Load()
	if old != nil {
		next.Version = old.Version + 1
		s.markRemoved(old, next)
	}
	s.snapshot.Store(next)
	s.version = next.Version
	return nil
}

// markRemoved records backends that existed before and are gone now, so
// established connections can drain instead of being RST.
func (s *Server) markRemoved(old, next *lbmodel.Snapshot) {
	nextSet := map[string]bool{}
	for _, pool := range next.Pools {
		for _, b := range pool.Backends {
			nextSet[pool.ID.String()+"|"+b.Addr] = true
		}
	}
	for _, pool := range old.Pools {
		for _, b := range pool.Backends {
			key := pool.ID.String() + "|" + b.Addr
			if !nextSet[key] {
				s.draining[key] = time.Now().Add(60 * time.Second)
			}
		}
	}
}

// Draining returns backend addrs currently draining (for observability).
func (s *Server) Draining() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.draining))
	for k := range s.draining {
		out = append(out, k)
	}
	return out
}

// Version returns the current snapshot version.
func (s *Server) Version() uint64 { return atomic.LoadUint64(&s.version) }

// Handler is the admin-plane HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/snapshot", s.handleSnapshot)
	mux.HandleFunc("/v1/pools", s.handlePools)
	mux.HandleFunc("/v1/admin/reload", s.handleReload)
	return mux
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap := s.Snapshot()
	if snap == nil {
		http.Error(w, `{"error":"no snapshot"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}

func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	snap := s.Snapshot()
	if snap == nil {
		http.Error(w, `{"error":"no snapshot"}`, http.StatusServiceUnavailable)
		return
	}
	out := make([]map[string]any, 0, len(snap.Pools))
	for id, pool := range snap.Pools {
		be := make([]map[string]any, 0, len(pool.Backends))
		for _, b := range pool.Backends {
			be = append(be, map[string]any{
				"id":     b.ID,
				"addr":   b.Addr,
				"weight": b.Weight,
				"health": b.Health.Load(),
				"conns":  b.Conns.Load(),
			})
		}
		out = append(out, map[string]any{"id": id, "picker": pool.Picker, "backends": be})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"pools": out})
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// A reload reconfigures the data plane: it must not be reachable by an
	// unauthenticated caller whenever the admin plane extends beyond
	// loopback. Checked here (not only at bind time) so the guard travels
	// with the endpoint.
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="rift"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := s.Reload(r.Context()); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"error":   err.Error(),
			"outcome": "fail",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"outcome": "ok",
		"version": s.Version(),
	})
}

// authorized reports whether the request may mutate configuration. When no
// token is configured the caller must be a loopback peer — an unauthenticated
// remote reload is a configuration-takeover primitive.
func (s *Server) authorized(r *http.Request) bool {
	s.mu.Lock()
	token := s.adminToken
	s.mu.Unlock()

	if token == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return false
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}

	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// BindAdmin serves the admin handler on addr (loopback by default; the
// config validation refuses routable binds without allow_remote).
func (s *Server) BindAdmin(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errs.Wrap(err, errs.ClassResource, "lb.control", "bind admin "+addr)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(ln)
	return nil
}
