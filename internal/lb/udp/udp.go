package udp

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	lbmodel "github.com/abhrajyoti-01/rift/internal/lb/model"
	"github.com/abhrajyoti-01/rift/internal/lb/picker"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// SessionID is the hex(srcaddr)|hex(dstaddr) 5-tuple-normalized session key.
type SessionID string

// Session is one client-to-backend flow with one connected upstream socket
// and one reply goroutine.
type Session struct {
	ID       SessionID
	Upstream *net.UDPConn
	LastSeen atomic.Int64 // unix nanos
	UpPkts, UpBytes, DownPkts, DownBytes atomic.Uint64

	done chan struct{}
	once sync.Once
}

// close stops the reply goroutine and the upstream socket.
func (s *Session) close() {
	s.once.Do(func() {
		if s.done != nil {
			close(s.done)
		}
		if s.Upstream != nil {
			s.Upstream.Close()
		}
	})
}

// Config sets session-table behavior.
type Config struct {
	PoolID      lbmodel.PoolID
	MaxSessions int           // hard admission gate; default 4096
	SessionTTL  time.Duration // default 120s
	SweepEvery  time.Duration // default 30s
	DialTimeout time.Duration
}

const (
	defaultMaxSessions = 4096
	defaultSessionTTL  = 120 * time.Second
	defaultSweepEvery  = 30 * time.Second
	defaultDialTimeout = 3 * time.Second
	maxPacketSize      = 65535
)

func (c Config) withDefaults() Config {
	if c.MaxSessions <= 0 {
		c.MaxSessions = defaultMaxSessions
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = defaultSessionTTL
	}
	if c.SweepEvery <= 0 {
		c.SweepEvery = defaultSweepEvery
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = defaultDialTimeout
	}
	return c
}

// Metrics is the UDP accounting surface. Drop reasons are distinct so an
// attacker-caused flood is never conflated with ordinary idle expiry.
type Metrics struct {
	PacketsIn      atomic.Uint64
	PacketsOut     atomic.Uint64
	Sessions       atomic.Int64
	DroppedTable   atomic.Uint64 // table full
	DroppedSweep   atomic.Uint64 // expired
	DroppedDial    atomic.Uint64 // upstream unreachable
	DroppedNoRoute atomic.Uint64 // no healthy backend
}

// Server is the UDP proxy: one read loop on the shared listener socket.
type Server struct {
	cfg      Config
	snap     func() *lbmodel.Snapshot
	pk       atomic.Pointer[picker.Picker]
	listener atomic.Pointer[net.UDPConn]
	sessions sync.Map // SessionID → *Session
	metrics  Metrics
}

// New builds a Server.
func New(cfg Config, snap func() *lbmodel.Snapshot) *Server {
	return &Server{cfg: cfg.withDefaults(), snap: snap}
}

// Metrics returns the accounting surface.
func (s *Server) Metrics() *Metrics { return &s.metrics }

// Serve reads packets from conn until ctx is canceled, dispatching by
// session table, creating per-session upstream sockets and reply
// goroutines.
func (s *Server) Serve(ctx context.Context, conn *net.UDPConn) error {
	if err := s.ensurePicker(); err != nil {
		return err
	}
	s.listener.Store(conn)

	go s.sweepLoop(ctx)

	go func() {
		<-ctx.Done()
		conn.SetReadDeadline(time.Now())
		s.closeAll()
	}()

	buf := make([]byte, maxPacketSize)
	for {
		n, clientAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return errs.Wrap(err, errs.ClassNetwork, "lb.udp.read", "listener read")
		}
		s.metrics.PacketsIn.Add(1)

		// Copy the datagram: buf is reused on the next read.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		sess := s.sessionFor(ctx, clientAddr)
		if sess == nil {
			continue // dropped, already counted with a reason
		}
		sess.LastSeen.Store(time.Now().UnixNano())
		sess.UpPkts.Add(1)
		sess.UpBytes.Add(uint64(n))
		if _, werr := sess.Upstream.Write(pkt); werr != nil {
			s.metrics.DroppedDial.Add(1)
			s.remove(sess)
		}
	}
}

// sessionFor returns the session for this client, creating it if absent.
// Backend affinity is per session: re-picking mid-flow would break
// application flow state.
func (s *Server) sessionFor(ctx context.Context, client *net.UDPAddr) *Session {
	id := SessionID(client.String())
	if cur, ok := s.sessions.Load(id); ok {
		return cur.(*Session)
	}

	// Admission gate: a full table drops rather than growing without bound.
	if s.metrics.Sessions.Load() >= int64(s.cfg.MaxSessions) {
		s.metrics.DroppedTable.Add(1)
		return nil
	}

	pk := s.pk.Load()
	if pk == nil {
		s.metrics.DroppedNoRoute.Add(1)
		return nil
	}
	backend, err := (*pk).Pick(ctx, picker.PickHint{})
	if err != nil {
		s.metrics.DroppedNoRoute.Add(1)
		return nil
	}

	dctx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
	defer cancel()
	var d net.Dialer
	upstream, derr := d.DialContext(dctx, "udp", backend.Addr)
	if derr != nil {
		s.metrics.DroppedDial.Add(1)
		return nil
	}
	uc := upstream.(*net.UDPConn)

	sess := &Session{
		ID:       id,
		Upstream: uc,
		done:     make(chan struct{}),
	}
	sess.LastSeen.Store(time.Now().UnixNano())

	// Load-then-store: a race could create two sessions; the loser is
	// closed immediately so exactly one reply goroutine survives.
	actual, loaded := s.sessions.LoadOrStore(id, sess)
	if loaded {
		sess.close()
		return actual.(*Session)
	}
	s.metrics.Sessions.Add(1)
	go s.replyLoop(sess, client)
	return sess
}

// replyLoop forwards upstream responses back to the client until the
// session closes.
func (s *Server) replyLoop(sess *Session, client *net.UDPAddr) {
	buf := make([]byte, maxPacketSize)
	for {
		select {
		case <-sess.done:
			return
		default:
		}
		sess.Upstream.SetReadDeadline(time.Now().Add(s.cfg.SessionTTL))
		n, _, err := sess.Upstream.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return // idles out; the sweeper will reap it
			}
			return
		}
		if _, err := s.listenerWrite(client, buf[:n]); err != nil {
			return
		}
		sess.LastSeen.Store(time.Now().UnixNano())
		sess.DownPkts.Add(1)
		sess.DownBytes.Add(uint64(n))
		s.metrics.PacketsOut.Add(1)
	}
}

// listenerWrite sends a datagram back to the client on the shared
// client-facing socket.
func (s *Server) listenerWrite(client *net.UDPAddr, payload []byte) (int, error) {
	l := s.listener.Load()
	if l == nil {
		return 0, errors.New("udp: listener not set")
	}
	return l.WriteToUDP(payload, client)
}

// sweepLoop reaps sessions past their TTL.
func (s *Server) sweepLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-s.cfg.SessionTTL).UnixNano()
			s.sessions.Range(func(_, v any) bool {
				sess := v.(*Session)
				if sess.LastSeen.Load() < cutoff {
					s.remove(sess)
					s.metrics.DroppedSweep.Add(1)
				}
				return true
			})
		}
	}
}

func (s *Server) remove(sess *Session) {
	if _, loaded := s.sessions.LoadAndDelete(sess.ID); loaded {
		sess.close()
		s.metrics.Sessions.Add(-1)
	}
}

func (s *Server) closeAll() {
	s.sessions.Range(func(_, v any) bool {
		sess := v.(*Session)
		sess.close()
		s.metrics.Sessions.Add(-1)
		s.sessions.Delete(sess.ID)
		return true
	})
}

// ensurePicker resolves the picker once, from the configured pool.
func (s *Server) ensurePicker() error {
	if s.pk.Load() != nil {
		return nil
	}
	snap := s.snapshot()
	if snap == nil {
		return errs.New(errs.ClassResource, "lb.udp", "no snapshot available")
	}
	var pool *lbmodel.Pool
	if s.cfg.PoolID != "" {
		pool = snap.Pools[s.cfg.PoolID]
	} else if len(snap.Pools) == 1 {
		for _, p := range snap.Pools {
			pool = p
		}
	}
	if pool == nil {
		return errs.New(errs.ClassConfig, "lb.udp", "pool not found in snapshot")
	}
	kind := pool.Picker
	if kind == "" {
		kind = picker.RoundRobin
	}
	pk, err := picker.New(kind, pool)
	if err != nil {
		return err
	}
	s.pk.Store(&pk)
	return nil
}

func (s *Server) snapshot() *lbmodel.Snapshot {
	if s.snap == nil {
		return nil
	}
	return s.snap()
}
