package circuit

import (
	"testing"
	"testing/synctest"
	"time"
)

// Breaker timing tests run inside synctest bubbles: zero real sleeps, all
// cooldown transitions deterministic (AR-8, TECHNICAL_SPEC §2.3).
func TestBreakerOpensAfterThreshold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now
		b := New(Config{FailureThreshold: 3, RecoveryCooldown: 10 * time.Second, SuccessThreshold: 2}, now)

		for i := 0; i < 2; i++ {
			if !b.Allow() {
				t.Fatal("closed breaker must allow")
			}
			b.Failure()
		}
		if b.State() != "closed" {
			t.Fatalf("after 2/3 failures: %s, want closed", b.State())
		}
		if !b.Allow() {
			t.Fatal("still closed at 2/3")
		}
		b.Failure() // 3rd consecutive failure → open
		if b.State() != "open" {
			t.Fatalf("after 3 failures: %s, want open", b.State())
		}
		if b.Allow() {
			t.Fatal("open breaker must refuse (cooldown running)")
		}
	})
}

func TestBreakerCooldownToHalfOpenAndClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(Config{FailureThreshold: 2, RecoveryCooldown: 10 * time.Second, SuccessThreshold: 2}, time.Now)
		b.Failure()
		b.Failure()
		if b.State() != "open" {
			t.Fatalf("state %s, want open", b.State())
		}

		time.Sleep(10 * time.Second) // bubble clock: instant, deterministic
		if b.State() != "half_open" {
			t.Fatalf("after cooldown: %s, want half_open", b.State())
		}

		// First probe succeeds; with SuccessThreshold 2 the second call is
		// still admitted (probe budget 2) and its success closes the
		// breaker.
		if !b.Allow() {
			t.Fatal("probe refused")
		}
		if !b.Allow() {
			t.Fatal("second half-open admission must be allowed (budget = SuccessThreshold)")
		}
		b.Success()
		b.Success()
		if b.State() != "closed" {
			t.Fatalf("state %s, want closed", b.State())
		}
		if !b.Allow() {
			t.Fatal("closed breaker must allow")
		}
	})
}

func TestBreakerHalfOpenCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(Config{FailureThreshold: 2, RecoveryCooldown: 5 * time.Second, SuccessThreshold: 2}, time.Now)
		b.Failure()
		b.Failure()
		time.Sleep(5 * time.Second)

		// Probe budget = SuccessThreshold (2): first probe succeeds, second
		// admission's success closes the breaker.
		if !b.Allow() {
			t.Fatal("probe refused")
		}
		b.Success()
		if b.State() != "half_open" {
			t.Fatalf("after 1/2 successes: %s, want half_open", b.State())
		}
		if !b.Allow() {
			t.Fatal("second half-open admission must be allowed")
		}
		b.Success()
		if b.State() != "closed" {
			t.Fatalf("state %s, want closed", b.State())
		}
		if !b.Allow() {
			t.Fatal("closed breaker must allow")
		}
	})
}

func TestBreakerHalfOpenProbeFailureReopens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(Config{FailureThreshold: 1, RecoveryCooldown: 5 * time.Second, SuccessThreshold: 2}, time.Now)
		b.Failure()
		time.Sleep(5 * time.Second)
		if !b.Allow() {
			t.Fatal("probe refused")
		}
		b.Failure() // probe fails → reopen immediately
		if b.State() != "open" {
			t.Fatalf("after probe failure: %s, want open", b.State())
		}
		if b.Allow() {
			t.Fatal("must be refused while reopened")
		}
	})
}

func TestBreakerSuccessResetsConsecutiveCount(t *testing.T) {
	b := New(Config{FailureThreshold: 3, RecoveryCooldown: time.Second, SuccessThreshold: 1}, time.Now)
	b.Failure()
	b.Failure()
	b.Success() // resets
	b.Failure()
	b.Failure()
	if b.State() != "closed" {
		t.Errorf("2 failures after reset: %s, want closed (never hit threshold)", b.State())
	}
}
