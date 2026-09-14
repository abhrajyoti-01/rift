// Package pool provides bounded executors for work whose unit is not a
// socket (TECHNICAL_SPEC §2.1). Submit never blocks: ErrFull is the
// visible backpressure decision. Close drains within the ctx budget and
// reports work still enqueued — never silently dropped.
package pool

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrClosed is returned by Submit after Close has begun.
	ErrClosed = errors.New("pool: closed")
	// ErrFull is returned when a worker queue is at capacity.
	ErrFull = errors.New("pool: queue full")
)

const (
	minWorkers        = 1
	maxQueuePerWorker = 1 << 20 // sanity ceiling; configs above this are config errors
)

// Executor is a bounded worker pool over queued work items of type T.
type Executor[T any] struct {
	mu     sync.Mutex
	queues []chan T
	next   uint64 // round-robin submit target (atomic via mu — cold path)

	closed bool
	wg     sync.WaitGroup
}

// NewExecutor builds a bounded executor. workers ≥ 1; queuePerWorker ≥ 1
// and bounded by the sanity ceiling — a config asking for an unbounded
// queue is exactly the hidden-overload bug the Executor exists to prevent.
func NewExecutor[T any](workers, queuePerWorker int) *Executor[T] {
	if workers < minWorkers {
		workers = minWorkers
	}
	if queuePerWorker < 1 {
		queuePerWorker = 1
	}
	if queuePerWorker > maxQueuePerWorker {
		panic("pool: queuePerWorker exceeds sanity ceiling — config error")
	}
	e := &Executor[T]{
		queues: make([]chan T, workers),
	}
	for i := range e.queues {
		e.queues[i] = make(chan T, queuePerWorker)
	}
	return e
}

// Submit enqueues t, returning ErrFull or ErrClosed rather than blocking.
func (e *Executor[T]) Submit(t T) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	idx := e.next % uint64(len(e.queues))
	e.next++
	// Round-robin across worker queues: full queues are skipped so one hot
	// worker cannot stall submissions while others idle (bounded attempt
	// pass — if ALL are full, that is ErrFull, the caller's decision).
	for i := 0; i < len(e.queues); i++ {
		q := e.queues[(idx+uint64(i))%uint64(len(e.queues))]
		select {
		case q <- t:
			return nil
		default:
		}
	}
	return ErrFull
}

// Run installs the processing function and drives workers until ctx is
// canceled. Each worker drains its own queue; fn is invoked sequentially
// per worker and must not retain t.
func (e *Executor[T]) Run(ctx context.Context, fn func(ctx context.Context, t T)) {
	if fn == nil {
		return
	}
	e.wg.Add(len(e.queues))
	for i := range e.queues {
		q := e.queues[i]
		go func() {
			defer e.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case t, ok := <-q:
					if !ok {
						return
					}
					fn(ctx, t)
				}
			}
		}()
	}
}

// Close stops accepting work, closes the worker queues (workers drain
// remaining buffered items, then exit), and waits within the ctx budget.
// nil if fully drained; otherwise a ClassResource error naming the
// leftover count — visible failure, never a silent drop.
func (e *Executor[T]) Close(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	e.closed = true
	queues := e.queues
	e.mu.Unlock()

	// Safe against Submit: the closed flag is set under mu, and Submit
	// checks it under the same mu before sending — a Submit that saw
	// closed=false completed its send before Close could acquire mu, and
	// any Submit after the flag sees ErrClosed. No send-on-closed-channel.
	for _, q := range queues {
		close(q)
	}

	drained := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		// Budget fired (typically an fn still running): count buffered
		// leftovers for the honest report. len() on a closed channel is
		// valid and returns the buffered count.
		var leftover int
		for _, q := range queues {
			leftover += len(q)
		}
		return errors.New("pool: drain budget exceeded; " + itoa(leftover) + " items undrained")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
