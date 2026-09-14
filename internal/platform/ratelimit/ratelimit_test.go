package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// fakeClock drives token refill deterministically (AR-8: no real sleeps).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0)}
}
func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func TestTokenBucketBurstAndRefill(t *testing.T) {
	fc := newFakeClock()
	s := NewSharded(8, 100, fc.Now)

	// 10 tokens/s, burst 5.
	s.Set("client-a", Rate{PerSecond: 10, Burst: 5})

	// Burst exhausts immediately.
	for i := 0; i < 5; i++ {
		if !s.Allow("client-a") {
			t.Fatalf("Allow #%d during burst: refused", i+1)
		}
	}
	if s.Allow("client-a") {
		t.Fatal("6th Allow within burst must be refused")
	}

	// 250ms refills 2.5 tokens at 10/s: two Allows pass, third fails.
	fc.Advance(250 * time.Millisecond)
	if !s.Allow("client-a") {
		t.Fatal("Allow after 250ms refill should pass")
	}
	if !s.Allow("client-a") {
		t.Fatal("second Allow after 250ms refill should pass")
	}
	if s.Allow("client-a") {
		t.Fatal("third Allow at 2.5 tokens must fail")
	}

	// Full refill after 1s: 5 Allows pass.
	fc.Advance(1 * time.Second)
	passed := 0
	for i := 0; i < 5; i++ {
		if s.Allow("client-a") {
			passed++
		}
	}
	if passed != 5 {
		t.Errorf("after full refill: %d/5 allowed", passed)
	}
}

func TestAllowNAtomicNoPartialSpend(t *testing.T) {
	fc := newFakeClock()
	s := NewSharded(8, 100, fc.Now)
	s.Set("bulk", Rate{PerSecond: 1, Burst: 3})

	// Ask for 4 with 3 available: refused, and ALL 3 tokens remain.
	if s.AllowN("bulk", 4) {
		t.Fatal("AllowN(4) with 3 tokens must refuse")
	}
	if !s.AllowN("bulk", 3) {
		t.Fatal("AllowN(3) after refused 4 must succeed (no partial spend)")
	}
}

func TestUnknownKeyFailsClosed(t *testing.T) {
	fc := newFakeClock()
	s := NewSharded(8, 100, fc.Now)
	// Unconfigured key: refused. Rate limiting is opt-in per key; a
	// default-allow limiter is no limiter (fail-closed posture).
	if s.Allow("never-configured") {
		t.Fatal("unknown key must be refused (fail-closed)")
	}
}

func TestBoundedCardinalityUnderFlood(t *testing.T) {
	fc := newFakeClock()
	const capacity = 64
	s := NewSharded(16, capacity, fc.Now)

	// Spoofed-source flood: 10,000 distinct keys. Tracked keys must never
	// exceed capacity, and the honesty counter must report every eviction.
	for i := 0; i < 10000; i++ {
		key := string(rune('a'+i%26)) + time.Duration(i).String()
		s.Set(key, Rate{PerSecond: 1, Burst: 1})
	}
	if got := s.KeysTracked(); got > capacity {
		t.Errorf("KeysTracked = %d exceeds capacity %d — memory bomb", got, capacity)
	}
	if s.EvictedTotal() == 0 {
		t.Error("evictions happened but honesty counter is zero — silent loss")
	}
}

func TestConcurrentAllowRace(t *testing.T) {
	fc := newFakeClock()
	s := NewSharded(8, 128, fc.Now)
	s.Set("hot", Rate{PerSecond: 1000, Burst: 100})

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				s.Allow("hot")
			}
		}()
	}
	wg.Wait()
	// No assertion on the exact count (that's the concurrency contract);
	// the race detector owns this test.
}

func TestShardsSpreadKeys(t *testing.T) {
	fc := newFakeClock()
	s := NewSharded(16, 4096, fc.Now)
	for i := 0; i < 512; i++ {
		s.Set("key-"+time.Duration(i).String(), Rate{PerSecond: 1, Burst: 1})
	}
	// With 512 keys across 16 shards, every shard should have some load —
	// a single hot shard would mean the hash is broken.
	var empty int
	for i := range s.shards {
		if len(s.shards[i].m) == 0 {
			empty++
		}
	}
	if empty > 12 { // allow statistical slack, fail on systematic collapse
		t.Errorf("%d/16 shards empty — shard selection is collapsing", empty)
	}
}
