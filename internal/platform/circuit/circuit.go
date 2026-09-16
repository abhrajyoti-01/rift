package circuit

import (
	"sync"
	"time"
)

// Config sets breaker thresholds. Zero fields adopt defaults.
type Config struct {
	FailureThreshold int           // consecutive failures to open; default 5
	RecoveryCooldown time.Duration // open → half-open; default 10s
	SuccessThreshold int           // half-open successes to close; default 2
}

func (c Config) withDefaults() Config {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.RecoveryCooldown <= 0 {
		c.RecoveryCooldown = 10 * time.Second
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = 2
	}
	return c
}

type state uint8

const (
	stateClosed state = iota
	stateOpen
	stateHalfOpen
)

// Breaker is a circuit breaker for one dependency.
type Breaker struct {
	mu                sync.Mutex
	cfg               Config
	state             state
	consecFailures    int
	halfOpenSuccesses int
	halfOpenAttempts  int
	openedAt          time.Time
	now               func() time.Time
}

// New builds a breaker. now may be nil (time.Now); tests must inject.
func New(cfg Config, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{cfg: cfg.withDefaults(), state: stateClosed, now: now}
}

// State reports the current state for metrics/inspection.
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Open → half-open transition is time-driven: report it when the
	// cooldown has elapsed so observation matches behavior.
	if b.state == stateOpen && b.now().Sub(b.openedAt) >= b.cfg.RecoveryCooldown {
		return "half_open"
	}
	switch b.state {
	case stateClosed:
		return "closed"
	case stateOpen:
		return "open"
	default:
		return "half_open"
	}
}

// Allow reports whether a call should proceed. When open, Allow admits
// half-open probes once the cooldown elapses — up to SuccessThreshold
// attempts per half-open cycle, so a SuccessThreshold above 1 can actually
// be reached. Attempts are consumed on admission; an abandoned probe
// (neither Success nor Failure) leaves the budget reduced, which fails
// safe (fewer admissions, never more).
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		return true
	case stateOpen:
		if b.now().Sub(b.openedAt) >= b.cfg.RecoveryCooldown {
			b.state = stateHalfOpen
			b.halfOpenSuccesses = 0
			b.halfOpenAttempts = 1
			return true
		}
		return false
	default:
		if b.halfOpenAttempts < b.cfg.SuccessThreshold {
			b.halfOpenAttempts++
			return true
		}
		return false
	}
}

// Success records a successful call.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		b.consecFailures = 0
	case stateHalfOpen:
		b.halfOpenSuccesses++
		if b.halfOpenSuccesses >= b.cfg.SuccessThreshold {
			b.state = stateClosed
			b.consecFailures = 0
			b.halfOpenSuccesses = 0
			b.halfOpenAttempts = 0
		}
	}
}

// Failure records a failed call; consecutive failures beyond the
// threshold open the breaker.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		b.consecFailures++
		if b.consecFailures >= b.cfg.FailureThreshold {
			b.state = stateOpen
			b.openedAt = b.now()
		}
	case stateHalfOpen:
		// Probe failed: reopen immediately for another cooldown.
		b.state = stateOpen
		b.openedAt = b.now()
	}
}
