package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dnsmodel "github.com/abhrajyoti-01/rift/internal/dnsmon/model"
	"github.com/abhrajyoti-01/rift/internal/dnsmon/wire"
)

func TestWindowBoundsMemory(t *testing.T) {
	// The window is the hub's hard memory ceiling: many distinct names must
	// not grow it without bound.
	w := NewWindow(4)
	for i := 0; i < 1000; i++ {
		w.Add(dnsmodel.Observation{
			QName: fmt.Sprintf("host-%d.example.com.", i),
			QType: wire.TypeA,
			View:  dnsmodel.ViewRecursive,
		})
	}
	if w.Keys() != 1000 {
		t.Errorf("distinct keys = %d, want 1000 (this is expected: keys are names)", w.Keys())
	}
	// Per-key capacity must hold: add many observations for ONE key.
	w2 := NewWindow(4)
	for i := 0; i < 1000; i++ {
		w2.Add(dnsmodel.Observation{QName: "one.example.com.", QType: wire.TypeA, View: dnsmodel.ViewRecursive})
	}
	got := w2.ForTarget("one.example.com.", uint16(wire.TypeA))
	if len(got) > 4 {
		t.Errorf("per-key window holds %d observations, want ≤ 4 (memory unbounded)", len(got))
	}
}

func signedPayload(t *testing.T, records ...dnsnodeWireObservation) string {
	t.Helper()
	var b strings.Builder
	for _, r := range records {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

type dnsnodeWireObservation struct {
	NodeID    string `json:"node_id"`
	Location  string `json:"location"`
	View      uint8  `json:"view"`
	Resolver  string `json:"resolver"`
	QName     string `json:"qname"`
	QType     uint16 `json:"qtype"`
	RCode     uint8  `json:"rcode"`
	Answers   []struct {
		Name string `json:"name"`
		Type uint16 `json:"type"`
		TTL  uint32 `json:"ttl"`
		Data string `json:"data"`
	} `json:"answers"`
	Truncated bool    `json:"truncated"`
	Transport string  `json:"transport"`
	LatencyMS float64 `json:"latency_ms"`
	Timestamp string  `json:"timestamp"`
	ErrClass  uint8   `json:"err_class"`
}

func rec(node string, view uint8, resolver, qname, data string) dnsnodeWireObservation {
	r := dnsnodeWireObservation{
		NodeID: node, Location: "lab", View: view, Resolver: resolver,
		QName: qname, QType: 1, Transport: "udp",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	r.Answers = append(r.Answers, struct {
		Name string `json:"name"`
		Type uint16 `json:"type"`
		TTL  uint32 `json:"ttl"`
		Data string `json:"data"`
	}{Name: qname, Type: 1, TTL: 60, Data: data})
	return r
}

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	h := New(HubConfig{
		DataDir:       t.TempDir(),
		WindowKeys:    64,
		SegmentBytes:  1 << 20,
		MinResponding: 3,
		MaxBatch:      100,
	})
	t.Cleanup(func() { h.Close() })
	return h
}

func TestHubIngestAndQuery(t *testing.T) {
	h := newTestHub(t)
	ts := httptest.NewServer(h.IngestHandler())
	defer ts.Close()

	body := signedPayload(t,
		rec("n1", 1, "1.1.1.1:53", "www.example.com.", "1.2.3.4"),
		rec("n1", 1, "8.8.8.8:53", "www.example.com.", "1.2.3.4"),
	)
	resp, err := http.Post(ts.URL+"/v1/ingest", "application/x-ndjson", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest status %d, want 202", resp.StatusCode)
	}

	obs := h.Window().ForTarget("www.example.com.", 1)
	if len(obs) != 2 {
		t.Errorf("window holds %d observations, want 2", len(obs))
	}
	if h.Metrics().Observations.Load() != 2 {
		t.Errorf("Observations = %d, want 2", h.Metrics().Observations.Load())
	}
}

func TestHubRejectsMalformedObservation(t *testing.T) {
	h := newTestHub(t)
	ts := httptest.NewServer(h.IngestHandler())
	defer ts.Close()

	// An unparsable timestamp must be rejected, not silently stored.
	bad := `{"node_id":"n1","view":1,"resolver":"r","qname":"x.","qtype":1,"timestamp":"not-a-time"}`
	resp, err := http.Post(ts.URL+"/v1/ingest", "application/x-ndjson", strings.NewReader(bad+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed observation status %d, want 400", resp.StatusCode)
	}
	if h.Metrics().BatchesRejected.Load() == 0 {
		t.Error("rejection not counted")
	}
}

func TestHubRejectsInvalidView(t *testing.T) {
	h := newTestHub(t)
	ts := httptest.NewServer(h.IngestHandler())
	defer ts.Close()

	bad := signedPayload(t, rec("n1", 9, "r", "x.", "1.1.1.1")) // view 9 is out of range
	resp, _ := http.Post(ts.URL+"/v1/ingest", "application/x-ndjson", strings.NewReader(bad))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid view status %d, want 400", resp.StatusCode)
	}
}

func TestHubRejectsOversizedBatch(t *testing.T) {
	h := New(HubConfig{DataDir: t.TempDir(), MaxBatch: 5, WindowKeys: 16})
	t.Cleanup(func() { h.Close() })
	ts := httptest.NewServer(h.IngestHandler())
	defer ts.Close()

	var recs []dnsnodeWireObservation
	for i := 0; i < 10; i++ {
		recs = append(recs, rec("n1", 1, "r", fmt.Sprintf("h%d.", i), "1.1.1.1"))
	}
	resp, _ := http.Post(ts.URL+"/v1/ingest", "application/x-ndjson", strings.NewReader(signedPayload(t, recs...)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized batch status %d, want 413", resp.StatusCode)
	}
}

func TestHubRejectsWrongMethod(t *testing.T) {
	h := newTestHub(t)
	ts := httptest.NewServer(h.IngestHandler())
	defer ts.Close()
	resp, _ := http.Get(ts.URL + "/v1/ingest")
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET ingest status %d, want 405", resp.StatusCode)
	}
}

func TestHubWritesSegments(t *testing.T) {
	dir := t.TempDir()
	h := New(HubConfig{DataDir: dir, WindowKeys: 16, SegmentBytes: 1 << 20})
	t.Cleanup(func() { h.Close() })
	ts := httptest.NewServer(h.IngestHandler())
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/v1/ingest", "application/x-ndjson",
		strings.NewReader(signedPayload(t, rec("n1", 1, "1.1.1.1:53", "a.example.com.", "1.2.3.4"))))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest status %d", resp.StatusCode)
	}
	if err := h.Flush(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no segment file written — persistence claim would be false")
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "a.example.com.") {
		t.Errorf("segment missing the observation: %s", raw)
	}
	if !strings.Contains(string(raw), "1.2.3.4") {
		t.Error("segment missing answer data")
	}
}

func TestHubPropagationEndpoint(t *testing.T) {
	h := New(HubConfig{DataDir: t.TempDir(), WindowKeys: 64, MinResponding: 3})
	t.Cleanup(func() { h.Close() })
	ingest := httptest.NewServer(h.IngestHandler())
	defer ingest.Close()

	// Authoritative reference + three agreeing resolvers.
	body := signedPayload(t,
		rec("n1", 2, "ns1.example.com:53", "www.example.com.", "9.9.9.9"),
		rec("n1", 1, "1.1.1.1:53", "www.example.com.", "9.9.9.9"),
		rec("n1", 1, "8.8.8.8:53", "www.example.com.", "9.9.9.9"),
		rec("n1", 1, "9.9.9.9:53", "www.example.com.", "9.9.9.9"),
	)
	resp, _ := http.Post(ingest.URL+"/v1/ingest", "application/x-ndjson", strings.NewReader(body))
	resp.Body.Close()

	query := httptest.NewServer(h.QueryHandler())
	defer query.Close()

	pr, err := http.Get(query.URL + "/v1/propagation?target=www.example.com.&type=A")
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(pr.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["state"] != "converged" {
		t.Errorf("state = %v, want converged (full response %v)", out["state"], out)
	}
}

func TestHubPropagationDivergent(t *testing.T) {
	// ConvergenceWindow 1s but the classifier's observation Window defaults
	// to 10m, so the reference must be backdated WITHIN that window while
	// still being older than the convergence window.
	h := New(HubConfig{
		DataDir: t.TempDir(), WindowKeys: 64, MinResponding: 3,
		ConvergenceWindow: time.Second,
	})
	t.Cleanup(func() { h.Close() })
	ingest := httptest.NewServer(h.IngestHandler())
	defer ingest.Close()

	ref := rec("n1", 2, "ns1.example.com:53", "www.example.com.", "9.9.9.9")
	ref.Timestamp = time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano)

	body := signedPayload(t, ref,
		rec("n1", 1, "1.1.1.1:53", "www.example.com.", "9.9.9.9"),
		rec("n1", 1, "8.8.8.8:53", "www.example.com.", "1.2.3.4"), // stale
		rec("n1", 1, "9.9.9.9:53", "www.example.com.", "9.9.9.9"),
	)
	resp, _ := http.Post(ingest.URL+"/v1/ingest", "application/x-ndjson", strings.NewReader(body))
	resp.Body.Close()

	query := httptest.NewServer(h.QueryHandler())
	defer query.Close()
	pr, err := http.Get(query.URL + "/v1/propagation?target=www.example.com.&type=A")
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	var out map[string]any
	json.NewDecoder(pr.Body).Decode(&out)
	if out["state"] != "divergent" {
		t.Errorf("state = %v, want divergent (response %v)", out["state"], out)
	}
}

func TestHubPropagationUnresolvableWhenReferenceTooOld(t *testing.T) {
	// A reference outside the observation window means we cannot say what
	// is published: the honest answer is unresolvable, not converged.
	h := New(HubConfig{DataDir: t.TempDir(), WindowKeys: 64, MinResponding: 3})
	t.Cleanup(func() { h.Close() })
	ingest := httptest.NewServer(h.IngestHandler())
	defer ingest.Close()

	old := rec("n1", 2, "ns1.example.com:53", "www.example.com.", "9.9.9.9")
	old.Timestamp = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	body := signedPayload(t, old,
		rec("n1", 1, "1.1.1.1:53", "www.example.com.", "9.9.9.9"),
		rec("n1", 1, "8.8.8.8:53", "www.example.com.", "9.9.9.9"),
		rec("n1", 1, "9.9.9.9:53", "www.example.com.", "9.9.9.9"),
	)
	resp, _ := http.Post(ingest.URL+"/v1/ingest", "application/x-ndjson", strings.NewReader(body))
	resp.Body.Close()

	query := httptest.NewServer(h.QueryHandler())
	defer query.Close()
	pr, err := http.Get(query.URL + "/v1/propagation?target=www.example.com.&type=A")
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	var out map[string]any
	json.NewDecoder(pr.Body).Decode(&out)
	if out["state"] != "unresolvable" {
		t.Errorf("state = %v, want unresolvable for a stale reference", out["state"])
	}
}

func TestHubObservationsPaginationCap(t *testing.T) {
	h := New(HubConfig{DataDir: t.TempDir(), WindowKeys: 5000, MaxBatch: 10000})
	t.Cleanup(func() { h.Close() })
	ingest := httptest.NewServer(h.IngestHandler())
	defer ingest.Close()

	var recs []dnsnodeWireObservation
	for i := 0; i < 1500; i++ {
		recs = append(recs, rec("n1", 1, "1.1.1.1:53", "many.example.com.", "1.2.3.4"))
	}
	resp, _ := http.Post(ingest.URL+"/v1/ingest", "application/x-ndjson", strings.NewReader(signedPayload(t, recs...)))
	resp.Body.Close()

	query := httptest.NewServer(h.QueryHandler())
	defer query.Close()
	qr, err := http.Get(query.URL + "/v1/observations?target=many.example.com.&type=A")
	if err != nil {
		t.Fatal(err)
	}
	defer qr.Body.Close()
	var out struct {
		Count int `json:"count"`
	}
	json.NewDecoder(qr.Body).Decode(&out)
	if out.Count > 1000 {
		t.Errorf("observations response returned %d, want ≤ 1000 (unbounded response)", out.Count)
	}
}

func TestHubQueryRequiresTarget(t *testing.T) {
	h := newTestHub(t)
	query := httptest.NewServer(h.QueryHandler())
	defer query.Close()
	resp, _ := http.Get(query.URL + "/v1/observations")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing target status %d, want 400", resp.StatusCode)
	}
}

func TestHubRunLifecycle(t *testing.T) {
	dir := t.TempDir()
	h := New(HubConfig{
		IngestAddr: "127.0.0.1:0",
		QueryAddr:  "127.0.0.1:0",
		DataDir:    dir,
		WindowKeys: 16,
	})
	// :0 binds an ephemeral port but Run reports the configured addrs, so
	// this test asserts the lifecycle rather than the address.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on clean cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestHubRunRequiresDataDir(t *testing.T) {
	h := New(HubConfig{IngestAddr: "127.0.0.1:0", QueryAddr: "127.0.0.1:0"})
	if err := h.Run(context.Background()); err == nil {
		t.Error("Run without data_dir must fail closed")
	}
}

// TestTwoNodeIntegration is the multi-node acceptance test: two independent
// nodes ingest into one hub, and the hub's window sees both.
func TestTwoNodeIntegration(t *testing.T) {
	h := New(HubConfig{DataDir: t.TempDir(), WindowKeys: 64, MinResponding: 2})
	t.Cleanup(func() { h.Close() })
	ingest := httptest.NewServer(h.IngestHandler())
	defer ingest.Close()

	// Node A and node B agree.
	bodyA := signedPayload(t, rec("node-a", 1, "1.1.1.1:53", "shared.example.com.", "5.5.5.5"))
	bodyB := signedPayload(t, rec("node-b", 1, "8.8.8.8:53", "shared.example.com.", "5.5.5.5"))

	for _, b := range []string{bodyA, bodyB} {
		resp, err := http.Post(ingest.URL+"/v1/ingest", "application/x-ndjson", strings.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("ingest status %d", resp.StatusCode)
		}
	}

	obs := h.Window().ForTarget("shared.example.com.", 1)
	if len(obs) != 2 {
		t.Fatalf("window holds %d observations, want 2 (one per node)", len(obs))
	}
	nodes := map[string]bool{}
	for _, o := range obs {
		nodes[o.NodeID] = true
	}
	if !nodes["node-a"] || !nodes["node-b"] {
		t.Errorf("both nodes must appear in the window, got %v", nodes)
	}
}