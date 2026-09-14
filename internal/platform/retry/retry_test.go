package retry

import (
	"context"
	"testing"
	"time"

	"github.com/rift/rift/internal/platform/errs"
)

func TestAttemptBudget(t *testing.T) {
	p := Policy{MaxAttempts: 3, Backoff: Backoff{Base: 10 * time.Millisecond, Cap: 1 * time.Second}}
	netErr := errs.New(errs.ClassNetwork, "test", "dial failed")

	// Attempt 0 failed → attempt 1 allowed; 1 failed → 2 allowed; 2
	// failed → budget exhausted (3 total attempts).
	for n := 0; n < 2; n++ {
		if _, ok := p.Attempt(context.Background(), n, netErr); !ok {
			t.Fatalf("attempt %d should be retryable within budget 3", n)
		}
	}
	if _, ok := p.Attempt(context.Background(), 2, netErr); ok {
		t.Fatal("attempt 3 must be refused (budget exhausted)")
	}
}

func TestMaxAttemptsOneDisablesRetry(t *testing.T) {
	p := Policy{MaxAttempts: 1}
	if _, ok := p.Attempt(context.Background(), 0, errs.New(errs.ClassNetwork, "t", "x")); ok {
		t.Fatal("MaxAttempts=1 must disable retry")
	}
}

func TestNonRetryableClassNeverRetries(t *testing.T) {
	p := Policy{MaxAttempts: 5}
	// Security, config, resource, peer-closed are never retryable under
	// the default predicate — the closed-set rule (AD-15 posture).
	for _, class := range []errs.Class{
		errs.ClassSecurity, errs.ClassConfig, errs.ClassResource, errs.ClassPeerClosed,
	} {
		e := errs.New(class, "t", "x")
		if _, ok := p.Attempt(context.Background(), 0, e); ok {
			t.Errorf("class %v must never be retryable by default", class)
		}
	}
	// Network and timeout are.
	for _, class := range []errs.Class{errs.ClassNetwork, errs.ClassTimeout} {
		e := errs.New(class, "t", "x")
		if _, ok := p.Attempt(context.Background(), 0, e); !ok {
			t.Errorf("class %v should be retryable by default", class)
		}
	}
}

func TestBackoffExponentialAndCap(t *testing.T) {
	p := Policy{
		MaxAttempts: 10,
		Backoff:     Backoff{Base: 10 * time.Millisecond, Cap: 100 * time.Millisecond},
	}.withDefaults()
	// No jitter for determinism here.
	p.Backoff.Jitter = 0

	want := []time.Duration{
		10 * time.Millisecond,  // after attempt 0
		20 * time.Millisecond, // after attempt 1
		40 * time.Millisecond,
		80 * time.Millisecond,
		100 * time.Millisecond, // capped (160 would exceed)
		100 * time.Millisecond,
	}
	for n, w := range want {
		if got := p.delayFor(n); got != w {
			t.Errorf("delayFor(%d) = %v, want %v", n, got, w)
		}
	}
}

func TestJitterBounded(t *testing.T) {
	p := Policy{
		MaxAttempts: 10,
		Backoff:     Backoff{Base: 100 * time.Millisecond, Cap: time.Second, Jitter: 0.5},
	}.withDefaults()
	for i := 0; i < 200; i++ {
		d := p.delayFor(3) // base delay 800ms
		if d < 400*time.Millisecond || d > 800*time.Millisecond {
			t.Fatalf("jittered delay %v outside [400ms, 800ms]", d)
		}
	}
}

func TestNilErrProceeds(t *testing.T) {
	// A nil error on Attempt means "previous attempt did not fail" — used
	// by schedulers polling for a slot. Budget rules still apply.
	p := Policy{MaxAttempts: 2}
	if _, ok := p.Attempt(context.Background(), 0, nil); !ok {
		t.Fatal("nil err should be proceedable within budget")
	}
}
