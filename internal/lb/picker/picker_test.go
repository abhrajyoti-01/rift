package picker

import (
	"context"
	"fmt"
	"sync"
	"testing"

	lbmodel "github.com/abhrajyoti-01/rift/internal/lb/model"
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

func TestRoundRobinUniform(t *testing.T) {
	const n, picks = 8, 8000
	p, err := New(RoundRobin, mkPool(n))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i := 0; i < picks; i++ {
		b, err := p.Pick(context.Background(), PickHint{})
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		counts[b.ID]++
	}
	var chi2 float64
	expected := float64(picks) / float64(n)
	for _, c := range counts {
		d := float64(c) - expected
		chi2 += d * d / expected
	}
	if chi2 > 18.48 { // χ²(7df, p=0.01)
		t.Errorf("χ² = %.2f exceeds critical value; distribution %v", chi2, counts)
	}
}

func TestRoundRobinSkipsUnhealthy(t *testing.T) {
	pool := mkPool(3)
	pool.Backends[1].Health.Store(false)
	p, _ := New(RoundRobin, pool)
	for i := 0; i < 10; i++ {
		b, err := p.Pick(context.Background(), PickHint{})
		if err != nil {
			t.Fatal(err)
		}
		if b.ID == "b1" {
			t.Fatalf("unhealthy backend picked at iteration %d", i)
		}
	}
}

func TestAllUnhealthyIsErrNoUpstream(t *testing.T) {
	for _, kind := range []string{RoundRobin, WeightedRR, LeastConns} {
		pool := mkPool(3)
		for _, b := range pool.Backends {
			b.Health.Store(false)
		}
		p, err := New(kind, pool)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if _, err := p.Pick(context.Background(), PickHint{}); err == nil {
			t.Errorf("%s: all-unhealthy must yield ErrNoUpstream", kind)
		}
	}
}

func TestSmoothWRRInterleaveAndProportion(t *testing.T) {
	// Weights 5:3:1 over 9 picks must hit 5/3/1 AND interleave rather than
	// burst 5 consecutive picks at the heavy backend.
	p, err := New(WeightedRR, mkPool(3, 5, 3, 1))
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
		t.Errorf("cycle counts = %v, want 5/3/1 (seq %v)", counts, seq)
	}
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

func TestSmoothWRRSkipsUnhealthyRedistributes(t *testing.T) {
	pool := mkPool(3, 5, 3, 1)
	pool.Backends[0].Health.Store(false) // remove the weight-5 backend
	p, _ := New(WeightedRR, pool)
	c := map[string]int{}
	for i := 0; i < 12; i++ {
		b, _ := p.Pick(context.Background(), PickHint{})
		c[b.ID]++
	}
	if c["b0"] != 0 {
		t.Errorf("unhealthy backend picked %d times", c["b0"])
	}
	// Effective weights 3:1 → 9:3 over 12 picks.
	if c["b1"] != 9 || c["b2"] != 3 {
		t.Errorf("redistributed 3:1 should be 9/3, got %v", c)
	}
}

func TestLeastConnsPicksMinimum(t *testing.T) {
	pool := mkPool(4)
	pool.Backends[0].Conns.Store(5)
	pool.Backends[1].Conns.Store(1)
	pool.Backends[2].Conns.Store(3)
	pool.Backends[3].Conns.Store(1)
	p, _ := New(LeastConns, pool)
	b, err := p.Pick(context.Background(), PickHint{})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "b1" && b.ID != "b3" {
		t.Errorf("picked %s with conns 5,3,1,1 — must be a minimum", b.ID)
	}
}

func TestLeastConnsSpreadsTies(t *testing.T) {
	pool := mkPool(3)
	p, _ := New(LeastConns, pool)
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		b, err := p.Pick(context.Background(), PickHint{})
		if err != nil {
			t.Fatal(err)
		}
		seen[b.ID] = true
		b.Conns.Add(1)
	}
	if len(seen) != 3 {
		t.Errorf("tied backends: only %d of 3 ever picked (%v)", len(seen), seen)
	}
}

func TestLeastConnsFollowsLoadShift(t *testing.T) {
	pool := mkPool(3)
	pool.Backends[0].Conns.Store(0)
	pool.Backends[1].Conns.Store(10)
	pool.Backends[2].Conns.Store(20)
	p, _ := New(LeastConns, pool)
	b0, _ := p.Pick(context.Background(), PickHint{})
	if b0.ID != "b0" {
		t.Fatalf("first pick %s, want b0", b0.ID)
	}
	pool.Backends[0].Conns.Store(15)
	pool.Backends[1].Conns.Store(2)
	b1, _ := p.Pick(context.Background(), PickHint{})
	if b1.ID != "b1" {
		t.Fatalf("after load shift pick %s, want b1", b1.ID)
	}
}

func TestPickerAllocFree(t *testing.T) {
	// NFR-2: zero allocations on the pick path.
	for _, kind := range []string{RoundRobin, WeightedRR, LeastConns} {
		pool := mkPool(64)
		for i := range pool.Backends {
			pool.Backends[i].Weight = 1 + i%5
			pool.Backends[i].Conns.Store(int64(i % 10))
		}
		p, err := New(kind, pool)
		if err != nil {
			t.Fatal(err)
		}
		if n := testing.AllocsPerRun(200, func() {
			_, _ = p.Pick(context.Background(), PickHint{})
		}); n > 0 {
			t.Errorf("%s: %.0f allocs/pick, want 0", kind, n)
		}
	}
}

func TestPickerConcurrentSafety(t *testing.T) {
	for _, kind := range []string{RoundRobin, WeightedRR, LeastConns} {
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
	if _, err := New("random_pick", mkPool(2)); err == nil {
		t.Error("unknown picker kind must be rejected at construction")
	}
}

func BenchmarkPick1000(b *testing.B) {
	pool := mkPool(1000)
	for _, kind := range []string{RoundRobin, WeightedRR, LeastConns} {
		p, err := New(kind, pool)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(kind, func(b *testing.B) {
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := p.Pick(ctx, PickHint{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
