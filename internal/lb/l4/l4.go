package l4

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	lbmodel "github.com/abhrajyoti-01/rift/internal/lb/model"
	"github.com/abhrajyoti-01/rift/internal/lb/picker"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// Config sets forwarding behavior. Splice selects the dst.ReadFrom(src)
// fast path; the pooled 8KiB copy is the always-compiled fallback and the
// default.
type Config struct {
	// PoolID is the pool this forwarder serves. One Forwarder serves one
	// listener, so the pool is fixed at construction rather than guessed
	// per connection.
	PoolID       lbmodel.PoolID
	DialTimeout  time.Duration // default 3s
	IdleTimeout  time.Duration // default 60s, per-direction
	CopyDeadline time.Duration // default 30s, reset per Read/Write
	MaxConns     int           // per listener; admission gate
	MaxRetries   int           // dial-phase repicks, distinct backends
	Splice       bool
}

const (
	defaultDialTimeout  = 3 * time.Second
	defaultIdleTimeout  = 60 * time.Second
	defaultCopyDeadline = 30 * time.Second
	defaultMaxRetries   = 2
	copyBufSize         = 8 << 10
)

func (c Config) withDefaults() Config {
	if c.DialTimeout <= 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = defaultIdleTimeout
	}
	if c.CopyDeadline <= 0 {
		c.CopyDeadline = defaultCopyDeadline
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = defaultMaxRetries
	}
	return c
}

// Metrics is the L4 accounting surface.
type Metrics struct {
	Accepted      atomic.Uint64
	Rejected      atomic.Uint64
	Closed        atomic.Uint64
	UpstreamBytes atomic.Uint64
	DownstreamBytes atomic.Uint64
	DialFailures  atomic.Uint64
	ActiveConns   atomic.Int64
}

// Forwarder proxies one listener's connections to its pool.
type Forwarder struct {
	cfg     Config
	snap    func() *lbmodel.Snapshot
	pools   sync.Map // lbmodel.PoolID → picker.Picker
	metrics Metrics
	// buffer pool for the non-splice copy path. Only used when Splice is
	// false; a pool that is not needed costs readability, so it is a field
	// rather than a package-global.
	bufPool sync.Pool
}

// New builds a Forwarder. snap must read the atomic.Pointer[Snapshot] in
// lb/control — the data plane never takes a lock for a pick.
func New(cfg Config, snap func() *lbmodel.Snapshot) *Forwarder {
	f := &Forwarder{cfg: cfg.withDefaults(), snap: snap}
	f.bufPool.New = func() any { b := make([]byte, copyBufSize); return &b }
	return f
}

// Metrics returns the accounting surface.
func (f *Forwarder) Metrics() *Metrics { return &f.metrics }

// Serve accepts on l until ctx is canceled, one goroutine per connection.
func (f *Forwarder) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			// Transient accept error (EMFILE etc.): report and continue
			// rather than killing the listener.
			return errs.Wrap(err, errs.ClassResource, "lb.l4.accept", "accept failed")
		}

		// Admission: refuse immediately rather than queue. Queuing
		// admissions converts overload into latency instead of a visible
		// refusal (LOAD_BALANCER_SPEC §3.1).
		if f.cfg.MaxConns > 0 && f.metrics.ActiveConns.Load() >= int64(f.cfg.MaxConns) {
			f.metrics.Rejected.Add(1)
			conn.Close()
			continue
		}

		f.metrics.Accepted.Add(1)
		f.metrics.ActiveConns.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer f.metrics.ActiveConns.Add(-1)
			defer f.metrics.Closed.Add(1)
			f.handle(ctx, conn)
		}()
	}
}

// handle proxies one accepted connection.
func (f *Forwarder) handle(ctx context.Context, client net.Conn) {
	defer client.Close()

	if tc, ok := client.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	pool := f.currentPool()
	if pool == nil {
		return
	}
	pk, err := f.pickerFor(pool)
	if err != nil {
		return
	}

	// Dial with connect-phase repicks onto distinct backends; zero bytes
	// have reached any backend yet, so this retry is safe for every
	// protocol.
	var upstream net.Conn
	var chosen *lbmodel.Backend
	tried := map[string]bool{}
	for attempt := 0; attempt <= f.cfg.MaxRetries; attempt++ {
		b, perr := pk.Pick(ctx, picker.PickHint{})
		if perr != nil {
			break
		}
		if tried[b.ID] {
			// Exhausted distinct backends.
			break
		}
		tried[b.ID] = true

		dctx, cancel := context.WithTimeout(ctx, f.cfg.DialTimeout)
		conn, derr := (&net.Dialer{}).DialContext(dctx, "tcp", b.Addr)
		cancel()
		if derr != nil {
			f.metrics.DialFailures.Add(1)
			continue
		}
		upstream = conn
		chosen = b
		break
	}
	if upstream == nil {
		return
	}
	defer upstream.Close()
	chosen.Conns.Add(1)
	defer chosen.Conns.Add(-1)

	if tc, ok := upstream.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	f.proxy(client, upstream)
}

// currentPool returns the pool this forwarder serves, read from the current
// snapshot. A reload may replace the pool contents; the identity stays.
func (f *Forwarder) currentPool() *lbmodel.Pool {
	snap := f.snapFn()
	if snap == nil {
		return nil
	}
	if f.cfg.PoolID != "" {
		return snap.Pools[f.cfg.PoolID]
	}
	// No pool configured: unambiguous only for a single-pool snapshot.
	if len(snap.Pools) == 1 {
		for _, p := range snap.Pools {
			return p
		}
	}
	return nil
}

func (f *Forwarder) snapFn() *lbmodel.Snapshot {
	if f.snap == nil {
		return nil
	}
	return f.snap()
}

// pickerFor returns a picker for the pool. Pickers are keyed by pool
// generation: a reload that changes pool membership replaces the picker so
// the algorithm never sees backends from two configurations.
func (f *Forwarder) pickerFor(pool *lbmodel.Pool) (picker.Picker, error) {
	if p, ok := f.pools.Load(pool.ID); ok {
		if pk := p.(poolPicker); pk.generation == poolGeneration(pool) {
			return pk.picker, nil
		}
	}
	kind := pool.Picker
	if kind == "" {
		kind = picker.RoundRobin
	}
	pk, err := picker.New(kind, pool)
	if err != nil {
		return nil, err
	}
	f.pools.Store(pool.ID, poolPicker{picker: pk, generation: poolGeneration(pool)})
	return pk, nil
}

type poolPicker struct {
	picker     picker.Picker
	generation uint64
}

// poolGeneration changes whenever the pool's backend membership or weights
// change, so a stale picker is never reused across a reload.
func poolGeneration(pool *lbmodel.Pool) uint64 {
	h := uint64(1469598103934665603)
	for _, b := range pool.Backends {
		for i := 0; i < len(b.ID); i++ {
			h = (h ^ uint64(b.ID[i])) * 1099511628211
		}
		h = (h ^ uint64(b.Weight)) * 1099511628211
	}
	return h
}

// proxy copies bidirectionally with half-close propagation: on client EOF
// we CloseWrite upstream and keep draining the response until upstream EOF
// or the idle deadline. Skipping the drain truncates large responses sent
// after a request body finishes.
func (f *Forwarder) proxy(client, upstream net.Conn) {
	done := make(chan struct{}, 2)

	go func() {
		n := f.copyDirection(upstream, client, true)
		if cw, ok := upstream.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			upstream.Close()
		}
		f.metrics.UpstreamBytes.Add(uint64(n))
		done <- struct{}{}
	}()

	go func() {
		n := f.copyDirection(client, upstream, false)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			client.Close()
		}
		f.metrics.DownstreamBytes.Add(uint64(n))
		done <- struct{}{}
	}()

	<-done
	<-done
}

// copyDirection copies src→dst, returning bytes copied.
//
// Two modes:
//   - Splice: dst.ReadFrom(src) takes the runtime zero-copy fast path
//     (splice on Linux for socket→socket). One syscall per chunk instead of
//     read+write, and no userspace buffer. Trade-off: the read deadline is
//     set once for the whole copy, so a stalled peer is detected at the
//     idle timeout rather than the finer per-operation copy deadline.
//   - Pooled copy: an 8 KiB buffer reused for the connection's lifetime,
//     with a deadline reset on every read and write. Finer stall detection
//     at the cost of a copy through userspace.
//
// idle selects the read deadline: true uses the idle timeout (no data
// flowing is fine up to that bound), false uses the copy deadline (a peer
// that stops mid-transfer is cut loose sooner).
func (f *Forwarder) copyDirection(dst, src net.Conn, idle bool) int64 {
	if f.cfg.Splice {
		if rf, ok := dst.(io.ReaderFrom); ok {
			timeout := f.cfg.CopyDeadline
			if idle {
				timeout = f.cfg.IdleTimeout
			}
			src.SetReadDeadline(time.Now().Add(timeout))
			dst.SetWriteDeadline(time.Now().Add(timeout))
			n, err := rf.ReadFrom(src)
			if err == nil || errors.Is(err, io.EOF) {
				return n
			}
			// A deadline or peer error ends the direction; bytes already
			// moved are still accounted.
			return n
		}
	}

	bp := f.bufPool.Get().(*[]byte)
	defer f.bufPool.Put(bp)
	buf := *bp

	var total int64
	for {
		timeout := f.cfg.CopyDeadline
		if idle {
			timeout = f.cfg.IdleTimeout
		}
		src.SetReadDeadline(time.Now().Add(timeout))
		n, rerr := src.Read(buf)
		if n > 0 {
			dst.SetWriteDeadline(time.Now().Add(f.cfg.CopyDeadline))
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total
			}
			total += int64(n)
		}
		if rerr != nil {
			return total
		}
	}
}
