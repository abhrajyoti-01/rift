package picker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"

	lbmodel "github.com/abhrajyoti-01/rift/internal/lb/model"
)

// ErrNoUpstream is returned when no healthy backend exists in a pool.
var ErrNoUpstream = errors.New("lb: no healthy upstream")

// PickHint carries only what pickers may need; v1 pickers read backend
// state directly and ignore the hint.
type PickHint struct {
	SourceIP netip.Addr
}

// Picker selects a backend for one request or connection.
type Picker interface {
	Pick(ctx context.Context, hint PickHint) (*lbmodel.Backend, error)
}

// Picker kind identifiers. These are the closed set accepted by New and
// referenced by configuration.
const (
	RoundRobin = "round_robin"
	WeightedRR = "weighted_round_robin"
	LeastConns = "least_connections"
)

// New builds from config; kind ∈ round_robin | weighted_round_robin |
// least_connections. Backends are shared with the pool snapshot and
// read-only after construction.
func New(kind string, pool *lbmodel.Pool) (Picker, error) {
	if pool == nil {
		return nil, fmt.Errorf("picker: nil pool")
	}
	switch kind {
	case RoundRobin, "":
		return &roundRobin{backends: pool.Backends}, nil
	case WeightedRR:
		return newSmoothWRR(pool.Backends)
	case LeastConns:
		return &leastConns{backends: pool.Backends}, nil
	default:
		return nil, fmt.Errorf("picker: unknown kind %q", kind)
	}
}

// healthy skips unhealthy backends without allocating. Returns nil when
// all are unhealthy.
func healthy(b *lbmodel.Backend) bool { return b.Health.Load() }

// roundRobin: single atomic counter, index modulo healthy count,
// unhealthy skipped without allocation.
type roundRobin struct {
	backends []*lbmodel.Backend
	counter  atomic.Uint64
}

func (r *roundRobin) Pick(ctx context.Context, hint PickHint) (*lbmodel.Backend, error) {
	_ = hint
	n := uint64(len(r.backends))
	if n == 0 {
		return nil, ErrNoUpstream
	}
	// Bounded scan: advance at most n slots; if all unhealthy, no pick.
	start := r.counter.Add(1)
	for i := uint64(0); i < n; i++ {
		b := r.backends[(start+i)%n]
		if healthy(b) {
			return b, nil
		}
	}
	return nil, ErrNoUpstream
}

// smoothWRR is nginx-style smooth weighted round-robin: cw[i] += w[i];
// pick argmax(cw); cw[arg] -= total. Interleaves heavy backends (no burst
// of k consecutive picks for weight k), max-min fair. One mutex over the
// small integer slice; contention is measured — a sharded variant replaces
// it only on superlinear scaling evidence.
type smoothWRR struct {
	mu       sync.Mutex
	backends []*lbmodel.Backend
	cw       []int64
	total    int64
}

func newSmoothWRR(backends []*lbmodel.Backend) (*smoothWRR, error) {
	p := &smoothWRR{backends: backends, cw: make([]int64, len(backends))}
	for _, b := range backends {
		if b.Weight < 1 {
			return nil, fmt.Errorf("picker: backend %q weight %d < 1", b.ID, b.Weight)
		}
		p.total += int64(b.Weight)
	}
	if p.total == 0 {
		return nil, ErrNoUpstream
	}
	return p, nil
}

func (s *smoothWRR) Pick(ctx context.Context, hint PickHint) (*lbmodel.Backend, error) {
	_ = hint
	s.mu.Lock()
	defer s.mu.Unlock()

	// Pass 1: accumulate weights for healthy backends only; track the
	// effective total so a fully-drained denominator cannot be zero.
	var effTotal int64
	for i, b := range s.backends {
		if healthy(b) {
			s.cw[i] += int64(b.Weight)
			effTotal += int64(b.Weight)
		}
	}
	if effTotal == 0 {
		return nil, ErrNoUpstream
	}

	// Pass 2: argmax.
	var best int = -1
	for i := range s.backends {
		if !healthy(s.backends[i]) {
			continue
		}
		if best < 0 || s.cw[i] > s.cw[best] {
			best = i
		}
	}
	// best ≥ 0 because effTotal > 0.
	s.cw[best] -= effTotal
	return s.backends[best], nil
}

// leastConns: scan for min in-flight among healthy; ties rotate by
// round-robin counter so equal-load backends spread. No lock — Conns is
// atomic per backend. Two passes, zero allocation: pass 1 finds the min
// and candidate count; pass 2 selects the (counter mod count)-th
// candidate. Between passes Conns may shift (other goroutines
// connect/close); if the selected candidate vanishes, fall back to the
// first seen — never a false ErrNoUpstream.
type leastConns struct {
	backends []*lbmodel.Backend
	counter  atomic.Uint64
}

func (l *leastConns) Pick(ctx context.Context, hint PickHint) (*lbmodel.Backend, error) {
	_ = hint
	n := len(l.backends)
	if n == 0 {
		return nil, ErrNoUpstream
	}

	var min int64 = -1
	var candidates int
	for _, b := range l.backends {
		if !healthy(b) {
			continue
		}
		c := b.Conns.Load()
		switch {
		case min < 0 || c < min:
			min, candidates = c, 1
		case c == min:
			candidates++
		}
	}
	if min < 0 {
		return nil, ErrNoUpstream
	}

	sel := int(l.counter.Add(1)-1) % candidates
	var seen int
	var first *lbmodel.Backend
	for _, b := range l.backends {
		if !healthy(b) || b.Conns.Load() != min {
			continue
		}
		if first == nil {
			first = b
		}
		if seen == sel {
			return b, nil
		}
		seen++
	}
	// Candidate set shrank mid-pick; the first-seen minimum is still a
	// correct least-connections choice.
	if first != nil {
		return first, nil
	}
	return nil, ErrNoUpstream
}
