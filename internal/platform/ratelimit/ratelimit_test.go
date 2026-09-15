package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// fakeClock drives token refill deterministically, with no real sleeps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock               { return &fakeClock{now: time.Unix(0, 0)} }
func (f *fakeClock) Now() time.Time           { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *fakeClock) Advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

func TestTokenBucketBurstAndRefill(t *testing.T) {
	fc := newFakeClock()
	s := NewSharded(8, 100, fc.Now)
	s.Set("client", Rate{PerSecond: 10, Burst: 5})

	for i := 0; i < 5; i++ {
		if !s.Allow("client") {
			t.Fatalf("Allow #%d within burst refused", i+1)
		}
	}
	if s.Allow("client") {
		t.Fatal("6th Allow within burst must be refused")
	}

	// 250ms at 10/s refills 2.5 tokens: two pass, third fails.
	fc.Advance(250 * time.Millisecond)
	if !s.Allow("client") || !s.Allow("client") {
		t.Fatal("two Allow calls should pass after 250ms refill")
	}
	if s.Allow("client") {
		t.Fatal("third Allow at 2.5 tokens must fail")
	}

	fc.Advance(time.Second)
	passed := 0
	for i := 0; i < 5; i++ {
		if s.Allow("client") {
			passed++
		}
	}
	if passed != 5 {
		t.Errorf("after full refill: %d/5 allowed", passed)
	}
}

func TestAllowNAtomic(t *testing.T) {
	fc := newFakeClock()
	s := NewSharded(8, 100, fc.Now)
	s.Set("bulk", Rate{PerSecond: 1, Burst: 3})

	if s.AllowN("bulk", 4) {
		t.Fatal("AllowN(4) with 3 tokens must refuse")
	}
	if !s.AllowN("bulk", 3) {
		t.Fatal("refused AllowN must not have consumed tokens (no partial spend)")
	}
}

func TestUnknownKeyFailsClosed(t *testing.T) {
	fc := newFakeClock()
	s := NewSharded(8, 100, fc.Now)
	if s.Allow("never-configured") {
		t.Fatal("unconfigured key must be refused: a default-allow limiter is no limiter")
	}
}

func TestBoundedCardinalityUnderFlood(t *testing.T) {
	fc := newFakeClock()
	const capacity = 64
	s := NewSharded(16, capacity, fc.Now)

	for i := 0; i < 10000; i++ {
		s.Set(string(rune('a'+i%26))+time.Duration(i).String(), Rate{PerSecond: 1, Burst: 1})
	}
	if got := s.KeysTracked(); got > capacity {
		t.Errorf("KeysTracked = %d exceeds capacity %d (memory bomb)", got, capacity)
	}
	if s.EvictedTotal() == 0 {
		t.Error("evictions occurred but the honesty counter is zero")
	}
}

func TestConcurrentAllow(t *testing.T) {
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
}

func TestShardsSpreadKeys(t *testing.T) {
	fc := newFakeClock()
	s := NewSharded(16, 4096, fc.Now)
	for i := 0; i < 512; i++ {
		s.Set("key-"+time.Duration(i).String(), Rate{PerSecond: 1, Burst: 1})
	}
	empty := 0
	for i := range s.shards {
		if len(s.shards[i].m) == 0 {
			empty++
		}
	}
	if empty > 12 {
		t.Errorf("%d/16 shards empty — shard selection is collapsing", empty)
	}
}