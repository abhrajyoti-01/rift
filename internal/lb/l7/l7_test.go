package l7

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	lbmodel "github.com/abhrajyoti-01/rift/internal/lb/model"
)

func snapshotHTTP(addrs ...string) *lbmodel.Snapshot {
	pool := &lbmodel.Pool{ID: "p", Picker: "round_robin"}
	for i, a := range addrs {
		b := &lbmodel.Backend{ID: fmt.Sprintf("b%d", i), Addr: a, Weight: 1}
		b.Health.Store(true)
		pool.Backends = append(pool.Backends, b)
	}
	return &lbmodel.Snapshot{
		Version: 1,
		Pools:   map[lbmodel.PoolID]*lbmodel.Pool{"p": pool},
	}
}

// httpBackend starts a real backend HTTP server.
func httpBackend(t *testing.T, h http.HandlerFunc) (string, func()) {
	ts := httptest.NewServer(h)
	host := strings.TrimPrefix(ts.URL, "http://")
	return host, ts.Close
}

func newProxy(t *testing.T, cfg Config, addrs ...string) *Proxy {
	t.Helper()
	cfg.PoolID = "p"
	snap := snapshotHTTP(addrs...)
	return New(cfg, func() *lbmodel.Snapshot { return snap })
}

func TestL7ForwardsRequestAndResponse(t *testing.T) {
	backend, stop := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "yes")
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	})
	defer stop()

	p := newProxy(t, Config{}, backend)
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/greet")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if string(body) != "hello /greet" {
		t.Errorf("body = %q, want %q", body, "hello /greet")
	}
	if resp.Header.Get("X-Backend") != "yes" {
		t.Error("backend response header not forwarded")
	}
	if resp.Header.Get("Server") != "" {
		t.Error("upstream Server header must be stripped")
	}
}

func TestL7DistributesAcrossBackends(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	counted := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name]++
			mu.Unlock()
			io.WriteString(w, name)
		}
	}
	b1, s1 := httpBackend(t, counted("b1"))
	defer s1()
	b2, s2 := httpBackend(t, counted("b2"))
	defer s2()

	p := newProxy(t, Config{}, b1, b2)
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	for i := 0; i < 20; i++ {
		resp, err := http.Get(front.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["b1"] == 0 || hits["b2"] == 0 {
		t.Errorf("round-robin did not reach both backends: %v", hits)
	}
}

// TestL7NeverRoutesByHostHeader is the open-proxy red line: a client cannot
// choose the upstream via Host or an absolute-form target.
func TestL7NeverRoutesByHostHeader(t *testing.T) {
	// A backend that would reveal whether the attacker's chosen host was
	// contacted.
	backendReached := false
	var mu sync.Mutex
	attackerBackend, stopAttacker := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		backendReached = true
		mu.Unlock()
		io.WriteString(w, "attacker")
	})
	defer stopAttacker()

	legit, stopLegit := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "legit")
	})
	defer stopLegit()

	// Proxy is configured ONLY with the legit backend.
	p := newProxy(t, Config{}, legit)
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	// Host header pointing at the attacker's backend.
	req.Host = attackerBackend
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	mu.Lock()
	reached := backendReached
	mu.Unlock()
	if reached {
		t.Fatal("SECURITY: proxy contacted a client-specified host (open proxy)")
	}
	if string(body) != "legit" {
		t.Errorf("body = %q, want the configured backend's response", body)
	}
}

func TestL7RejectsAmbiguousFraming(t *testing.T) {
	legit, stop := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	defer stop()
	p := newProxy(t, Config{}, legit)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x"))
	req.Header.Set("Transfer-Encoding", "chunked")
	req.Header.Set("Content-Length", "1")
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("ambiguous framing: status %d, want 400", rec.Code)
	}
}

func TestL7RejectsOversizedBody(t *testing.T) {
	legit, stop := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	defer stop()
	p := newProxy(t, Config{MaxBodyBytes: 10}, legit)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 100)))
	req.ContentLength = 100
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: status %d, want 413", rec.Code)
	}
}

func TestL7ForwardedForAppendOnly(t *testing.T) {
	var seen string
	backend, stop := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Forwarded-For")
		io.WriteString(w, "ok")
	})
	defer stop()

	// No trusted proxies: inbound XFF must be ignored, not appended to.
	p := newProxy(t, Config{}, backend)
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	req.Header.Set("X-Forwarded-For", "6.6.6.6") // spoof attempt
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if strings.Contains(seen, "6.6.6.6") {
		t.Errorf("spoofed XFF was trusted: %q (must be ignored for untrusted peers)", seen)
	}
	if !strings.HasPrefix(seen, "127.0.0.1") {
		t.Errorf("XFF = %q, want the real peer first", seen)
	}
}

func TestL7TrustedProxyHonorsInboundXFF(t *testing.T) {
	var seen string
	backend, stop := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Forwarded-For")
		io.WriteString(w, "ok")
	})
	defer stop()

	// Trust loopback: inbound XFF from a loopback peer is preserved and
	// appended to.
	p := newProxy(t, Config{TrustedProxies: []string{"127.0.0.0/8"}}, backend)
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(seen, "203.0.113.7") || !strings.Contains(seen, "127.0.0.1") {
		t.Errorf("trusted-proxy XFF = %q, want inbound preserved and peer appended", seen)
	}
}

func TestL7RFC7239Forwarded(t *testing.T) {
	var forwarded string
	backend, stop := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		forwarded = r.Header.Get("Forwarded")
		io.WriteString(w, "ok")
	})
	defer stop()
	p := newProxy(t, Config{Forwarded: "rfc7239"}, backend)
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(forwarded, "for=127.0.0.1") || !strings.Contains(forwarded, "proto=http") {
		t.Errorf("Forwarded = %q, want for= and proto= directives", forwarded)
	}
}

func TestL7UpstreamDownIs502(t *testing.T) {
	// A backend address with nothing listening.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()

	p := newProxy(t, Config{}, deadAddr)
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("dead upstream: status %d, want 502", resp.StatusCode)
	}
}

func TestL7NoBackendsIs502(t *testing.T) {
	p := New(Config{PoolID: "p"}, func() *lbmodel.Snapshot {
		return &lbmodel.Snapshot{Pools: map[lbmodel.PoolID]*lbmodel.Pool{"p": {ID: "p"}}}
	})
	front := httptest.NewServer(p.Handler())
	defer front.Close()
	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("no backends: status %d, want 502", resp.StatusCode)
	}
}

// TestRetryTable is the closed-table contract: the safety decision that
// prevents duplicate non-idempotent writes.
func TestRetryTable(t *testing.T) {
	cases := []struct {
		method    string
		postWrite bool
		idemOpt   bool
		attempts  int
		max       int
		want      RetryDecision
	}{
		{"GET", true, false, 0, 2, RetryAllowed},
		{"HEAD", true, false, 0, 2, RetryAllowed},
		{"OPTIONS", false, false, 0, 2, RetryAllowed},
		{"POST", false, false, 0, 2, RetryAllowed},
		{"POST", true, false, 0, 2, RetryRefusedNonIdempotent},
		{"PATCH", true, false, 0, 2, RetryRefusedNonIdempotent},
		{"PUT", true, false, 0, 2, RetryRefusedNonIdempotent},
		{"PUT", true, true, 0, 2, RetryAllowed},
		{"DELETE", true, true, 0, 2, RetryAllowed},
		{"DELETE", true, false, 0, 2, RetryRefusedNonIdempotent},
		{"FROBNICATE", false, false, 0, 2, RetryRefusedNonIdempotent},
		{"GET", false, false, 2, 2, RetryRefusedBudget},
	}
	for _, c := range cases {
		got := Retryable(c.method, c.postWrite, c.idemOpt, c.attempts, c.max)
		if got != c.want {
			t.Errorf("Retryable(%s, postWrite=%v, idem=%v, att=%d/%d) = %v, want %v",
				c.method, c.postWrite, c.idemOpt, c.attempts, c.max, got, c.want)
		}
	}
}

func TestIsIdempotent(t *testing.T) {
	idem := []string{"GET", "HEAD", "OPTIONS", "TRACE", "PUT", "DELETE"}
	nonIdem := []string{"POST", "PATCH", "FROB"}
	for _, m := range idem {
		if !IsIdempotent(m) {
			t.Errorf("IsIdempotent(%s) = false, want true", m)
		}
	}
	for _, m := range nonIdem {
		if IsIdempotent(m) {
			t.Errorf("IsIdempotent(%s) = true, want false", m)
		}
	}
}

func TestL7KeepsBackendHostByDefault(t *testing.T) {
	var gotHost string
	backend, stop := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		io.WriteString(w, "ok")
	})
	defer stop()
	p := newProxy(t, Config{}, backend)
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	req.Host = "public.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotHost != "public.example.com" {
		t.Errorf("backend saw Host %q, want the client's Host preserved", gotHost)
	}
}

func BenchmarkL7ProxySmall(b *testing.B) {
	backend, stop := httpBackendBench(b)
	defer stop()
	snap := snapshotHTTP(backend)
	p := New(Config{PoolID: "p"}, func() *lbmodel.Snapshot { return snap })
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConns: 64, MaxIdleConnsPerHost: 64}}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(front.URL + "/")
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func httpBackendBench(b *testing.B) (string, func()) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	return strings.TrimPrefix(ts.URL, "http://"), ts.Close
}
