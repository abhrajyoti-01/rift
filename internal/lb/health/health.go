package lbhealth

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"

	lbmodel "github.com/abhrajyoti-01/rift/internal/lb/model"
)

// Config sets the active-check schedule and thresholds. Zero fields use the
// defaults: 5s interval, 2s timeout, rise 2, and fall 3.
type Config struct {
	Interval time.Duration
	Timeout  time.Duration
	Rise     int
	Fall     int
	Kind     string // "tcp" | "http"
	// HTTP checks only:
	Method string
	Path   string
	Expect []int // e.g. {200, 204}; empty = {200}
}

const (
	defaultInterval = 5 * time.Second
	defaultTimeout  = 2 * time.Second
	defaultRise     = 2
	defaultFall     = 3
	maxBodyRead     = 1 << 10 // checks read at most 1 KiB of body
)

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.Rise <= 0 {
		c.Rise = defaultRise
	}
	if c.Fall <= 0 {
		c.Fall = defaultFall
	}
	if c.Kind == "" {
		c.Kind = "tcp"
	}
	return c
}

// CheckResult is one check outcome. Detail is for logs only — never a
// metric label (closed label set).
type CheckResult struct {
	Healthy bool
	Latency time.Duration
	Detail  string
}

// Checker performs one check against one backend.
type Checker interface {
	Check(ctx context.Context, b *lbmodel.Backend) CheckResult
}

// TCPChecker reports healthy when a TCP connection can be established.
type TCPChecker struct{ Timeout time.Duration }

// Check dials b.Addr and reports connect success within the timeout.
//
// The connection is closed immediately: the check answers "is a listener
// accepting here", not "is the application correct".
func (c TCPChecker) Check(ctx context.Context, b *lbmodel.Backend) CheckResult {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", b.Addr)
	lat := time.Since(start)
	if err != nil {
		return CheckResult{Healthy: false, Latency: lat, Detail: err.Error()}
	}
	conn.Close()
	return CheckResult{Healthy: true, Latency: lat}
}

// HTTPChecker GETs a path and expects a status within a set.
type HTTPChecker struct {
	Timeout                  time.Duration
	Method, Path, HostHeader string
	Expect                   []int
}

// Check performs the HTTP probe and matches the status set. A non-matching
// status is unhealthy; the body is read only to let the connection reuse
// cleanly and is capped at 1 KiB.
func (c HTTPChecker) Check(ctx context.Context, b *lbmodel.Backend) CheckResult {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	method := c.Method
	if method == "" {
		method = http.MethodGet
	}
	path := c.Path
	if path == "" {
		path = "/"
	}
	expect := c.Expect
	if len(expect) == 0 {
		expect = []int{http.StatusOK}
	}

	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: (&net.Dialer{Timeout: timeout}).DialContext,
		},
	}
	// The probe reads no body: closing early is correct and cheap.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	req, err := http.NewRequestWithContext(dctx, method, "http://"+b.Addr+path, nil)
	if err != nil {
		return CheckResult{Healthy: false, Detail: err.Error()}
	}
	if c.HostHeader != "" {
		req.Host = c.HostHeader
	}

	start := time.Now()
	resp, err := client.Do(req)
	lat := time.Since(start)
	if err != nil {
		return CheckResult{Healthy: false, Latency: lat, Detail: err.Error()}
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyRead))
	resp.Body.Close()

	for _, code := range expect {
		if resp.StatusCode == code {
			return CheckResult{Healthy: true, Latency: lat}
		}
	}
	return CheckResult{Healthy: false, Latency: lat,
		Detail: "unexpected status " + resp.Status}
}

// New builds the Checker from Config.
func New(cfg Config) Checker {
	cfg = cfg.withDefaults()
	switch cfg.Kind {
	case "http":
		path := cfg.Path
		if path == "" {
			path = "/"
		}
		method := cfg.Method
		if method == "" {
			method = http.MethodGet
		}
		expect := cfg.Expect
		if len(expect) == 0 {
			expect = []int{http.StatusOK}
		}
		return HTTPChecker{
			Timeout: cfg.Timeout,
			Method:  method,
			Path:    path,
			Expect:  expect,
		}
	default:
		return TCPChecker{Timeout: cfg.Timeout}
	}
}

// Tracker applies rise/fall hysteresis to a backend's advertised health.
// A backend flips healthy only after Rise consecutive passes and unhealthy
// only after Fall consecutive failures — flapping backends stay put.
type Tracker struct {
	rise, fall int

	passes   int
	failures int
	healthy  bool
}

// NewTracker builds a tracker with the configured thresholds.
func NewTracker(rise, fall int) *Tracker {
	if rise <= 0 {
		rise = defaultRise
	}
	if fall <= 0 {
		fall = defaultFall
	}
	return &Tracker{rise: rise, fall: fall}
}

// Observe feeds one check result and returns the (possibly changed) health
// state. The first successful check can transition to healthy after Rise
// passes; the initial state is unhealthy so a fresh backend must earn
// traffic.
func (t *Tracker) Observe(healthy bool) bool {
	if healthy {
		t.passes++
		t.failures = 0
		if !t.healthy && t.passes >= t.rise {
			t.healthy = true
		}
	} else {
		t.failures++
		t.passes = 0
		if t.healthy && t.failures >= t.fall {
			t.healthy = false
		}
	}
	return t.healthy
}

// Healthy reports the current hysteresis state.
func (t *Tracker) Healthy() bool { return t.healthy }
