// Package loadgen is the RIFT load generator. Open-loop scenarios schedule
package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Scenario is one named, versioned load definition.
type Scenario struct {
	Name       string        `json:"name"`
	Tool       string        `json:"tool"`
	Target     string        `json:"target"`
	Duration   time.Duration `json:"duration_ms"`
	Warmup     time.Duration `json:"warmup_ms"`
	Loop       string        `json:"loop"`
	Conns      int           `json:"conns"`
	RatePerSec float64       `json:"rate_per_sec"`
	Payload    int           `json:"payload_bytes"`
	Method     string        `json:"method"`
	KeepAlive  float64       `json:"keep_alive_fraction"`
	RawURL     bool          `json:"raw_url"`
}

const (
	defaultDuration = 30 * time.Second
	defaultWarmup   = 5 * time.Second
	defaultConns    = 50
)

func (s Scenario) withDefaults() Scenario {
	if s.Duration <= 0 {
		s.Duration = defaultDuration
	}
	if s.Loop == "" {
		s.Loop = "closed"
	}
	if s.Conns <= 0 {
		s.Conns = defaultConns
	}
	if s.Method == "" {
		s.Method = http.MethodGet
	}
	return s
}

// Sample is one request's raw record. IntendedStart is what makes an
// open-loop run correctable; it is always emitted.
type Sample struct {
	IntendedStart time.Time     `json:"intended_start"`
	ActualStart   time.Time     `json:"actual_start"`
	Latency       time.Duration `json:"latency_ns"`
	Status        int           `json:"status"`
	Bytes         int64         `json:"bytes"`
	ConnID        int           `json:"conn_id"`
	Err           string        `json:"err,omitempty"`
}

// Result summarizes a run.
type Result struct {
	Scenario  Scenario
	Completed uint64
	Errors    uint64
	P50       time.Duration
	P95       time.Duration
	P99       time.Duration
	P999      time.Duration
	Max       time.Duration
	Mean      time.Duration
	Bytes     int64
	Elapsed   time.Duration
	Samples   []Sample
}

// Runner executes scenarios and emits raw samples.
type Runner struct {
	scenario Scenario
	client   *http.Client
	samples  []Sample
	mu       sync.Mutex

	completed atomic.Uint64
	errors    atomic.Uint64

	// OutDir, when set, receives samples.ndjson.
	OutDir string
}

// NewRunner builds a runner for the scenario.
func NewRunner(s Scenario) *Runner {
	s = s.withDefaults()
	return &Runner{
		scenario: s,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        s.Conns * 2,
				MaxIdleConnsPerHost: s.Conns * 2,
				IdleConnTimeout:     90 * time.Second,
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			},
		},
	}
}

// Run executes the scenario for its steady-state window, writing NDJSON
// samples.
//
// The window is enforced by a STOP signal that ends the request loop, not by
// canceling in-flight requests: aborting them would book the scenario's own
// shutdown as a server error and corrupt the error rate.
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	loopCtx, stopLoop := context.WithCancel(context.Background())
	defer stopLoop()
	// A parent cancel (caller gives up) still ends everything.
	go func() {
		select {
		case <-ctx.Done():
			stopLoop()
		case <-loopCtx.Done():
		}
	}()

	window := r.scenario.Warmup + r.scenario.Duration
	timer := time.AfterFunc(window, stopLoop)

	start := time.Now()
	if r.scenario.Loop == "open" {
		r.runOpen(loopCtx)
	} else {
		r.runClosed(loopCtx)
	}
	timer.Stop()
	elapsed := time.Since(start)

	res := r.summarize()
	res.Elapsed = elapsed

	if r.OutDir != "" {
		if err := r.writeSamples(); err != nil {
			return res, err
		}
	}
	return res, nil
}

// runClosed keeps Conns workers each issuing requests back-to-back. This is
// the classic closed-loop model: it cannot report queueing latency, because
// a request is never held back by a previous one.
func (r *Runner) runClosed(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < r.scenario.Conns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r.workerLoop(ctx, id)
		}(i)
	}
	wg.Wait()
}

func (r *Runner) workerLoop(ctx context.Context, id int) {
	for {
		// Check the stop signal BEFORE issuing: a request started after the
		// window closed would be aborted by the same cancellation and
		// miscounted as a server error.
		select {
		case <-ctx.Done():
			return
		default:
		}
		started := time.Now()
		s := r.oneRequest(context.Background(), id, started, started)
		r.record(s)
	}
}

// runOpen schedules requests at fixed intended intervals, independent of
// completion. This is what makes latency percentiles honest: a slow response
// does not delay the next request's deadline, so the queueing delay shows up
// where it belongs.
func (r *Runner) runOpen(ctx context.Context) {
	if r.scenario.RatePerSec <= 0 {
		r.scenario.RatePerSec = 1000
	}
	interval := time.Duration(float64(time.Second) / r.scenario.RatePerSec)
	workers := r.scenario.Conns
	if workers < 1 {
		workers = 1
	}

	// A buffered channel of intended start times feeds the workers.
	intents := make(chan time.Time, workers*2)
	var sched sync.WaitGroup
	sched.Add(1)
	go func() {
		defer sched.Done()
		defer close(intents)
		t0 := time.Now()
		next := t0
		i := 0
		for {
			now := time.Now()
			if now.Before(next) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(next.Sub(now)):
				}
			}
			select {
			case intents <- next:
			case <-ctx.Done():
				return
			}
			i++
			next = t0.Add(time.Duration(i) * interval)
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for intended := range intents {
				actual := time.Now()
				// Each request gets its own bounded context so a hung
				// server cannot pin a worker forever, while the loop's stop
				// signal remains independent of request completion.
				reqCtx, cancel := context.WithTimeout(context.Background(), r.scenario.Duration)
				s := r.oneRequest(reqCtx, id, intended, actual)
				cancel()
				r.record(s)
			}
		}(w)
	}
	wg.Wait()
	sched.Wait()
}

// oneRequest performs one HTTP request, deliberately discarding the body so
// the measurement is of the server's response path, not the client's
// allocation.
func (r *Runner) oneRequest(ctx context.Context, connID int, intended, actual time.Time) Sample {
	s := Sample{IntendedStart: intended, ActualStart: actual, ConnID: connID}

	var body io.Reader
	if r.scenario.Payload > 0 && r.scenario.Method != http.MethodGet &&
		r.scenario.Method != http.MethodHead {
		body = strings.NewReader(strings.Repeat("x", r.scenario.Payload))
	}
	req, err := http.NewRequestWithContext(ctx, r.scenario.Method, r.scenario.Target, body)
	if err != nil {
		s.Err = err.Error()
		return s
	}
	if body != nil {
		req.ContentLength = int64(r.scenario.Payload)
	}
	// KeepAlive is a fraction of requests that reuse a connection. The
	// default (0 unset) means keep-alive throughout: forcing Close would
	// silently turn every scenario into a connection-churn benchmark.
	req.Close = r.scenario.KeepAlive < 0

	t0 := time.Now()
	resp, err := r.client.Do(req)
	s.Latency = time.Since(t0)
	if err != nil {
		s.Err = err.Error()
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			s.Err = "timeout"
		}
		return s
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, resp.Body)
	s.Status = resp.StatusCode
	s.Bytes = n
	return s
}

func (r *Runner) record(s Sample) {
	r.mu.Lock()
	r.samples = append(r.samples, s)
	r.mu.Unlock()
	if s.Err != "" {
		r.errors.Add(1)
	} else {
		r.completed.Add(1)
	}
}

// summarize computes percentiles from recorded samples. Observed (completed)
// requests only: an errored request has no meaningful latency.
func (r *Runner) summarize() *Result {
	r.mu.Lock()
	samples := make([]Sample, len(r.samples))
	copy(samples, r.samples)
	r.mu.Unlock()

	res := &Result{
		Scenario:  r.scenario,
		Completed: r.completed.Load(),
		Errors:    r.errors.Load(),
		Samples:   samples,
	}
	var latencies []time.Duration
	var totalBytes int64
	for _, s := range samples {
		if s.Err != "" {
			continue
		}
		latencies = append(latencies, s.Latency)
		totalBytes += s.Bytes
	}
	res.Bytes = totalBytes
	if len(latencies) == 0 {
		return res
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	res.P50 = percentile(latencies, 0.50)
	res.P95 = percentile(latencies, 0.95)
	res.P99 = percentile(latencies, 0.99)
	res.P999 = percentile(latencies, 0.999)
	res.Max = latencies[len(latencies)-1]
	var sum time.Duration
	for _, l := range latencies {
		sum += l
	}
	res.Mean = sum / time.Duration(len(latencies))
	return res
}

// percentile uses nearest-rank on the sorted slice: with log-bucketed
// histograms unavailable in stdlib, nearest-rank on raw samples is the
// honest choice (it never interpolates a value that was not observed).
func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func (r *Runner) writeSamples() error {
	if err := os.MkdirAll(r.OutDir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(r.OutDir, "samples.ndjson"))
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.samples {
		if err := enc.Encode(r.samples[i]); err != nil {
			return err
		}
	}
	return nil
}

// LoadScenario reads a scenario definition from bench/scenarios/<name>.json.
func LoadScenario(dir, name string) (Scenario, error) {
	path := filepath.Join(dir, name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, fmt.Errorf("scenario %q not found at %s: %w", name, path, err)
	}
	var s Scenario
	if err := json.Unmarshal(raw, &s); err != nil {
		return Scenario{}, fmt.Errorf("scenario %q is not valid JSON: %w", name, err)
	}
	if s.Name == "" {
		s.Name = name
	}
	return s.withDefaults(), nil
}

// ListScenarios returns the scenario names available in dir.
func ListScenarios(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

// Summary renders a human-readable result.
func (r *Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "scenario %s (%s, %s loop)\n", r.Scenario.Name, r.Scenario.Tool, r.Scenario.Loop)
	fmt.Fprintf(&b, "  target       %s\n", r.Scenario.Target)
	fmt.Fprintf(&b, "  elapsed      %s\n", r.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(&b, "  completed    %d\n", r.Completed)
	fmt.Fprintf(&b, "  errors       %d\n", r.Errors)
	if r.Completed > 0 {
		rps := float64(r.Completed) / r.Elapsed.Seconds()
		fmt.Fprintf(&b, "  throughput   %.1f req/s\n", rps)
		fmt.Fprintf(&b, "  p50/p95/p99  %v / %v / %v\n", r.P50.Round(time.Microsecond),
			r.P95.Round(time.Microsecond), r.P99.Round(time.Microsecond))
		fmt.Fprintf(&b, "  p99.9/max    %v / %v\n", r.P999.Round(time.Microsecond), r.Max.Round(time.Microsecond))
		fmt.Fprintf(&b, "  mean         %v\n", r.Mean.Round(time.Microsecond))
		fmt.Fprintf(&b, "  bytes        %d\n", r.Bytes)
	}
	return b.String()
}
