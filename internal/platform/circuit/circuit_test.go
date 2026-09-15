package circuit

import (
	"testing"
	"testing/synctest"
	"time"
)

// Breaker timing runs entirely under synctest: zero real sleeps, fully
// deterministic cooldown transitions.
func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(Config{FailureThreshold: 3, RecoveryCooldown: 10 * time.Second, SuccessThreshold: 2}, time.Now)
		for i := 0; i < 2; i++ {
			if !b.Allow() {
				t.Fatal("closed breaker must allow")
			}
			b.Failure()
		}
		if b.State() != "closed" {
			t.Fatalf("2/3 failures: %s, want closed", b.State())
		}
		b.Failure()
		if b.State() != "open" {
			t.Fatalf("3 failures: %s, want open", b.State())
		}
		if b.Allow() {
			t.Fatal("open breaker must refuse while cooling down")
		}
	})
}

func TestBreakerHalfOpenProbeBudgetReachesThreshold(t *testing.T) {
	// Regression guard: with SuccessThreshold 2, the breaker must admit
	// enough half-open probes for the threshold to be reachable. Admitting
	// only one probe would make closing impossible.
	synctest.Test(t, func(t *testing.T) {
		b := New(Config{FailureThreshold: 2, RecoveryCooldown: 5 * time.Second, SuccessThreshold: 2}, time.Now)
		b.Failure()
		b.Failure()
		time.Sleep(5 * time.Second)

		if !b.Allow() {
			t.Fatal("first half-open probe refused")
		}
		b.Success()
		if b.State() != "half_open" {
			t.Fatalf("1/2 successes: %s, want half_open", b.State())
		}
		if !b.Allow() {
			t.Fatal("second half-open admission must be allowed (budget = threshold)")
		}
		b.Success()
		if b.State() != "closed" {
			t.Fatalf("after threshold successes: %s, want closed", b.State())
		}
	})
}

func TestBreakerProbeFailureReopens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(Config{FailureThreshold: 1, RecoveryCooldown: 5 * time.Second, SuccessThreshold: 2}, time.Now)
		b.Failure()
		time.Sleep(5 * time.Second)
		if !b.Allow() {
			t.Fatal("probe refused")
		}
		b.Failure()
		if b.State() != "open" {
			t.Fatalf("after probe failure: %s, want open", b.State())
		}
		if b.Allow() {
			t.Fatal("must refuse while reopened")
		}
	})
}

func TestBreakerSuccessResetsConsecutiveCount(t *testing.T) {
	b := New(Config{FailureThreshold: 3, RecoveryCooldown: time.Second, SuccessThreshold: 1}, time.Now)
	b.Failure()
	b.Failure()
	b.Success()
	b.Failure()
	b.Failure()
	if b.State() != "closed" {
		t.Errorf("2 failures after a reset: %s, want closed", b.State())
	}
}

func TestBreakerDefaults(t *testing.T) {
	b := New(Config{}, time.Now)
	for i := 0; i < 4; i++ {
		b.Failure()
	}
	if b.State() != "closed" {
		t.Error("default FailureThreshold is 5; 4 failures must not open")
	}
	b.Failure()
	if b.State() != "open" {
		t.Error("5 failures should open with default threshold")
	}
}