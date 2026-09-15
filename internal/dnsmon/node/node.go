package dnsnode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	dnsmodel "github.com/abhrajyoti-01/rift/internal/dnsmon/model"
	"github.com/abhrajyoti-01/rift/internal/dnsmon/probe"
	"github.com/abhrajyoti-01/rift/internal/dnsmon/resolver"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// NodeConfig configures one monitoring node.
type NodeConfig struct {
	NodeID    string
	Location  string
	Resolvers []resolver.EngineConfig
	Targets   []dnsmodel.Target
	Interval  time.Duration // per target, default 60s
	RingCap   int           // default 65536; hard memory ceiling
	ShipEvery time.Duration // batch age trigger, default 5s
	ShipSize  int           // batch size trigger, default 512
	HubURL    string
	SpoolDir  string // offline spool directory; empty disables spooling
	SpoolMax  int64  // spool byte cap; default 64 MiB

	// Now is injectable for tests.
	Now func() time.Time
}

const (
	defaultInterval  = 60 * time.Second
	defaultRingCap   = 65536
	defaultShipEvery = 5 * time.Second
	defaultShipSize  = 512
	defaultSpoolMax  = 64 << 20
)

func (c NodeConfig) withDefaults() NodeConfig {
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.RingCap <= 0 {
		c.RingCap = defaultRingCap
	}
	if c.ShipEvery <= 0 {
		c.ShipEvery = defaultShipEvery
	}
	if c.ShipSize <= 0 {
		c.ShipSize = defaultShipSize
	}
	if c.SpoolMax <= 0 {
		c.SpoolMax = defaultSpoolMax
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Metrics is the node's accounting surface. Dropped observations are
// counted with a reason: a monitor must report its own gaps.
type Metrics struct {
	Observations    atomic.Uint64
	Shipped         atomic.Uint64
	ShipFailures    atomic.Uint64
	DroppedRingFull atomic.Uint64
	DroppedSpool    atomic.Uint64
	Spooled         atomic.Uint64
	SpoolBytes      atomic.Int64
}

// Ring is a fixed-capacity lossy observation buffer. Put drops the OLDEST
// entry when full and reports it, because an unbounded buffer under attack
// is a memory-exhaustion primitive.
type Ring struct {
	mu    sync.Mutex
	buf   []dnsmodel.Observation
	head  int
	size  int
	cap   int
	drops uint64
}

// NewRing builds a ring with the given capacity.
func NewRing(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{buf: make([]dnsmodel.Observation, capacity), cap: capacity}
}

// Put appends an observation, evicting the oldest if full. It reports
// whether an eviction happened.
func (r *Ring) Put(o dnsmodel.Observation) (dropped bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size == r.cap {
		r.buf[r.head] = o
		r.head = (r.head + 1) % r.cap
		r.drops++
		return true
	}
	r.buf[(r.head+r.size)%r.cap] = o
	r.size++
	return false
}

// Take removes and returns up to n observations in FIFO order.
func (r *Ring) Take(n int) []dnsmodel.Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > r.size {
		n = r.size
	}
	if n <= 0 {
		return nil
	}
	out := make([]dnsmodel.Observation, n)
	for i := 0; i < n; i++ {
		out[i] = r.buf[r.head]
		r.buf[r.head] = dnsmodel.Observation{}
		r.head = (r.head + 1) % r.cap
	}
	r.size -= n
	return out
}

// Len reports buffered observations.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.size
}

// Drops reports total evictions.
func (r *Ring) Drops() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drops
}

// Node is the monitoring node process.
type Node struct {
	cfg     NodeConfig
	engines []*resolver.Engine
	prober  *probe.Prober
	ring    *Ring
	client  *http.Client
	metrics Metrics

	spoolMu  sync.Mutex
	spoolSeq atomic.Uint64
}

// New builds a node from config.
func New(cfg NodeConfig) *Node {
	cfg = cfg.withDefaults()
	engines := make([]*resolver.Engine, 0, len(cfg.Resolvers))
	for _, rc := range cfg.Resolvers {
		engines = append(engines, resolver.NewEngine(rc, cfg.Now))
	}
	return &Node{
		cfg:     cfg,
		engines: engines,
		prober: probe.NewWithConfig(probe.Config{
			View:    dnsmodel.ViewRecursive,
			Targets: cfg.Targets,
		}, engines),
		ring:   NewRing(cfg.RingCap),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Metrics returns the accounting surface.
func (n *Node) Metrics() *Metrics { return &n.metrics }

// Ring exposes the buffer for tests and introspection.
func (n *Node) Ring() *Ring { return n.ring }

// Run schedules probes and ships observations until ctx is canceled.
func (n *Node) Run(ctx context.Context) error {
	if n.cfg.HubURL == "" {
		return errs.New(errs.ClassConfig, "dns.node", "hub url is required")
	}
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { defer wg.Done(); n.probeLoop(ctx) }()

	wg.Add(1)
	go func() { defer wg.Done(); n.shipLoop(ctx) }()

	<-ctx.Done()
	wg.Wait()
	return nil
}

// probeLoop runs one probe cycle per interval.
func (n *Node) probeLoop(ctx context.Context) {
	// Probe once immediately so a short-lived run still produces data.
	n.cycle(ctx)

	t := time.NewTicker(n.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.cycle(ctx)
		}
	}
}

func (n *Node) cycle(ctx context.Context) {
	obs := n.prober.ProbeAll(ctx)
	for i := range obs {
		obs[i].NodeID = n.cfg.NodeID
		n.metrics.Observations.Add(1)
		if n.ring.Put(obs[i]) {
			n.metrics.DroppedRingFull.Add(1)
		}
	}
}

// shipLoop batches on size or age, whichever fires first.
func (n *Node) shipLoop(ctx context.Context) {
	t := time.NewTicker(n.cfg.ShipEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Final flush on a DETACHED, bounded context: the request that
			// delivers the last batch must not be aborted by the very
			// cancellation that triggered it, or a clean shutdown silently
			// loses data.
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			n.ship(flushCtx, 0)
			cancel()
			return
		case <-t.C:
			n.ship(ctx, n.cfg.ShipSize)
		}
	}
}

// ship takes up to max observations (0 = drain everything) and posts them.
func (n *Node) ship(ctx context.Context, max int) {
	if max == 0 {
		max = n.ring.Len()
	}
	batch := n.ring.Take(max)
	if len(batch) == 0 {
		return
	}
	if err := n.postBatch(ctx, batch); err != nil {
		n.metrics.ShipFailures.Add(1)
		// Shipping failed: spool the batch so an outage produces a
		// recoverable gap rather than silent loss.
		if !n.spool(batch) {
			// Spool unavailable/full: put them back only if there is room,
			// otherwise count the loss explicitly.
			for _, o := range batch {
				if n.ring.Put(o) {
					n.metrics.DroppedRingFull.Add(1)
				}
			}
		}
		return
	}
	n.metrics.Shipped.Add(uint64(len(batch)))
}

// postBatch sends observations to the hub as JSON lines.
func (n *Node) postBatch(ctx context.Context, batch []dnsmodel.Observation) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range batch {
		if err := enc.Encode(n.wireObservation(batch[i])); err != nil {
			return errs.Wrap(err, errs.ClassConfig, "dns.node", "encode observation")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.HubURL+"/v1/ingest", &buf)
	if err != nil {
		return errs.Wrap(err, errs.ClassConfig, "dns.node", "build request")
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("X-Rift-Node", n.cfg.NodeID)
	req.Header.Set("X-Rift-Location", n.cfg.Location)

	resp, err := n.client.Do(req)
	if err != nil {
		return errs.Wrap(err, errs.ClassNetwork, "dns.node", "post batch")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return errs.New(errs.ClassNetwork, "dns.node", fmt.Sprintf("hub returned %d", resp.StatusCode))
	}
	return nil
}

// WireObservation is the JSON shape sent to the hub (/v1/ingest). It is
// exported so the hub can decode the identical shape without duplication.
type WireObservation struct {
	NodeID    string       `json:"node_id"`
	Location  string       `json:"location"`
	View      uint8        `json:"view"`
	Resolver  string       `json:"resolver"`
	QName     string       `json:"qname"`
	QType     uint16       `json:"qtype"`
	RCode     uint8        `json:"rcode"`
	Answers   []WireAnswer `json:"answers"`
	Truncated bool         `json:"truncated"`
	Transport string       `json:"transport"`
	LatencyMS float64      `json:"latency_ms"`
	Timestamp string       `json:"timestamp"`
	ErrClass  uint8        `json:"err_class"`
}

// WireAnswer is one answer in the hub payload.
type WireAnswer struct {
	Name string `json:"name"`
	Type uint16 `json:"type"`
	TTL  uint32 `json:"ttl"`
	Data string `json:"data"`
}

func (n *Node) wireObservation(o dnsmodel.Observation) WireObservation {
	answers := make([]WireAnswer, 0, len(o.Answers))
	for _, a := range o.Answers {
		answers = append(answers, WireAnswer{Name: a.Name, Type: uint16(a.Type), TTL: a.TTL, Data: a.Data})
	}
	return WireObservation{
		NodeID:    o.NodeID,
		Location:  n.cfg.Location,
		View:      uint8(o.View),
		Resolver:  o.Resolver,
		QName:     o.QName,
		QType:     uint16(o.QType),
		RCode:     o.RCode,
		Answers:   answers,
		Truncated: o.Truncated,
		Transport: o.Transport,
		LatencyMS: float64(o.Latency.Microseconds()) / 1000,
		Timestamp: o.Timestamp.UTC().Format(time.RFC3339Nano),
		ErrClass:  uint8(o.ErrClass),
	}
}

// spool writes a batch to disk, bounded by SpoolMax. Returns false when
// spooling is disabled or would exceed the cap.
func (n *Node) spool(batch []dnsmodel.Observation) bool {
	if n.cfg.SpoolDir == "" {
		return false
	}
	data, err := json.Marshal(batch)
	if err != nil {
		return false
	}
	if int64(len(data)) > n.cfg.SpoolMax {
		n.metrics.DroppedSpool.Add(uint64(len(batch)))
		return false
	}

	n.spoolMu.Lock()
	defer n.spoolMu.Unlock()

	if err := os.MkdirAll(n.cfg.SpoolDir, 0o700); err != nil {
		return false
	}
	if n.metrics.SpoolBytes.Load()+int64(len(data)) > n.cfg.SpoolMax {
		n.metrics.DroppedSpool.Add(uint64(len(batch)))
		return false
	}
	// The filename carries a monotonic sequence as well as a timestamp:
	// two spools within the same clock tick would otherwise collide and one
	// would silently overwrite the other, making the byte accounting
	// disagree with what is actually on disk.
	seq := n.spoolSeq.Add(1)
	name := fmt.Sprintf("%s/spool-%s-%06d.json",
		n.cfg.SpoolDir, n.cfg.Now().UTC().Format("20060102T150405.000000000"), seq)
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return false
	}
	n.metrics.SpoolBytes.Add(int64(len(data)))
	n.metrics.Spooled.Add(uint64(len(batch)))
	return true
}

// DrainSpool sends spooled batches and removes the files on success. Called
// when connectivity returns.
func (n *Node) DrainSpool(ctx context.Context) error {
	if n.cfg.SpoolDir == "" {
		return nil
	}
	entries, err := os.ReadDir(n.cfg.SpoolDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := n.cfg.SpoolDir + "/" + e.Name()
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		var batch []dnsmodel.Observation
		if jerr := json.Unmarshal(raw, &batch); jerr != nil {
			os.Remove(path) // corrupt spool file: drop it, it is not data
			continue
		}
		if perr := n.postBatch(ctx, batch); perr != nil {
			return perr
		}
		if rmErr := os.Remove(path); rmErr == nil {
			n.metrics.SpoolBytes.Add(-int64(len(raw)))
		}
	}
	return nil
}
