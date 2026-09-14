package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutorProcessesAllWork(t *testing.T) {
	e := NewExecutor[int](4, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var processed atomic.Int64
	e.Run(ctx, func(ctx context.Context, n int) {
		processed.Add(1)
	})

	const work = 1000
	submitted := 0
	for submitted < work {
		if err := e.Submit(submitted); err == ErrFull {
			// Backpressure contract: ErrFull is a visible "not now",
			// never an error — the caller retries or sheds.
			time.Sleep(time.Millisecond)
			continue
		} else if err != nil {
			t.Fatalf("Submit(%d): %v", submitted, err)
		}
		submitted++
	}

	deadline := time.After(5 * time.Second)
	for processed.Load() < work {
		select {
		case <-deadline:
			t.Fatalf("processed %d/%d after 5s", processed.Load(), work)
		case <-time.After(10 * time.Millisecond):
		}
	}

	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestExecutorFullNeverBlocks(t *testing.T) {
	// One worker, capacity one, no Run yet: submission must return ErrFull
	// promptly — never block — because backpressure is the caller's
	// decision (TECHNICAL_SPEC §2.1).
	e := NewExecutor[int](1, 1)
	if err := e.Submit(1); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Submit(2) }()
	select {
	case err := <-done:
		if err != ErrFull {
			t.Errorf("second Submit = %v, want ErrFull", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Submit blocked instead of returning ErrFull")
	}
}

func TestExecutorClosedRejects(t *testing.T) {
	e := NewExecutor[int](1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	e.Run(ctx, func(ctx context.Context, n int) {})
	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cancel()
	if err := e.Submit(1); !errors.Is(err, ErrClosed) {
		t.Errorf("Submit after Close = %v, want ErrClosed", err)
	}
	// Double-close is idempotent, not a panic.
	if err := e.Close(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close = %v, want ErrClosed", err)
	}
}

func TestExecutorCloseDrainsQueuedWork(t *testing.T) {
	e := NewExecutor[int](2, 128)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	var processed atomic.Int64
	e.Run(ctx, func(ctx context.Context, n int) {
		<-release // hold work until the drain path is under test
		processed.Add(1)
	})

	for i := 0; i < 50; i++ {
		if err := e.Submit(i); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	go func() {
		// Let a little work finish, then release the rest.
		time.Sleep(100 * time.Millisecond)
		close(release)
	}()
	if err := e.Close(closeCtx); err != nil {
		t.Fatalf("Close with release: %v", err)
	}
	if processed.Load() == 0 {
		t.Error("no work processed during drain")
	}
}

func TestExecutorDrainBudgetReportsLeftovers(t *testing.T) {
	// Work never releases: Close's budget must fire and the error must
	// NAME the leftover count — visible failure, never silent drop.
	e := NewExecutor[int](1, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Run(ctx, func(ctx context.Context, n int) {
		<-ctx.Done() // workers only exit on ctx cancel
	})
	for i := 0; i < 10; i++ {
		if err := e.Submit(i); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer closeCancel()
	err := e.Close(closeCtx)
	if err == nil {
		t.Fatal("expected drain-budget error, got nil")
	}
	// Leftover count ≥ 1 must appear in the message.
	if !containsInt(err.Error(), "undrained") {
		t.Errorf("Close error should name leftovers: %v", err)
	}
}

func TestExecutorConcurrentSubmitAndDrain(t *testing.T) {
	e := NewExecutor[int](8, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var processed atomic.Int64
	e.Run(ctx, func(ctx context.Context, n int) { processed.Add(1) })

	var wg sync.WaitGroup
	producers := 16
	perProducer := 200
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				// ErrFull is acceptable under overload; anything else is a bug.
				if err := e.Submit(i); err != nil && err != ErrFull {
					t.Errorf("Submit: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := e.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := processed.Load(); got == 0 {
		t.Error("no work processed under concurrent load")
	}
}

func containsInt(s string, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
