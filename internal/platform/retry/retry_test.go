package retry

import (
	"context"
	"testing"
	"time"

	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

func TestAttemptBudget(t *testing.T) {
	p := Policy{MaxAttempts: 3, Backoff: Backoff{Base: 10 * time.Millisecond, Cap: time.Second}}
	netErr := errs.New(errs.ClassNetwork, "test", "dial failed")

	for n := 0; n < 2; n++ {
		if _, ok := p.Attempt(context.Background(), n, netErr); !ok {
			t.Fatalf("attempt %d should be retryable within a budget of 3", n)
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

// TestRetryableClassSet is the safety contract: the classes that must never
// be retried.
func TestRetryableClassSet(t *testing.T) {
	p := Policy{MaxAttempts: 5}
	for _, class := range []errs.Class{
		errs.ClassSecurity, errs.ClassConfig, errs.ClassResource, errs.ClassPeerClosed,
	} {
		if _, ok := p.Attempt(context.Background(), 0, errs.New(class, "t", "x")); ok {
			t.Errorf("class %v must never be retryable by default", class)
		}
	}
	for _, class := range []errs.Class{errs.ClassNetwork, errs.ClassTimeout} {
		if _, ok := p.Attempt(context.Background(), 0, errs.New(class, "t", "x")); !ok {
			t.Errorf("class %v should be retryable by default", class)
		}
	}
}

func TestBackoffExponentialWithCap(t *testing.T) {
	p := Policy{
		MaxAttempts: 10,
		Backoff:     Backoff{Base: 10 * time.Millisecond, Cap: 100 * time.Millisecond},
	}.withDefaults()
	p.Backoff.Jitter = 0

	want := []time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond,
		80 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond,
	}
	for n, w := range want {
		if got := p.delayFor(n); got != w {
			t.Errorf("delayFor(%d) = %v, want %v", n, got, w)
		}
	}
}

func TestJitterStaysBounded(t *testing.T) {
	p := Policy{
		MaxAttempts: 10,
		Backoff:     Backoff{Base: 100 * time.Millisecond, Cap: time.Second, Jitter: 0.5},
	}.withDefaults()
	for i := 0; i < 200; i++ {
		d := p.delayFor(3) // base 800ms
		if d < 400*time.Millisecond || d > 800*time.Millisecond {
			t.Fatalf("jittered delay %v outside [400ms, 800ms]", d)
		}
	}
}

func TestNilErrorProceedsWithinBudget(t *testing.T) {
	p := Policy{MaxAttempts: 2}
	if _, ok := p.Attempt(context.Background(), 0, nil); !ok {
		t.Fatal("nil error should be proceedable within budget")
	}
	if _, ok := p.Attempt(context.Background(), 1, nil); ok {
		t.Fatal("budget must still apply with a nil error")
	}
}