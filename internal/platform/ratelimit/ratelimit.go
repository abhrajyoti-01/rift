package ratelimit

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

// Rate is a token-bucket configuration.
type Rate struct {
	PerSecond float64
	Burst     int
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time
	rate   Rate
}

type shard struct {
	mu  sync.Mutex
	m   map[string]*list.Element
	lru list.List
}

// Sharded is the bounded-cardinality per-key limiter.
type Sharded struct {
	shards   []shard
	capacity int
	mask     uint64
	keyCount atomic.Int64
	evicted  atomic.Uint64
	now      func() time.Time
}

// NewSharded builds a sharded limiter. shards is rounded to a power of two
// with floor 8; capacity bounds distinct tracked keys globally. now may be
// nil (time.Now); tests inject a fake clock.
func NewSharded(shards, capacity int, now func() time.Time) *Sharded {
	if shards < 8 {
		shards = 8
	}
	p := 1
	for p < shards {
		p <<= 1
	}
	if capacity < 8 {
		capacity = 8
	}
	if now == nil {
		now = time.Now
	}
	s := &Sharded{
		shards:   make([]shard, p),
		capacity: capacity,
		mask:     uint64(p - 1),
		now:      now,
	}
	for i := range s.shards {
		s.shards[i].m = make(map[string]*list.Element)
	}
	return s
}

// fnv1a64 hashes a key; high bits (>> 32) select the shard so distinct
// keys spread uniformly.
func fnv1a64(k string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(k); i++ {
		h ^= uint64(k[i])
		h *= prime64
	}
	return h
}

func (s *Sharded) shardFor(key string) *shard {
	return &s.shards[(fnv1a64(key)>>32)&s.mask]
}

// EvictedTotal reports how many keys lost their token history to LRU
// eviction.
func (s *Sharded) EvictedTotal() uint64 { return s.evicted.Load() }

// KeysTracked reports current distinct keys (exact when quiesced;
// atomic under concurrent use).
func (s *Sharded) KeysTracked() int {
	return int(s.keyCount.Load())
}

// Set configures the bucket for key. Config-time only.
func (s *Sharded) Set(key string, r Rate) {
	sh := s.shardFor(key)
	now := s.now()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	s.upsertLocked(sh, key, r, now)
}

// upsertLocked inserts or updates key's bucket, evicting LRU keys beyond
// the global capacity. Caller holds sh.mu. Eviction happens within the
// caller's shard only: cross-shard eviction would need cross-shard locks
// (deadlock-prone), and per-shard capacity (capacity/shards rounded up)
// bounds the global total just as hard.
func (s *Sharded) upsertLocked(sh *shard, key string, r Rate, now time.Time) {
	if el, ok := sh.m[key]; ok {
		b := el.Value.(*bucket)
		b.rate = r
		b.last = now
		sh.lru.MoveToFront(el)
		return
	}
	b := &bucket{key: key, tokens: float64(r.Burst), last: now, rate: r}
	el := sh.lru.PushFront(b)
	sh.m[key] = el
	s.keyCount.Add(1)

	shardCap := (s.capacity + len(s.shards) - 1) / len(s.shards)
	for len(sh.m) > shardCap {
		if !s.evictTailLocked(sh) {
			return
		}
	}
}

// evictTailLocked removes the least-recently-used key from sh, keeping
// keyCount and evicted exact. Caller holds sh.mu.
func (s *Sharded) evictTailLocked(sh *shard) bool {
	el := sh.lru.Back()
	if el == nil {
		return false
	}
	b := el.Value.(*bucket)
	sh.lru.Remove(el)
	delete(sh.m, b.key)
	s.keyCount.Add(-1)
	s.evicted.Add(1)
	return true
}

// Allow consumes one token for key.
func (s *Sharded) Allow(key string) bool {
	return s.AllowN(key, 1)
}

// AllowN consumes n tokens atomically: if the bucket holds fewer than n,
// nothing is consumed (no partial spends). Unknown keys are refused
// (fail-closed): rate limiting is opt-in per configured key, not
// default-allow.
func (s *Sharded) AllowN(key string, n int) bool {
	sh := s.shardFor(key)
	now := s.now()
	sh.mu.Lock()
	defer sh.mu.Unlock()

	el, ok := sh.m[key]
	if !ok {
		return false
	}
	b := el.Value.(*bucket)
	s.refillLocked(b, now)
	if b.tokens >= float64(n) {
		b.tokens -= float64(n)
		sh.lru.MoveToFront(el)
		return true
	}
	sh.lru.MoveToFront(el)
	return false
}

// refillLocked adds elapsed × rate tokens, capped at Burst.
func (s *Sharded) refillLocked(b *bucket, now time.Time) {
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	b.tokens += elapsed.Seconds() * b.rate.PerSecond
	if b.tokens > float64(b.rate.Burst) {
		b.tokens = float64(b.rate.Burst)
	}
	b.last = now
}
