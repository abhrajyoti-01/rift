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
	e.Run(ctx, func(ctx context.Context, n int) { processed.Add(1) })

	const work = 500
	submitted := 0
	for submitted < work {
		if err := e.Submit(submitted); err == ErrFull {
			// Backpressure contract: ErrFull is a visible "not now", so the
			// caller retries or sheds. It is not a failure.
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
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSubmitNeverBlocks is the backpressure contract: a full pool refuses
// promptly rather than queueing invisibly.
func TestSubmitNeverBlocks(t *testing.T) {
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

func TestClosedRejects(t *testing.T) {
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
	if err := e.Close(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close = %v, want ErrClosed", err)
	}
}

// TestCloseDrainsQueuedWork: Close must let buffered work finish, not drop it.
func TestCloseDrainsQueuedWork(t *testing.T) {
	e := NewExecutor[int](2, 128)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var processed atomic.Int64
	e.Run(ctx, func(ctx context.Context, n int) { processed.Add(1) })

	for i := 0; i < 50; i++ {
		if err := e.Submit(i); err == ErrFull {
			break
		}
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer closeCancel()
	if err := e.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if processed.Load() == 0 {
		t.Error("no work processed during drain")
	}
}

func TestCloseBudgetReportsLeftovers(t *testing.T) {
	// Work that never finishes: Close's budget must fire and name the
	// leftover count rather than hanging or silently dropping.
	e := NewExecutor[int](1, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	block := make(chan struct{})
	defer close(block)
	e.Run(ctx, func(ctx context.Context, n int) { <-block })

	for i := 0; i < 10; i++ {
		if err := e.Submit(i); err == ErrFull {
			break
		}
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer closeCancel()
	err := e.Close(closeCtx)
	if err == nil {
		t.Fatal("expected a drain-budget error")
	}
	if !containsSub(err.Error(), "undrained") {
		t.Errorf("Close error should name leftovers: %v", err)
	}
}

func TestConcurrentSubmitAndClose(t *testing.T) {
	e := NewExecutor[int](8, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var processed atomic.Int64
	e.Run(ctx, func(ctx context.Context, n int) { processed.Add(1) })

	var wg sync.WaitGroup
	for p := 0; p < 16; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if err := e.Submit(i); err != nil && err != ErrFull && !errors.Is(err, ErrClosed) {
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
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}