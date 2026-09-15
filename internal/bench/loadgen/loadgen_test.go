package loadgen

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClosedLoopRunnerRecordsSamples(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		strings.NewReader("ok")
		w.Write([]byte("ok"))
	}))
	defer ts.Close()

	r := NewRunner(Scenario{
		Name: "test.closed", Target: ts.URL, Loop: "closed",
		Conns: 4, Duration: 300 * time.Millisecond, Method: "GET",
	})
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed == 0 {
		t.Fatal("no requests completed")
	}
	if res.Errors != 0 {
		t.Errorf("unexpected errors: %d (scenario shutdown must not be booked as a server error)", res.Errors)
	}
	// p50 may legitimately be 0 on a fast loopback server (a sub-nanosecond
	// resolution is not meaningful); assert ordering instead.
	if res.P99 < res.P50 {
		t.Error("p99 must be >= p50")
	}
	if res.Max < res.P99 {
		t.Error("max must be >= p99")
	}
}

// TestOpenLoopRecordsIntendedStart is the coordinated-omission contract:
// every sample carries the intended start time, so latency is correctable.
func TestOpenLoopRecordsIntendedStart(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer ts.Close()

	r := NewRunner(Scenario{
		Name: "test.open", Target: ts.URL, Loop: "open",
		Conns: 4, RatePerSec: 200, Duration: 400 * time.Millisecond, Method: "GET",
	})
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed == 0 {
		t.Fatal("no requests completed")
	}
	for i, s := range res.Samples {
		if s.IntendedStart.IsZero() {
			t.Fatalf("sample %d has no intended start — coordinated omission is uncorrectable", i)
		}
	}
}

func TestRunnerCountsErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	r := NewRunner(Scenario{
		Name: "test.5xx", Target: ts.URL, Loop: "closed",
		Conns: 2, Duration: 200 * time.Millisecond, Method: "GET",
	})
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A 5xx is a completed request with a recorded status, not a transport
	// error: the distinction matters for interpreting a run.
	if res.Completed == 0 {
		t.Error("5xx responses should still count as completed requests")
	}
	for _, s := range res.Samples {
		if s.Status != 500 {
			t.Fatalf("status = %d, want 500", s.Status)
		}
	}
}

func TestUnreachableTargetScoresErrors(t *testing.T) {
	r := NewRunner(Scenario{
		Name: "test.dead", Target: "http://127.0.0.1:1/", Loop: "closed",
		Conns: 1, Duration: 150 * time.Millisecond, Method: "GET",
	})
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors == 0 {
		t.Error("unreachable target must produce errors")
	}
}

func TestPercentileNearestRank(t *testing.T) {
	// Nearest-rank never interpolates a value that was not observed.
	vals := []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(vals, 0.5); got != 6 {
		t.Errorf("p50 = %v, want 6 (nearest-rank)", got)
	}
	if got := percentile(vals, 0.99); got != 10 {
		t.Errorf("p99 = %v, want 10", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("empty percentile = %v, want 0", got)
	}
}

func TestScenarioDefaults(t *testing.T) {
	s := Scenario{}.withDefaults()
	if s.Loop != "closed" {
		t.Errorf("default loop = %q, want closed", s.Loop)
	}
	if s.Method != "GET" {
		t.Errorf("default method = %q, want GET", s.Method)
	}
	if s.Duration <= 0 {
		t.Error("default duration must be positive")
	}
}