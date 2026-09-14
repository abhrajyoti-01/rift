package picker

import (
	"context"
	"fmt"
	"sync"
	"testing"

	lbmodel "github.com/rift/rift/internal/lb/model"
)

func mkPool(n int, weights ...int) *lbmodel.Pool {
	bs := make([]*lbmodel.Backend, n)
	for i := range bs {
		w := 1
		if i < len(weights) && weights[i] > 0 {
			w = weights[i]
		}
		b := &lbmodel.Backend{ID: fmt.Sprintf("b%d", i), Addr: fmt.Sprintf("127.0.0.1:%d", 9000+i), Weight: w}
		b.Health.Store(true)
		bs[i] = b
	}
	return &lbmodel.Pool{ID: "p", Backends: bs}
}

func TestRoundRobinUniformDistribution(t *testing.T) {
	const n, picks = 8, 8000
	pool := mkPool(n)
	p, err := New("round_robin", pool)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for i := 0; i < picks; i++ {
		b, err := p.Pick(context.Background(), PickHint{})
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		counts[b.ID]++
	}
	// χ² test: with 7992/8 expected per backend, critical χ²(7 df, p=0.01)
	// ≈ 18.48. Round-robin is deterministic-uniform modulo skipping, so
	// this passing is a structural check; the χ² gate matters for the
	// weighted and least-conn variants.
	var chi2 float64
	expected := float64(picks) / float64(n)
	for _, c := range counts {
		d := float64(c) - expected
		chi2 += d * d / expected
	}
	if chi2 > 18.48 {
		t.Errorf("χ² = %.2f exceeds p=0.01 critical value 18.48; distribution: %v", chi2, counts)
	}
}

func TestRoundRobinSkipsUnhealthy(t *testing.T) {
	pool := mkPool(3)
	pool.Backends[1].Health.Store(false)
	p, _ := New("round_robin", pool)
	for i := 0; i < 10; i++ {
		b, err := p.Pick(context.Background(), PickHint{})
		if err != nil {
			t.Fatal(err)
		}
		if b.ID == "b1" {
			t.Fatalf("unhealthy b1 picked at iteration %d", i)
		}
	}
}

func TestAllUnhealthyIsErrNoUpstream(t *testing.T) {
	for _, kind := range []string{"round_robin", "weighted_round_robin", "least_connections"} {
		pool := mkPool(3)
		for _, b := range pool.Backends {
			b.Health.Store(false)
		}
		p, err := New(kind, pool)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if _, err := p.Pick(context.Background(), PickHint{}); err == nil {
			t.Errorf("%s: all-unhealthy pool must yield ErrNoUpstream", kind)
		}
	}
}

func TestSmoothWRRInterleave(t *testing.T) {
	// weights 5,3,1: over 9 picks, the smooth sequence interleaves rather
	// than bursting 5 consecutive b0 picks (nginx algorithm property).
	pool := mkPool(3, 5, 3, 1)
	p, err := New("weighted_round_robin", pool)
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for i := 0; i < 9; i++ {
		b, err := p.Pick(context.Background(), PickHint{})
		if err != nil {
			t.Fatal(err)
		}
		seq = append(seq, b.ID)
	}
	counts := map[string]int{}
	for _, id := range seq {
		counts[id]++
	}
	if counts["b0"] != 5 || counts["b1"] != 3 || counts["b2"] != 1 {
		t.Errorf("counts over cycle = %v, want 5/3/1 (seq %v)", counts, seq)
	}
	// No burst: b0 never picked 3× in a row (weight-5 burst would be the
	// naive-interleave failure mode the smooth variant exists to prevent).
	run := 1
	for i := 1; i < len(seq); i++ {
		if seq[i] == seq[i-1] {
			run++
			if run >= 3 {
				t.Errorf("burst of %d consecutive %s in %v — not smooth", run, seq[i], seq)
			}
		} else {
			run = 1
		}
	}
}

func TestSmoothWRRSkipsUnhealthy(t *testing.T) {
	pool := mkPool(3, 5, 3, 1)
	pool.Backends[0].Health.Store(false) // the weight-5 one
	p, _ := New("weighted_round_robin", pool)
	for i := 0; i < 12; i++ {
		b, err := p.Pick(context.Background(), PickHint{})
		if err != nil {
			t.Fatal(err)
		}
		if b.ID == "b0" {
			t.Fatalf("unhealthy b0 picked at %d", i)
		}
	}
	// With b0 out, effective weights 3/1: counts over 12 picks.
	pool2 := mkPool(3, 5, 3, 1)
	pool2.Backends[0].Health.Store(false)
	p2, _ := New("weighted_round_robin", pool2)
	c := map[string]int{}
	for i := 0; i < 12; i++ {
		b, _ := p2.Pick(context.Background(), PickHint{})
		c[b.ID]++
	}
	if c["b1"] != 9 || c["b2"] != 3 {
		t.Errorf("remaining backends should split 3:1 → 9/3, got %v", c)
	}
}

func TestLeastConnsPicksMinimum(t *testing.T) {
	pool := mkPool(4)
	pool.Backends[0].Conns.Store(5)
	pool.Backends[1].Conns.Store(1)
	pool.Backends[2].Conns.Store(3)
	pool.Backends[3].Conns.Store(1)
	p, _ := New("least_connections", pool)

	b, err := p.Pick(context.Background(), PickHint{})
	if err != nil {
		t.Fatal(err)
	}
	// b1 and b3 tie at 1; either is correct, but NOT b0 (5) or b2 (3).
	if b.ID != "b1" && b.ID != "b3" {
		t.Errorf("picked %s with conns 5,3,1,1 — must be a minimum", b.ID)
	}
}

func TestLeastConnsTiesRotate(t *testing.T) {
	pool := mkPool(3)
	// All at 0: picks must spread across all three.
	p, _ := New("least_connections", pool)
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		b, err := p.Pick(context.Background(), PickHint{})
		if err != nil {
			t.Fatal(err)
		}
		seen[b.ID] = true
		b.Conns.Add(1) // simulate load so next pick goes elsewhere
	}
	if len(seen) != 3 {
		t.Errorf("tied backends: only %d of 3 ever picked (%v)", len(seen), seen)
	}
}

func TestLeastConnsFollowsDroppingLoad(t *testing.T) {
	pool := mkPool(3)
	pool.Backends[0].Conns.Store(0)
	pool.Backends[1].Conns.Store(10)
	pool.Backends[2].Conns.Store(20)
	p, _ := New("least_connections", pool)

	b0, _ := p.Pick(context.Background(), PickHint{})
	if b0.ID != "b0" {
		t.Fatalf("first pick %s, want b0 (0 conns)", b0.ID)
	}
	// b0 takes load; b1 releases.
	pool.Backends[0].Conns.Store(15)
	pool.Backends[1].Conns.Store(2)
	b1, _ := p.Pick(context.Background(), PickHint{})
	if b1.ID != "b1" {
		t.Fatalf("after load shift, pick %s, want b1 (2 conns)", b1.ID)
	}
}

func TestPickerAllocFree(t *testing.T) {
	// NFR-2: zero allocations on the pick path (asserted; the benchmark
	// sweeps scale and parallelism in Phase 2's bench files).
	for _, kind := range []string{"round_robin", "weighted_round_robin", "least_connections"} {
		pool := mkPool(64)
		for i := range pool.Backends {
			pool.Backends[i].Weight = 1 + i%5
			pool.Backends[i].Conns.Store(int64(i % 10))
		}
		p, err := New(kind, pool)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		n := testing.AllocsPerRun(100, func() {
			_, _ = p.Pick(context.Background(), PickHint{})
		})
		if n > 0 {
			t.Errorf("%s: %.0f allocs/pick, want 0 (NFR-2)", kind, n)
		}
	}
}

func TestPickerConcurrentSafety(t *testing.T) {
	// Race-detector food: concurrent picks across all three pickers.
	for _, kind := range []string{"round_robin", "weighted_round_robin", "least_connections"} {
		pool := mkPool(16)
		for i := range pool.Backends {
			pool.Backends[i].Weight = 1 + i%4
		}
		p, _ := New(kind, pool)
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 500; i++ {
					if _, err := p.Pick(context.Background(), PickHint{}); err != nil {
						t.Errorf("pick: %v", err)
						return
					}
				}
			}()
		}
		wg.Wait()
	}
}

func TestUnknownKindRejected(t *testing.T) {
	if _, err := New("random", mkPool(2)); err == nil {
		t.Error("unknown picker kind must be rejected at construction")
	}
}
