package dnsnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	dnsmodel "github.com/abhrajyoti-01/rift/internal/dnsmon/model"
	"github.com/abhrajyoti-01/rift/internal/dnsmon/wire"
)

func TestRingDropsOldestAndCounts(t *testing.T) {
	r := NewRing(3)
	for i := 0; i < 3; i++ {
		if r.Put(dnsmodel.Observation{QName: string(rune('a' + i))}) {
			t.Fatalf("Put %d unexpectedly evicted", i)
		}
	}
	if r.Len() != 3 {
		t.Fatalf("Len = %d, want 3", r.Len())
	}
	// The fourth evicts the oldest and reports it.
	if !r.Put(dnsmodel.Observation{QName: "d"}) {
		t.Error("Put past capacity must report an eviction")
	}
	if r.Drops() != 1 {
		t.Errorf("Drops = %d, want 1", r.Drops())
	}
	got := r.Take(10)
	if len(got) != 3 {
		t.Fatalf("Take returned %d, want 3", len(got))
	}
	// FIFO: the oldest surviving entry is "b", not "a" (which was evicted).
	if got[0].QName != "b" {
		t.Errorf("oldest surviving = %q, want \"b\" (oldest must be dropped)", got[0].QName)
	}
}

func TestRingTakeRespectsRequestedCount(t *testing.T) {
	r := NewRing(10)
	for i := 0; i < 5; i++ {
		r.Put(dnsmodel.Observation{QName: "x"})
	}
	if got := r.Take(2); len(got) != 2 {
		t.Errorf("Take(2) returned %d", len(got))
	}
	if r.Len() != 3 {
		t.Errorf("Len after Take(2) = %d, want 3", r.Len())
	}
	if got := r.Take(100); len(got) != 3 {
		t.Errorf("Take(100) returned %d, want remainder 3", len(got))
	}
}

func TestRingMemoryIsBounded(t *testing.T) {
	// The whole point of the ring: 100k Puts into a 64-slot ring must not
	// grow memory, and every eviction is counted.
	r := NewRing(64)
	for i := 0; i < 100000; i++ {
		r.Put(dnsmodel.Observation{Resolver: "1.1.1.1:53"})
	}
	if r.Len() != 64 {
		t.Errorf("Len = %d, want capacity 64 (memory must be bounded)", r.Len())
	}
	if r.Drops() != 100000-64 {
		t.Errorf("Drops = %d, want %d", r.Drops(), 100000-64)
	}
}

// hubStub records ingested batches.
type hubStub struct {
	mu      sync.Mutex
	batches [][]WireObservation
	fail    bool
}

func (h *hubStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.fail {
			http.Error(w, "nope", http.StatusServiceUnavailable)
			return
		}
		dec := json.NewDecoder(r.Body)
		var batch []WireObservation
		for dec.More() {
			var wo WireObservation
			if err := dec.Decode(&wo); err != nil {
				break
			}
			batch = append(batch, wo)
		}
		h.batches = append(h.batches, batch)
		w.WriteHeader(http.StatusAccepted)
	})
}

func (h *hubStub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, b := range h.batches {
		n += len(b)
	}
	return n
}

func TestNodeShipsObservationsToHub(t *testing.T) {
	stub := &hubStub{}
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	// Build observations directly through the ring/ship path: the probe
	// path needs live resolvers, which is covered by network-gated tests.
	n := New(NodeConfig{
		NodeID:    "node-1",
		Location:  "test-lab",
		HubURL:    ts.URL,
		RingCap:   16,
		ShipEvery: 20 * time.Millisecond,
		ShipSize:  4,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); n.shipLoop(ctx) }()

	for i := 0; i < 10; i++ {
		n.ring.Put(dnsmodel.Observation{
			NodeID:    "node-1",
			View:      dnsmodel.ViewRecursive,
			Resolver:  "1.1.1.1:53",
			QName:     "example.com.",
			QType:     wire.TypeA,
			Timestamp: time.Now().UTC(),
			Answers:   []dnsmodel.Answer{{Name: "example.com.", Type: wire.TypeA, Data: "1.2.3.4", TTL: 60}},
		})
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && stub.count() < 10 {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if got := stub.count(); got < 10 {
		t.Errorf("hub received %d observations, want 10 shipped", got)
	}
	if n.Metrics().Shipped.Load() == 0 {
		t.Error("Shipped counter not incremented")
	}
}

func TestNodeFlushesOnShutdown(t *testing.T) {
	stub := &hubStub{}
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	n := New(NodeConfig{
		NodeID:    "node-1",
		HubURL:    ts.URL,
		RingCap:   16,
		ShipEvery: time.Hour, // never fires: only shutdown should flush
		ShipSize:  1000,      // never reached
	})
	n.ring.Put(dnsmodel.Observation{
		NodeID: "node-1", QName: "x.", QType: wire.TypeA, Timestamp: time.Now().UTC(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); n.shipLoop(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel() // triggers the final flush
	<-done

	if stub.count() != 1 {
		t.Errorf("shutdown flush delivered %d observations, want 1 (silent loss on shutdown)", stub.count())
	}
}

func TestNodeSpoolsOnHubFailure(t *testing.T) {
	stub := &hubStub{fail: true}
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	spoolDir := t.TempDir()
	n := New(NodeConfig{
		NodeID:    "node-1",
		HubURL:    ts.URL,
		RingCap:   16,
		ShipEvery: 20 * time.Millisecond,
		SpoolDir:  spoolDir,
		SpoolMax:  1 << 20,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); n.shipLoop(ctx) }()

	for i := 0; i < 5; i++ {
		n.ring.Put(dnsmodel.Observation{NodeID: "node-1", QName: "x.", QType: wire.TypeA, Timestamp: time.Now().UTC()})
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	if n.Metrics().ShipFailures.Load() == 0 {
		t.Error("ship failures not counted")
	}
	if n.Metrics().Spooled.Load() == 0 {
		t.Error("failed batch was not spooled (outage data would be lost)")
	}
	if n.Metrics().SpoolBytes.Load() <= 0 {
		t.Error("spool bytes not tracked")
	}
}

func TestNodeDrainSpool(t *testing.T) {
	stub := &hubStub{fail: true}
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	spoolDir := t.TempDir()
	n := New(NodeConfig{
		NodeID: "n", HubURL: ts.URL, SpoolDir: spoolDir, SpoolMax: 1 << 20,
	})
	// Spool two batches while the hub is down.
	for i := 0; i < 2; i++ {
		if !n.spool([]dnsmodel.Observation{{NodeID: "n", QName: "x."}}) {
			t.Fatal("spool failed")
		}
	}

	// Hub recovers.
	stub.mu.Lock()
	stub.fail = false
	stub.mu.Unlock()

	if err := n.DrainSpool(context.Background()); err != nil {
		t.Fatalf("DrainSpool: %v", err)
	}
	if n.Metrics().SpoolBytes.Load() != 0 {
		t.Errorf("spool bytes after drain = %d, want 0", n.Metrics().SpoolBytes.Load())
	}
}

func TestNodeRunRequiresHubURL(t *testing.T) {
	n := New(NodeConfig{NodeID: "n"})
	err := n.Run(context.Background())
	if err == nil {
		t.Error("Run without a hub URL must fail closed rather than silently drop data")
	}
}

func TestWireObservationRoundTrip(t *testing.T) {
	n := New(NodeConfig{NodeID: "n", Location: "loc"})
	orig := dnsmodel.Observation{
		NodeID:    "n",
		View:      dnsmodel.ViewAuthoritative,
		Resolver:  "ns1.example.com:53",
		QName:     "example.com.",
		QType:     wire.TypeMX,
		RCode:     0,
		Answers:   []dnsmodel.Answer{{Name: "example.com.", Type: wire.TypeMX, TTL: 300, Data: "mail.example.com."}},
		Transport: "udp",
		Latency:   12 * time.Millisecond,
		Timestamp: time.Now().UTC(),
	}
	wo := n.wireObservation(orig)

	raw, err := json.Marshal(wo)
	if err != nil {
		t.Fatal(err)
	}
	var back WireObservation
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.NodeID != "n" || back.QName != orig.QName || back.QType != uint16(wire.TypeMX) {
		t.Errorf("round trip mismatch: %+v", back)
	}
	if len(back.Answers) != 1 || back.Answers[0].Data != "mail.example.com." {
		t.Errorf("answers lost in round trip: %+v", back.Answers)
	}
	if back.Location != "loc" {
		t.Errorf("location = %q, want loc", back.Location)
	}
}