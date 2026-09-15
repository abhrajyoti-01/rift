package lbhealth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	lbmodel "github.com/abhrajyoti-01/rift/internal/lb/model"
)

func liveListener(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestTCPCheckerHealthy(t *testing.T) {
	addr, stop := liveListener(t)
	defer stop()
	b := &lbmodel.Backend{ID: "b", Addr: addr}
	res := TCPChecker{Timeout: time.Second}.Check(context.Background(), b)
	if !res.Healthy {
		t.Errorf("live listener reported unhealthy: %s", res.Detail)
	}
	if res.Latency <= 0 {
		t.Error("latency not measured")
	}
}

func TestTCPCheckerDeadPort(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listening now

	b := &lbmodel.Backend{ID: "b", Addr: addr}
	res := TCPChecker{Timeout: 300 * time.Millisecond}.Check(context.Background(), b)
	if res.Healthy {
		t.Error("dead port reported healthy")
	}
	if res.Detail == "" {
		t.Error("failure detail should explain why, for logs")
	}
}

func TestHTTPCheckerMatchesStatusSet(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	ok := HTTPChecker{Timeout: time.Second, Path: "/healthz", Expect: []int{200, 204}}
	if res := ok.Check(context.Background(), &lbmodel.Backend{Addr: addr}); !res.Healthy {
		t.Errorf("204 should match {200,204}: %s", res.Detail)
	}

	strict := HTTPChecker{Timeout: time.Second, Path: "/healthz", Expect: []int{200}}
	if res := strict.Check(context.Background(), &lbmodel.Backend{Addr: addr}); res.Healthy {
		t.Error("204 should NOT match {200}")
	}

	bad := HTTPChecker{Timeout: time.Second, Path: "/other", Expect: []int{200}}
	if res := bad.Check(context.Background(), &lbmodel.Backend{Addr: addr}); res.Healthy {
		t.Error("500 should not be healthy")
	}
}

func TestHTTPCheckerDefaultsTo200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()
	c := HTTPChecker{Timeout: time.Second} // no path, no expect
	if res := c.Check(context.Background(), &lbmodel.Backend{Addr: ts.Listener.Addr().String()}); !res.Healthy {
		t.Errorf("default check should be healthy against 200: %s", res.Detail)
	}
}

func TestHTTPCheckerTimeoutIsUnhealthy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer ts.Close()
	c := HTTPChecker{Timeout: 50 * time.Millisecond}
	res := c.Check(context.Background(), &lbmodel.Backend{Addr: ts.Listener.Addr().String()})
	if res.Healthy {
		t.Error("timed-out check reported healthy")
	}
}

func TestNewSelectsCheckerByKind(t *testing.T) {
	if _, ok := New(Config{Kind: "tcp"}).(TCPChecker); !ok {
		t.Error("kind tcp should build a TCPChecker")
	}
	if _, ok := New(Config{Kind: "http"}).(HTTPChecker); !ok {
		t.Error("kind http should build an HTTPChecker")
	}
	hc := New(Config{Kind: "http", Path: "/status", Expect: []int{200, 202}}).(HTTPChecker)
	if hc.Path != "/status" {
		t.Errorf("configured path lost: %q", hc.Path)
	}
	if len(hc.Expect) != 2 || hc.Expect[0] != 200 || hc.Expect[1] != 202 {
		t.Errorf("configured expect set lost: %v", hc.Expect)
	}
}

// TestTrackerHysteresis is the core health-stability property: a flapping
// backend must not flip on every single probe.
func TestTrackerHysteresis(t *testing.T) {
	tr := NewTracker(2, 3)
	// Fresh backend starts unhealthy and must EARN traffic (rise=2).
	if tr.Observe(true) {
		t.Error("one pass should not be enough with rise=2")
	}
	if !tr.Observe(true) {
		t.Error("two passes should mark healthy with rise=2")
	}
	// Now flapping: one failure is not enough to flip (fall=3).
	if !tr.Observe(false) {
		t.Error("one failure must not flip a healthy backend with fall=3")
	}
	if !tr.Observe(false) {
		t.Error("two failures must not flip with fall=3")
	}
	if tr.Observe(false) {
		t.Error("three consecutive failures should flip to unhealthy with fall=3")
	}
	// Recovery requires rise again.
	if tr.Observe(true) {
		t.Error("one pass should not recover with rise=2")
	}
	if !tr.Observe(true) {
		t.Error("two passes should recover")
	}
}

func TestTrackerFailureResetsPassCount(t *testing.T) {
	tr := NewTracker(3, 1)
	tr.Observe(true)
	tr.Observe(true)
	tr.Observe(false) // resets passes
	if tr.Observe(true) {
		t.Error("pass counter must reset after a failure (3 consecutive required)")
	}
}

func TestTrackerDefaultsAreApplied(t *testing.T) {
	tr := NewTracker(0, 0) // defaults: rise 2, fall 3
	if tr.Observe(true) {
		t.Error("rise default should be 2, not 1")
	}
	if !tr.Observe(true) {
		t.Error("second pass should succeed with default rise")
	}
}
