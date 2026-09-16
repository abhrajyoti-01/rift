package resolver

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/abhrajyoti-01/rift/internal/dnsmon/wire"
	"github.com/abhrajyoti-01/rift/internal/platform/circuit"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
	"github.com/abhrajyoti-01/rift/internal/platform/netx"
)

var (
	errTransactionMismatch = errors.New("dns transaction mismatch")
	errQuestionMismatch    = errors.New("dns question mismatch")
)

// EngineConfig configures one engine bound to one upstream resolver.
type EngineConfig struct {
	Resolver    netip.AddrPort
	Concurrency int           // per-resolver worker cap, default 8
	Timeout     time.Duration // per attempt, default 2s
	EDNSUDPSize uint16        // default 1232
}

// Engine queries one upstream resolver.
type Engine struct {
	resolver    netip.AddrPort
	concurrency chan struct{}
	timeout     time.Duration
	ednsUDPSize uint16
	breaker     *circuit.Breaker
	guard       *netx.Guard
	now         func() time.Time
	queries     atomic.Uint64
	timeouts    atomic.Uint64
	truncated   atomic.Uint64
	inFlight    atomic.Int64
	rcodes      [16]atomic.Uint64
}

// Stats is a point-in-time snapshot of resolver health counters. RCodeCounts
// is indexed by the DNS RCODE and deliberately has a fixed size to avoid
// attacker-controlled metric cardinality.
type Stats struct {
	Queries     uint64
	Timeouts    uint64
	Truncated   uint64
	InFlight    int64
	RCodeCounts [16]uint64
	Breaker     string
}

const (
	defaultConcurrency = 8
	defaultTimeout     = 2 * time.Second
	defaultEDNSSize    = 1232
)

// NewEngine builds an engine. A nil clock uses time.Now.
func NewEngine(cfg EngineConfig, now func() time.Time) *Engine {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.EDNSUDPSize == 0 {
		cfg.EDNSUDPSize = defaultEDNSSize
	}
	if cfg.EDNSUDPSize < 512 {
		cfg.EDNSUDPSize = 512
	}
	if cfg.EDNSUDPSize > defaultEDNSSize {
		cfg.EDNSUDPSize = defaultEDNSSize
	}
	if now == nil {
		now = time.Now
	}
	guard, err := netx.NewGuard(netx.GuardOptions{Allow: []netip.Prefix{netip.PrefixFrom(cfg.Resolver.Addr(), cfg.Resolver.Addr().BitLen())}})
	if err != nil {
		// PrefixFrom with a valid Addr cannot fail. Keep construction total if
		// that invariant ever changes; Query will report the config failure.
		guard = nil
	}
	return &Engine{
		resolver:    cfg.Resolver,
		concurrency: make(chan struct{}, cfg.Concurrency),
		timeout:     cfg.Timeout,
		ednsUDPSize: cfg.EDNSUDPSize,
		breaker:     circuit.New(circuit.Config{}, now),
		guard:       guard,
		now:         now,
	}
}

// Query returns the final message (post TCP fallback if TC was set), the
// measured resolver latency, and a classified error. It never retries
// internally — scheduling owns retry.
func (e *Engine) Query(ctx context.Context, q wire.Question) (wire.Message, time.Duration, error) {
	if e == nil {
		return wire.Message{}, 0, errs.New(errs.ClassConfig, "dns.resolver.query", "nil engine")
	}
	if !e.resolver.IsValid() || e.resolver.Port() == 0 {
		return wire.Message{}, 0, errs.New(errs.ClassConfig, "dns.resolver.query", "invalid resolver address")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return wire.Message{}, 0, classifyContextError(err)
	}
	if !e.breaker.Allow() {
		return wire.Message{}, 0, errs.New(errs.ClassResource, "dns.resolver.query", "resolver circuit is open")
	}
	select {
	case e.concurrency <- struct{}{}:
		defer func() { <-e.concurrency }()
	default:
		return wire.Message{}, 0, errs.New(errs.ClassResource, "dns.resolver.query", "resolver concurrency limit reached")
	}

	e.queries.Add(1)
	e.inFlight.Add(1)
	defer e.inFlight.Add(-1)
	started := e.now()
	query := wire.EncodeQuery(q, e.ednsUDPSize)
	if len(query) == 0 {
		return wire.Message{}, 0, errs.New(errs.ClassConfig, "dns.resolver.query", "invalid DNS question")
	}

	udpCtx, udpCancel := context.WithTimeout(ctx, e.timeout)
	msg, err := e.queryUDP(udpCtx, query, q)
	udpCancel()
	if err == nil && msg.TC {
		e.truncated.Add(1)
		tcpCtx, tcpCancel := context.WithTimeout(ctx, e.timeout)
		msg, err = e.queryTCP(tcpCtx, q)
		tcpCancel()
	}
	latency := e.now().Sub(started)
	if latency < 0 {
		latency = 0
	}
	if err != nil {
		if errs.ClassOf(err) == errs.ClassTimeout {
			e.timeouts.Add(1)
		}
		if isBreakerFailure(err) {
			e.breaker.Failure()
		}
		return wire.Message{}, latency, err
	}
	e.breaker.Success()
	if msg.RCode < uint8(len(e.rcodes)) {
		e.rcodes[msg.RCode].Add(1)
	}
	return *msg, latency, nil
}

// ResolverAddr returns the upstream resolver this engine queries, as
// "ip:port". Used as the resolver identity on observations.
func (e *Engine) ResolverAddr() string {
	if e == nil {
		return ""
	}
	return e.resolver.String()
}

// Snapshot returns counters without exposing mutable atomics to callers.
func (e *Engine) Snapshot() Stats {
	var s Stats
	if e == nil {
		return s
	}
	s.Queries = e.queries.Load()
	s.Timeouts = e.timeouts.Load()
	s.Truncated = e.truncated.Load()
	s.InFlight = e.inFlight.Load()
	s.Breaker = e.breaker.State()
	for i := range s.RCodeCounts {
		s.RCodeCounts[i] = e.rcodes[i].Load()
	}
	return s
}

func (e *Engine) queryUDP(ctx context.Context, query []byte, q wire.Question) (*wire.Message, error) {
	if e.guard == nil {
		return nil, errs.New(errs.ClassConfig, "dns.resolver.udp", "outbound dial guard unavailable")
	}
	conn, err := e.guard.DialContext(ctx, "udp", e.resolver.String())
	if err != nil {
		return nil, wrapTransportError(ctx, err, "udp dial")
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, wrapTransportError(ctx, err, "udp write")
	}
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, wrapTransportError(ctx, err, "udp read")
		}
		msg, err := wire.DecodeMessageWithLimit(buf[:n], int(e.ednsUDPSize))
		if err != nil {
			return nil, errs.Wrap(err, errs.ClassSecurity, "dns.resolver.udp", "invalid response")
		}
		if err := validateResponse(msg, q, binary.BigEndian.Uint16(query[:2])); err != nil {
			// A connected UDP socket prevents unrelated source addresses, but a
			// stale or spoofed datagram can still carry a wrong ID/question.
			// Ignore it and keep reading until the per-attempt deadline.
			if isTransactionMismatch(err) {
				continue
			}
			return nil, err
		}
		return msg, nil
	}
}

func (e *Engine) queryTCP(ctx context.Context, q wire.Question) (*wire.Message, error) {
	query := wire.EncodeQuery(q, e.ednsUDPSize)
	if len(query) == 0 {
		return nil, errs.New(errs.ClassConfig, "dns.resolver.tcp", "invalid DNS question")
	}
	if e.guard == nil {
		return nil, errs.New(errs.ClassConfig, "dns.resolver.tcp", "outbound dial guard unavailable")
	}
	conn, err := e.guard.DialContext(ctx, "tcp", e.resolver.String())
	if err != nil {
		return nil, wrapTransportError(ctx, err, "tcp dial")
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(query)))
	if err := writeFull(conn, length[:]); err != nil {
		return nil, wrapTransportError(ctx, err, "tcp length write")
	}
	if err := writeFull(conn, query); err != nil {
		return nil, wrapTransportError(ctx, err, "tcp query write")
	}
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		return nil, wrapTransportError(ctx, err, "tcp length read")
	}
	responseLen := int(binary.BigEndian.Uint16(length[:]))
	if responseLen < 12 || responseLen > 65535 {
		return nil, errs.New(errs.ClassSecurity, "dns.resolver.tcp", "invalid response length")
	}
	response := make([]byte, responseLen)
	if _, err := io.ReadFull(conn, response); err != nil {
		return nil, wrapTransportError(ctx, err, "tcp response read")
	}
	msg, err := wire.DecodeMessageWithLimit(response, 65535)
	if err != nil {
		return nil, errs.Wrap(err, errs.ClassSecurity, "dns.resolver.tcp", "invalid response")
	}
	if err := validateResponse(msg, q, binary.BigEndian.Uint16(query[:2])); err != nil {
		return nil, err
	}
	return msg, nil
}

func validateResponse(msg *wire.Message, q wire.Question, id uint16) error {
	if !msg.QR {
		return errs.New(errs.ClassSecurity, "dns.resolver.response", "response QR flag is not set")
	}
	if msg.ID != id {
		return errs.Wrap(errTransactionMismatch, errs.ClassSecurity, "dns.resolver.response", "transaction ID mismatch")
	}
	if len(msg.Question) != 1 || !sameQuestion(msg.Question[0], q) {
		return errs.Wrap(errQuestionMismatch, errs.ClassSecurity, "dns.resolver.response", "question mismatch")
	}
	return nil
}

func sameQuestion(a, b wire.Question) bool {
	return normalizeName(a.Name) == normalizeName(b.Name) && a.Type == b.Type && (a.Class == b.Class || a.Class == 0 || b.Class == 0)
}

func normalizeName(name string) string {
	if name == "." {
		return "."
	}
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

func wrapTransportError(ctx context.Context, err error, op string) error {
	if ctx.Err() != nil {
		return classifyContextError(ctx.Err())
	}
	// Preserve classifications from the shared dial guard. In particular, an
	// SSRF denial must remain ClassSecurity and must never become retryable
	// merely because it crossed this transport boundary.
	if class := errs.ClassOf(err); class != errs.ClassUnknown {
		return err
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return errs.Wrap(err, errs.ClassTimeout, "dns.resolver", op+" timed out")
	}
	if errors.Is(err, io.EOF) {
		return errs.Wrap(err, errs.ClassPeerClosed, "dns.resolver", op+" peer closed")
	}
	return errs.Wrap(err, errs.ClassNetwork, "dns.resolver", op+" failed")
}

func classifyContextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(err, errs.ClassTimeout, "dns.resolver", "query deadline exceeded")
	}
	return errs.Wrap(err, errs.ClassNetwork, "dns.resolver", "query canceled")
}

func isBreakerFailure(err error) bool {
	switch errs.ClassOf(err) {
	case errs.ClassNetwork, errs.ClassTimeout:
		return true
	default:
		return false
	}
}

func isTransactionMismatch(err error) bool {
	return errs.ClassOf(err) == errs.ClassSecurity &&
		(errors.Is(err, errTransactionMismatch) || errors.Is(err, errQuestionMismatch))
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
