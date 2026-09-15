package retry

import (
	"context"
	"math/rand"
	"time"

	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// Backoff is the delay schedule between attempts. Jitter ∈ [0,1] is the
// fraction of the computed delay randomized (full jitter: delay × rand).
type Backoff struct {
	Base, Cap time.Duration
	Jitter    float64
}

// Policy decides whether attempt n+1 should proceed after err, and after
// what delay. MaxAttempts ≥ 1; 1 disables retry (one attempt total).
type Policy struct {
	MaxAttempts int
	Backoff     Backoff
	RetryOn     func(err error) bool
}

// defaultRetryable is the conservative default predicate: only network and
// timeout classes retry. Security/config/resource/peer-closed never do.
func defaultRetryable(err error) bool {
	switch errs.ClassOf(err) {
	case errs.ClassNetwork, errs.ClassTimeout:
		return true
	default:
		return false
	}
}

func (p Policy) withDefaults() Policy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.RetryOn == nil {
		p.RetryOn = defaultRetryable
	}
	if p.Backoff.Base <= 0 {
		p.Backoff.Base = 100 * time.Millisecond
	}
	if p.Backoff.Cap <= 0 {
		p.Backoff.Cap = 5 * time.Second
	}
	if p.Backoff.Jitter < 0 {
		p.Backoff.Jitter = 0
	}
	if p.Backoff.Jitter > 1 {
		p.Backoff.Jitter = 1
	}
	return p
}

// Attempt returns the delay for the next attempt (n is the attempt index
// that just failed, 0-based) and whether to proceed. It never sleeps.
//
// Semantics: attempt indices 0..MaxAttempts-1 are allowed. When n+1 ==
// MaxAttempts the budget is exhausted → proceed=false.
func (p Policy) Attempt(ctx context.Context, n int, err error) (time.Duration, bool) {
	p = p.withDefaults()
	if err != nil {
		if !p.RetryOn(err) {
			return 0, false
		}
	}
	if n+1 >= p.MaxAttempts {
		return 0, false
	}
	delay := p.delayFor(n)
	return delay, true
}

// delayFor computes the exponential delay for the next attempt after
// attempt n failed: Base × 2^n, capped, then full-jitter scaled.
func (p Policy) delayFor(n int) time.Duration {
	d := p.Backoff.Base
	for i := 0; i < n && d < p.Backoff.Cap; i++ {
		d *= 2
	}
	if d > p.Backoff.Cap {
		d = p.Backoff.Cap
	}
	if p.Backoff.Jitter > 0 {
		j := 1 - p.Backoff.Jitter*rand.Float64() // full jitter: [1-J, 1]
		d = time.Duration(float64(d) * j)
	}
	return d
}
