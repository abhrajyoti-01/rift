package dnsmodel

import (
	"time"

	"github.com/abhrajyoti-01/rift/internal/dnsmon/wire"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

type View uint8

const (
	// ViewRecursive queries configured public resolvers with RD=1.
	ViewRecursive View = iota + 1
	// ViewAuthoritative queries zone NS servers with RD=0.
	ViewAuthoritative
)

// Target is one monitored (zone, name, type, view) tuple.
type Target struct {
	Zone string
	Name string
	Type wire.Type
	View View
}

// Answer is one canonicalized answer; sorted by (Name, Type, Data) in
// Observation.Answers.
type Answer struct {
	Name string
	Type wire.Type
	TTL  uint32
	Data string
}

// Observation is one node's one query result. Fingerprint (the comparison
// unit) is the sorted Answer.Data set — TTL deliberately excluded so TTL
// decay alone never reads as propagation divergence.
type Observation struct {
	NodeID    string
	View      View
	Resolver  string
	QName     string
	QType     wire.Type
	RCode     uint8
	Answers   []Answer
	Truncated bool
	Transport string // "udp" | "tcp"
	Latency   time.Duration
	Timestamp time.Time
	ErrClass  errs.Class
}

// PropagationState is the classifier output over the trailing window.
// Enum order is severity order for dashboards.
type PropagationState uint8

const (
	// PropStateUnknown: no data in window.
	PropStateUnknown PropagationState = iota
	// PropInsufficientCoverage: fewer than min_responding distinct
	// responsive resolvers.
	PropInsufficientCoverage
	// PropUnresolvable: the authoritative view itself is failing.
	PropUnresolvable
	// PropDivergent: ≥1 resolver fingerprint ≠ reference, beyond the
	// convergence window.
	PropDivergent
	// PropConverging: reference changed recently; partial agreement.
	PropConverging
	// PropConverged: all responding resolvers match the reference.
	PropConverged
)

// String is the closed label set for rift_dns_propagation_state rendering.
func (p PropagationState) String() string {
	switch p {
	case PropStateUnknown:
		return "unknown"
	case PropInsufficientCoverage:
		return "insufficient_coverage"
	case PropUnresolvable:
		return "unresolvable"
	case PropDivergent:
		return "divergent"
	case PropConverging:
		return "converging"
	case PropConverged:
		return "converged"
	default:
		return "unknown"
	}
}
