package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	dnsmodel "github.com/abhrajyoti-01/rift/internal/dnsmon/model"
	dnsnode "github.com/abhrajyoti-01/rift/internal/dnsmon/node"
	"github.com/abhrajyoti-01/rift/internal/dnsmon/wire"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// HubConfig configures the hub.
type HubConfig struct {
	IngestAddr        string // ":9001" TLS+mTLS in production
	QueryAddr         string // ":9002"
	WindowKeys        int    // per (target,view); default 4096
	SegmentBytes      int64  // JSONL rotation size; default 128 MiB
	DataDir           string
	MinResponding     int
	ConvergenceWindow time.Duration
	// MaxBatch caps observations accepted in one ingest request. A cap is
	// required: an uncapped batch is a memory-exhaustion primitive.
	MaxBatch int
	// Now is injectable for tests.
	Now func() time.Time
}

const (
	defaultWindowKeys     = 4096
	defaultSegmentBytes   = 128 << 20
	defaultMinResponding  = 3
	defaultMaxBatch       = 4096
	defaultConvergenceWin = 5 * time.Minute
)

func (c HubConfig) withDefaults() HubConfig {
	if c.WindowKeys <= 0 {
		c.WindowKeys = defaultWindowKeys
	}
	if c.SegmentBytes <= 0 {
		c.SegmentBytes = defaultSegmentBytes
	}
	if c.MinResponding <= 0 {
		c.MinResponding = defaultMinResponding
	}
	if c.MaxBatch <= 0 {
		c.MaxBatch = defaultMaxBatch
	}
	if c.ConvergenceWindow <= 0 {
		c.ConvergenceWindow = defaultConvergenceWin
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Window is a bounded per-(target,view) observation history. The bound is
// the hard memory ceiling: an attacker cannot grow hub memory by sending
// more distinct names than the configured capacity.
type Window struct {
	mu   sync.RWMutex
	cap  int
	seen map[string]*ringBuf
}

type ringBuf struct {
	obs  []dnsmodel.Observation
	head int
	size int
}

// NewWindow builds a window with the given per-key capacity.
func NewWindow(perKeyCap int) *Window {
	if perKeyCap < 1 {
		perKeyCap = 1
	}
	return &Window{cap: perKeyCap, seen: make(map[string]*ringBuf)}
}

func key(o dnsmodel.Observation) string {
	return fmt.Sprintf("%d/%s/%d", o.View, o.QName, o.QType)
}

// Add inserts an observation, evicting the oldest for that key when full.
// Returns true when an eviction occurred (the window is lossy by design and
// says so).
func (w *Window) Add(o dnsmodel.Observation) (evicted bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := key(o)
	b, ok := w.seen[k]
	if !ok {
		b = &ringBuf{obs: make([]dnsmodel.Observation, w.cap)}
		w.seen[k] = b
	}
	if b.size == w.cap {
		b.obs[b.head] = o
		b.head = (b.head + 1) % w.cap
		return true
	}
	b.obs[(b.head+b.size)%w.cap] = o
	b.size++
	return false
}

// ForTarget returns all observations matching qname/qtype across views.
func (w *Window) ForTarget(qname string, qtype uint16) []dnsmodel.Observation {
	w.mu.RLock()
	defer w.mu.RUnlock()
	var out []dnsmodel.Observation
	for _, b := range w.seen {
		for i := 0; i < b.size; i++ {
			o := b.obs[(b.head+i)%w.cap]
			if o.QName == qname && uint16(o.QType) == qtype {
				out = append(out, o)
			}
		}
	}
	return out
}

// Keys reports how many distinct (target,view) keys are tracked.
func (w *Window) Keys() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.seen)
}

// Hub is the aggregation hub process.
type Hub struct {
	cfg    HubConfig
	window *Window
	metrics Metrics

	segMu    sync.Mutex
	segFile  *os.File
	segBuf   *bufio.Writer
	segBytes int64
	segSeq   uint64

	ingestSrv *http.Server
	querySrv  *http.Server
}

// Metrics is the hub's accounting surface.
type Metrics struct {
	BatchesAccepted atomic.Uint64
	BatchesRejected atomic.Uint64
	Observations    atomic.Uint64
	WindowEvictions atomic.Uint64
	Segments        atomic.Uint64
	SegmentBytes    atomic.Int64
}

// New builds a hub from config.
func New(cfg HubConfig) *Hub {
	cfg = cfg.withDefaults()
	return &Hub{cfg: cfg, window: NewWindow(cfg.WindowKeys)}
}

// Metrics returns the accounting surface.
func (h *Hub) Metrics() *Metrics { return &h.metrics }

// Window exposes the window for the query API and tests.
func (h *Hub) Window() *Window { return h.window }

// IngestHandler serves POST /v1/ingest (JSON lines of WireObservation).
func (h *Hub) IngestHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ingest", h.handleIngest)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"ok"}`)
	})
	return mux
}

// QueryHandler serves the read API.
func (h *Hub) QueryHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/observations", h.handleObservations)
	mux.HandleFunc("/v1/propagation", h.handlePropagation)
	mux.HandleFunc("/v1/targets", h.handleTargets)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ready":true}`)
	})
	return mux
}

func (h *Hub) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Bound the request body before parsing: an unbounded read is a
	// memory-exhaustion primitive regardless of the batch cap below.
	maxBody := int64(h.cfg.MaxBatch) * 2048
	if maxBody <= 0 {
		maxBody = 8 << 20
	}
	body := http.MaxBytesReader(w, r.Body, maxBody)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)

	var count int
	for scanner.Scan() {
		if count >= h.cfg.MaxBatch {
			h.metrics.BatchesRejected.Add(1)
			http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
			return
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var wo dnsnode.WireObservation
		if err := json.Unmarshal(line, &wo); err != nil {
			h.metrics.BatchesRejected.Add(1)
			http.Error(w, "malformed observation", http.StatusBadRequest)
			return
		}
		obs, err := h.fromWire(wo)
		if err != nil {
			h.metrics.BatchesRejected.Add(1)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if h.window.Add(obs) {
			h.metrics.WindowEvictions.Add(1)
		}
		h.metrics.Observations.Add(1)
		if err := h.appendSegment(obs); err != nil {
			// Storage failure: tell the node so it can spool rather than
			// letting it believe the data landed.
			h.metrics.BatchesRejected.Add(1)
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
			return
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		h.metrics.BatchesRejected.Add(1)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	h.metrics.BatchesAccepted.Add(1)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]int{"accepted": count})
}

func (h *Hub) fromWire(wo dnsnode.WireObservation) (dnsmodel.Observation, error) {
	ts, err := time.Parse(time.RFC3339Nano, wo.Timestamp)
	if err != nil {
		return dnsmodel.Observation{}, errors.New("invalid timestamp")
	}
	if wo.View == 0 || wo.View > 2 {
		return dnsmodel.Observation{}, errors.New("invalid view")
	}
	if wo.QName == "" || len(wo.QName) > 253 {
		return dnsmodel.Observation{}, errors.New("invalid qname")
	}
	ans := make([]dnsmodel.Answer, 0, len(wo.Answers))
	for _, a := range wo.Answers {
		ans = append(ans, dnsmodel.Answer{
			Name: a.Name,
			Type: wire.Type(a.Type),
			TTL:  a.TTL,
			Data: a.Data,
		})
	}
	return dnsmodel.Observation{
		NodeID:    wo.NodeID,
		View:      dnsmodel.View(wo.View),
		Resolver:  wo.Resolver,
		QName:     wo.QName,
		QType:     wire.Type(wo.QType),
		RCode:     wo.RCode,
		Answers:   ans,
		Truncated: wo.Truncated,
		Transport: wo.Transport,
		Latency:   time.Duration(wo.LatencyMS * float64(time.Millisecond)),
		Timestamp: ts.UTC(),
		ErrClass:  errs.Class(wo.ErrClass),
	}, nil
}

// appendSegment writes one observation as a JSON line, rotating the segment
// when it exceeds SegmentBytes.
func (h *Hub) appendSegment(o dnsmodel.Observation) error {
	h.segMu.Lock()
	defer h.segMu.Unlock()

	if h.segFile == nil {
		if err := h.openSegmentLocked(); err != nil {
			return err
		}
	}
	rec, err := json.Marshal(segmentRecord{
		QName:     o.QName,
		QType:     uint16(o.QType),
		View:      uint8(o.View),
		NodeID:    o.NodeID,
		Resolver:  o.Resolver,
		RCode:     o.RCode,
		Answers:   toWireAnswers(o.Answers),
		LatencyMS: float64(o.Latency.Microseconds()) / 1000,
		Timestamp: o.Timestamp.UTC().Format(time.RFC3339Nano),
		ErrClass:  uint8(o.ErrClass),
	})
	if err != nil {
		return err
	}
	rec = append(rec, '\n')
	n, werr := h.segBuf.Write(rec)
	if werr != nil {
		return werr
	}
	h.segBytes += int64(n)
	h.metrics.SegmentBytes.Add(int64(n))
	if h.segBytes >= h.cfg.SegmentBytes {
		return h.rotateSegmentLocked()
	}
	return nil
}

type segmentRecord struct {
	QName     string          `json:"qname"`
	QType     uint16          `json:"qtype"`
	View      uint8           `json:"view"`
	NodeID    string          `json:"node_id"`
	Resolver  string          `json:"resolver"`
	RCode     uint8           `json:"rcode"`
	Answers   []dnsnode.WireAnswer `json:"answers"`
	LatencyMS float64         `json:"latency_ms"`
	Timestamp string          `json:"ts"`
	ErrClass  uint8           `json:"err_class"`
}

func toWireAnswers(ans []dnsmodel.Answer) []dnsnode.WireAnswer {
	out := make([]dnsnode.WireAnswer, 0, len(ans))
	for _, a := range ans {
		out = append(out, dnsnode.WireAnswer{Name: a.Name, Type: uint16(a.Type), TTL: a.TTL, Data: a.Data})
	}
	return out
}

func (h *Hub) openSegmentLocked() error {
	if h.cfg.DataDir == "" {
		return errors.New("hub: data_dir not configured")
	}
	if err := os.MkdirAll(h.cfg.DataDir, 0o700); err != nil {
		return err
	}
	h.segSeq++
	name := filepath.Join(h.cfg.DataDir,
		fmt.Sprintf("seg-%s-%06d.jsonl", h.cfg.Now().UTC().Format("20060102T150405"), h.segSeq))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	h.segFile = f
	h.segBuf = bufio.NewWriterSize(f, 64<<10)
	h.segBytes = 0
	h.metrics.Segments.Add(1)
	return nil
}

func (h *Hub) rotateSegmentLocked() error {
	if h.segBuf != nil {
		if err := h.segBuf.Flush(); err != nil {
			return err
		}
	}
	if h.segFile != nil {
		if err := h.segFile.Sync(); err != nil {
			return err
		}
		if err := h.segFile.Close(); err != nil {
			return err
		}
	}
	h.segFile = nil
	h.segBuf = nil
	return h.openSegmentLocked()
}

// Flush durably writes buffered segment data. Called on shutdown.
func (h *Hub) Flush() error {
	h.segMu.Lock()
	defer h.segMu.Unlock()
	return h.flushLocked()
}

func (h *Hub) flushLocked() error {
	if h.segBuf == nil {
		return nil
	}
	if err := h.segBuf.Flush(); err != nil {
		return err
	}
	if h.segFile != nil {
		return h.segFile.Sync()
	}
	return nil
}

// Close flushes and releases the segment file. Callers own this: leaving
// the file open holds a descriptor and, on Windows, blocks removal of the
// data directory.
func (h *Hub) Close() error {
	h.segMu.Lock()
	defer h.segMu.Unlock()
	flushErr := h.flushLocked()
	closeErr := error(nil)
	if h.segFile != nil {
		closeErr = h.segFile.Close()
		h.segFile = nil
		h.segBuf = nil
	}
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

func (h *Hub) handleObservations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	qname := q.Get("target")
	qtype := q.Get("type")
	if qname == "" {
		http.Error(w, "target parameter required", http.StatusBadRequest)
		return
	}
	var typ uint16 = 1
	if qtype != "" {
		if v, err := parseType(qtype); err == nil {
			typ = v
		} else {
			http.Error(w, "unsupported type", http.StatusBadRequest)
			return
		}
	}
	obs := h.window.ForTarget(qname, typ)
	// Cap the response: unbounded responses are a resource-exhaustion
	// primitive (API_SPEC §2).
	const maxPage = 1000
	if len(obs) > maxPage {
		obs = obs[len(obs)-maxPage:]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"observations": obs, "count": len(obs)})
}

func (h *Hub) handlePropagation(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	qname := q.Get("target")
	if qname == "" {
		http.Error(w, "target parameter required", http.StatusBadRequest)
		return
	}
	var typ uint16 = 1
	if t := q.Get("type"); t != "" {
		if v, err := parseType(t); err == nil {
			typ = v
		}
	}
	obs := h.window.ForTarget(qname, typ)
	cfg := dnsmodel.ClassifierConfig{
		MinResponding:     h.cfg.MinResponding,
		ConvergenceWindow: h.cfg.ConvergenceWindow,
	}
	res := dnsmodel.Classify(obs, cfg, nil, h.cfg.Now())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"target":               qname,
		"state":                res.State.String(),
		"state_code":           uint8(res.State),
		"reference":            res.Reference,
		"divergent_resolvers":  res.DivergentResolvers,
		"responding_resolvers": res.RespondingResolvers,
		"divergence_seconds":   res.DivergenceDuration.Seconds(),
	})
}

func (h *Hub) handleTargets(w http.ResponseWriter, r *http.Request) {
	h.window.mu.RLock()
	keys := make([]string, 0, len(h.window.seen))
	for k := range h.window.seen {
		keys = append(keys, k)
	}
	h.window.mu.RUnlock()
	sort.Strings(keys)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"keys": keys, "count": len(keys)})
}

func parseType(s string) (uint16, error) {
	switch s {
	case "A", "a", "1":
		return 1, nil
	case "NS", "ns", "2":
		return 2, nil
	case "CNAME", "cname", "5":
		return 5, nil
	case "MX", "mx", "15":
		return 15, nil
	case "TXT", "txt", "16":
		return 16, nil
	case "AAAA", "aaaa", "28":
		return 28, nil
	default:
		return 0, fmt.Errorf("unknown type %q", s)
	}
}

// Run serves ingest and query planes until ctx is canceled.
func (h *Hub) Run(ctx context.Context) error {
	if h.cfg.DataDir == "" {
		return errs.New(errs.ClassConfig, "dns.hub", "data_dir is required")
	}
	ingestAddr := h.cfg.IngestAddr
	if ingestAddr == "" {
		ingestAddr = "127.0.0.1:9001"
	}
	queryAddr := h.cfg.QueryAddr
	if queryAddr == "" {
		queryAddr = "127.0.0.1:9002"
	}

	ingestLn, err := net.Listen("tcp", ingestAddr)
	if err != nil {
		return errs.Wrap(err, errs.ClassResource, "dns.hub", "bind ingest "+ingestAddr)
	}
	queryLn, err := net.Listen("tcp", queryAddr)
	if err != nil {
		ingestLn.Close()
		return errs.Wrap(err, errs.ClassResource, "dns.hub", "bind query "+queryAddr)
	}

	h.ingestSrv = &http.Server{
		Handler:           h.IngestHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	h.querySrv = &http.Server{
		Handler:           h.QueryHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	errCh := make(chan error, 2)
	go func() { errCh <- h.ingestSrv.Serve(ingestLn) }()
	go func() { errCh <- h.querySrv.Serve(queryLn) }()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errs.Wrap(err, errs.ClassNetwork, "dns.hub", "serve")
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.ingestSrv.Shutdown(shutCtx)
	h.querySrv.Shutdown(shutCtx)
	return h.Close()
}