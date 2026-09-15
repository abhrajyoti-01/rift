package l7

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lbmodel "github.com/abhrajyoti-01/rift/internal/lb/model"
	"github.com/abhrajyoti-01/rift/internal/lb/picker"
)

// Config derives the Transport and forwarding policy from pool config.
type Config struct {
	PoolID                lbmodel.PoolID
	MaxIdleConnsPerHost   int // default 32 per backend
	MaxIdleConns          int // global
	IdleConnTimeout       time.Duration
	ExpectContinueTimeout time.Duration
	MaxBodyBytes          int64    // early 413; default 64 MiB
	MaxAttempts           int      // retries (not attempts) for the closed table
	Forwarded             string   // "" | "rfc7239"
	TrustedProxies        []string // empty = trust nobody for attribution
}

const (
	defaultMaxIdlePerHost   = 32
	defaultIdleConnTimeout  = 90 * time.Second
	defaultExpectContinue   = 1 * time.Second
	defaultMaxBodyBytes     = 64 << 20
	defaultMaxAttempts      = 2
	bufferPoolSize          = 16 << 10
)

func (c Config) withDefaults() Config {
	if c.MaxIdleConnsPerHost <= 0 {
		c.MaxIdleConnsPerHost = defaultMaxIdlePerHost
	}
	if c.IdleConnTimeout <= 0 {
		c.IdleConnTimeout = defaultIdleConnTimeout
	}
	if c.ExpectContinueTimeout <= 0 {
		c.ExpectContinueTimeout = defaultExpectContinue
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = defaultMaxBodyBytes
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxAttempts
	}
	return c
}

// Metrics is the L7 accounting surface.
type Metrics struct {
	Requests       atomic.Uint64
	Responses2xx   atomic.Uint64
	Responses4xx   atomic.Uint64
	Responses5xx   atomic.Uint64
	Rejected       atomic.Uint64 // 413/431/etc. by us
	Retries        atomic.Uint64
	RetryRefused   atomic.Uint64 // post-write non-idempotent refusals
	UpstreamErrors atomic.Uint64
}

// Proxy is the L7 data plane.
type Proxy struct {
	cfg     Config
	snap    func() *lbmodel.Snapshot
	rp      *httputil.ReverseProxy
	bufPool sync.Pool
	metrics Metrics
	mu      sync.Mutex
	pickers map[lbmodel.PoolID]picker.Picker
}

// New builds the proxy over the pool snapshot source.
func New(cfg Config, snap func() *lbmodel.Snapshot) *Proxy {
	cfg = cfg.withDefaults()
	p := &Proxy{cfg: cfg, snap: snap, pickers: map[lbmodel.PoolID]picker.Picker{}}
	p.bufPool.New = func() any { return make([]byte, bufferPoolSize) }

	p.rp = &httputil.ReverseProxy{
		Director:      p.director,
		Transport:     p.transport(),
		BufferPool:    p.bufferPool(),
		ModifyResponse: p.modifyResponse,
		ErrorHandler:  p.errorHandler,
		FlushInterval: -1, // stream immediately: this is a proxy, not a buffer
	}
	return p
}

// Metrics returns the accounting surface.
func (p *Proxy) Metrics() *Metrics { return &p.metrics }

// Handler returns the http.Handler to mount on the data-plane listener.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.metrics.Requests.Add(1)
		// Cap the request body before any upstream work happens.
		if p.cfg.MaxBodyBytes > 0 && r.ContentLength > p.cfg.MaxBodyBytes {
			p.metrics.Rejected.Add(1)
			http.Error(w, "request entity too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Request smuggling guard: carrying both Content-Length and
		// Transfer-Encoding at our boundary is ambiguous framing. net/http
		// rejects most of it, but a proxy must not forward the ambiguity.
		if r.Header.Get("Transfer-Encoding") != "" && r.Header.Get("Content-Length") != "" {
			p.metrics.Rejected.Add(1)
			http.Error(w, "ambiguous framing", http.StatusBadRequest)
			return
		}
		p.appendForwardedFor(r)
		p.rp.ServeHTTP(w, r)
	})
}

// director selects the upstream for the request. The upstream comes ONLY
// from the configured pool: an absolute-form request-target or a Host
// header never selects a destination (open-proxy boundary).
func (p *Proxy) director(r *http.Request) {
	b := p.pick()
	if b == nil {
		return
	}
	u, err := url.Parse("http://" + b.Addr)
	if err != nil {
		return
	}
	r.URL.Scheme = u.Scheme
	r.URL.Host = u.Host
	// Preserve the client's Host for the backend unless overridden.
	if r.Host == "" {
		r.Host = u.Host
	}
	r.Header.Set("X-Forwarded-Proto", schemeOf(r))
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// appendForwardedFor appends the peer to X-Forwarded-For. The inbound value
// is never trusted for attribution unless the peer is a trusted proxy.
func (p *Proxy) appendForwardedFor(r *http.Request) {
	peer := peerIP(r.RemoteAddr)
	prior := ""
	if p.peerTrusted(peer) {
		prior = r.Header.Get("X-Forwarded-For")
	}
	value := peer
	if prior != "" {
		value = prior + ", " + peer
	}
	r.Header.Set("X-Forwarded-For", value)
	if p.cfg.Forwarded == "rfc7239" {
		proto := schemeOf(r)
		forwarded := "for=" + quoteIfNeeded(peer) + ";proto=" + proto
		if existing := r.Header.Get("Forwarded"); existing != "" {
			forwarded = existing + ", " + forwarded
		}
		r.Header.Set("Forwarded", forwarded)
	}
}

func quoteIfNeeded(v string) string {
	// RFC 7239 node identifiers with ':' (IPv6) must be quoted.
	if strings.Contains(v, ":") {
		return `"` + v + `"`
	}
	return v
}

func (p *Proxy) pick() *lbmodel.Backend {
	pk := p.picker()
	if pk == nil {
		return nil
	}
	b, err := pk.Pick(context.Background(), picker.PickHint{})
	if err != nil {
		return nil
	}
	return b
}

func (p *Proxy) picker() picker.Picker {
	snap := p.snapshot()
	if snap == nil {
		return nil
	}
	var pool *lbmodel.Pool
	if p.cfg.PoolID != "" {
		pool = snap.Pools[p.cfg.PoolID]
	} else if len(snap.Pools) == 1 {
		for _, pl := range snap.Pools {
			pool = pl
		}
	}
	if pool == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if pk, ok := p.pickers[pool.ID]; ok {
		return pk
	}
	kind := pool.Picker
	if kind == "" {
		kind = picker.RoundRobin
	}
	pk, err := picker.New(kind, pool)
	if err != nil {
		return nil
	}
	p.pickers[pool.ID] = pk
	return pk
}

func (p *Proxy) snapshot() *lbmodel.Snapshot {
	if p.snap == nil {
		return nil
	}
	return p.snap()
}

// transport builds the owned Transport: connection reuse is a first-class
// tunable here because idle-connection behaviour is where a proxy's tail
// latency lives.
func (p *Proxy) transport() http.RoundTripper {
	return &http.Transport{
		Proxy: nil, // never follow environment proxies: we ARE the proxy
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          max(1024, p.cfg.MaxIdleConns),
		MaxIdleConnsPerHost:   p.cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       p.cfg.IdleConnTimeout,
		ExpectContinueTimeout: p.cfg.ExpectContinueTimeout,
		// Backend cleartext h2c stays off: it is a smuggling amplifier and
		// must be an explicit opt-in with a logged warning.
		ForceAttemptHTTP2: false,
		TLSHandshakeTimeout: 5 * time.Second,
		DisableCompression:  true, // do not alter representations in transit
	}
}

func (p *Proxy) bufferPool() httputil.BufferPool { return (*proxyBufferPool)(p) }

// proxyBufferPool adapts the sync.Pool to httputil.BufferPool.
type proxyBufferPool Proxy

func (bp *proxyBufferPool) Get() []byte {
	p := (*Proxy)(bp)
	return p.bufPool.Get().([]byte)
}

func (bp *proxyBufferPool) Put(b []byte) {
	p := (*Proxy)(bp)
	if cap(b) < bufferPoolSize {
		return
	}
	p.bufPool.Put(b[:bufferPoolSize])
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	switch {
	case resp.StatusCode >= 500:
		p.metrics.Responses5xx.Add(1)
	case resp.StatusCode >= 400:
		p.metrics.Responses4xx.Add(1)
	case resp.StatusCode >= 200:
		p.metrics.Responses2xx.Add(1)
	}
	// Do not advertise the upstream's identity.
	resp.Header.Del("Server")
	return nil
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	p.metrics.UpstreamErrors.Add(1)
	var status int
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	default:
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
}

func peerIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// peerTrusted reports whether peer is inside a configured trusted-proxy
// prefix. With no configured prefixes nothing is trusted.
func (p *Proxy) peerTrusted(peer string) bool {
	if len(p.cfg.TrustedProxies) == 0 {
		return false
	}
	ip := net.ParseIP(peer)
	if ip == nil {
		return false
	}
	for _, cidr := range p.cfg.TrustedProxies {
		if _, netw, err := net.ParseCIDR(cidr); err == nil && netw.Contains(ip) {
			return true
		}
		if host := net.ParseIP(cidr); host != nil && host.Equal(ip) {
			return true
		}
	}
	return false
}

// RetryDecision encodes the closed method/phase table.
type RetryDecision uint8

const (
	// RetryAllowed permits another attempt.
	RetryAllowed RetryDecision = iota
	// RetryRefusedBudget means MaxAttempts is exhausted.
	RetryRefusedBudget
	// RetryRefusedNonIdempotent means the method/phase is unsafe to retry.
	RetryRefusedNonIdempotent
)

// Retryable decides whether a failed attempt may be retried, per the closed
// table in LOAD_BALANCER_SPEC §5.2.
//
// method: the HTTP method. postWrite: true once any request byte reached
// the backend socket (anchored on httptrace WroteRequest, not guessed).
// idempotentPutDelete: operator opt-in for PUT/DELETE post-write retries.
// attempts: retries already performed.
func Retryable(method string, postWrite bool, idempotentPutDelete bool, attempts, maxAttempts int) RetryDecision {
	if attempts >= maxAttempts {
		return RetryRefusedBudget
	}
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return RetryAllowed
	case http.MethodPost, http.MethodPatch:
		// Safe only before any byte reached the backend.
		if postWrite {
			return RetryRefusedNonIdempotent
		}
		return RetryAllowed
	case http.MethodPut, http.MethodDelete:
		if postWrite && !idempotentPutDelete {
			return RetryRefusedNonIdempotent
		}
		return RetryAllowed
	default:
		// Unknown verbs never retry: we cannot reason about their safety.
		return RetryRefusedNonIdempotent
	}
}

// IsIdempotent reports whether a method is safe to repeat without operator
// opt-in.
func IsIdempotent(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace,
		http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
