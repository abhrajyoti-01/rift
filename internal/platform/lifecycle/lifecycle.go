package lifecycle

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// Phase is the observable service lifecycle state.
type Phase int32

const (
	PhaseInit Phase = iota
	PhaseStarting
	PhaseServing
	PhaseDraining
	PhaseFlushing
	PhaseStopped
)

// String renders the closed phase label set (health endpoints, logs).
func (p Phase) String() string {
	switch p {
	case PhaseInit:
		return "init"
	case PhaseStarting:
		return "starting"
	case PhaseServing:
		return "serving"
	case PhaseDraining:
		return "draining"
	case PhaseFlushing:
		return "flushing"
	case PhaseStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Service is one supervised unit.
type Service interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// App supervises an ordered set of Services through the Phase sequence.
type App struct {
	name     string
	services []Service
	phase    atomic.Int32
	mu       sync.Mutex
	// shutdownTotal is the config-driven shutdown budget. Read once at
	// shutdown time.
	shutdownTotal time.Duration
}

// New builds an App over services, started in order and stopped in reverse.
// The default shutdown budget is 15s.
func New(name string, services ...Service) *App {
	a := &App{name: name, services: services, shutdownTotal: 15 * time.Second}
	a.phase.Store(int32(PhaseInit))
	return a
}

// SetShutdownTimeout sets the total shutdown budget (config
// shutdown_timeout). Must be called before Run; panics otherwise — a
// budget change mid-shutdown is a race by definition.
func (a *App) SetShutdownTimeout(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Phase() != PhaseInit && a.Phase() != PhaseStarting {
		panic("lifecycle: SetShutdownTimeout after Run")
	}
	a.shutdownTotal = d
}

// Phase reports the current lifecycle phase.
func (a *App) Phase() Phase { return Phase(a.phase.Load()) }

// setPhase transitions atomically; only forward transitions are legal.
func (a *App) setPhase(p Phase) {
	a.phase.Store(int32(p))
}

// ShutdownBudget splits the total shutdown budget T:
// stop-accepting is immediate; drain receives T − 1s − 5%; flush receives
// the remainder.
func ShutdownBudget(total time.Duration) (drain, flush time.Duration) {
	if total <= 0 {
		total = 15 * time.Second
	}
	drain = total - time.Second - total/20
	if drain < 0 {
		drain = total / 2
	}
	flush = total - drain
	if flush < 0 {
		flush = 0
	}
	return drain, flush
}

// Run starts all services in order, blocks until a fatal error or
// shutdown signal (SIGINT/SIGTERM), then drives ordered shutdown and
// returns the terminal error. A service failing Start aborts startup:
// already-started services are stopped (reverse order) before returning.
func (a *App) Run(ctx context.Context) error {
	return a.RunWithSignal(ctx, func() <-chan os.Signal {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		return sig
	})
}

// Flusher is the optional post-drain hook:
// services holding segment writers or metric buffers implement Flush to
// durably close them within the flush window after Stop returned.
type Flusher interface {
	Flush(ctx context.Context) error
}

// RunWithSignal is Run with an injectable signal source (tests drive it
// with a plain channel — no real signals in tests).
func (a *App) RunWithSignal(ctx context.Context, signalSource func() <-chan os.Signal) error {
	a.setPhase(PhaseStarting)

	// Start services in order; abort on first failure.
	started := make([]Service, 0, len(a.services))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, svc := range a.services {
		if err := svc.Start(runCtx); err != nil {
			// Roll back: stop what started, reverse order, bounded.
			a.stopStarted(started, 5*time.Second)
			a.setPhase(PhaseStopped)
			return errs.Wrap(err, errs.ClassResource, "lifecycle",
				"service "+svc.Name()+" failed to start")
		}
		started = append(started, svc)
	}

	a.setPhase(PhaseServing)

	sig := signalSource()
	select {
	case <-ctx.Done():
	case <-sig:
	}

	// Stop each service exactly once in reverse order within the drain budget,
	// then flush services that implement Flusher.
	a.setPhase(PhaseDraining)
	a.mu.Lock()
	total := a.shutdownTotal
	a.mu.Unlock()
	drainBudget, flushBudget := ShutdownBudget(total)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainBudget)
	defer drainCancel()
	drainErr := a.stopAll(drainCtx)

	a.setPhase(PhaseFlushing)
	flushCtx, flushCancel := context.WithTimeout(context.Background(), flushBudget)
	defer flushCancel()
	for i := len(a.services) - 1; i >= 0; i-- {
		if f, ok := a.services[i].(Flusher); ok {
			f.Flush(flushCtx)
		}
	}

	a.setPhase(PhaseStopped)

	if drainErr != nil {
		return errors.Join(errs.New(errs.ClassResource, "lifecycle",
			"drain deadline exceeded"), ErrDrainExceeded)
	}
	return nil
}

// stopAll stops every service in reverse order under the ctx budget. A
// service exceeding the budget is reported and does not block the rest.
func (a *App) stopAll(ctx context.Context) error {
	a.mu.Lock()
	services := append([]Service(nil), a.services...)
	a.mu.Unlock()

	var firstErr error
	for i := len(services) - 1; i >= 0; i-- {
		svc := services[i]
		done := make(chan error, 1)
		go func() { done <- svc.Stop(ctx) }()
		select {
		case err := <-done:
			if err != nil && firstErr == nil {
				firstErr = err
			}
		case <-ctx.Done():
			// Budget exhausted mid-stop: record and continue with a
			// zero-budget ctx so remaining services still see cancellation
			// rather than being skipped silently.
			if firstErr == nil {
				firstErr = errs.New(errs.ClassResource, "lifecycle",
					"stop budget exceeded at service "+svc.Name())
			}
		}
	}
	return firstErr
}

// stopStarted rolls back partially-completed startup (reverse order).
func (a *App) stopStarted(started []Service, budget time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	for i := len(started) - 1; i >= 0; i-- {
		started[i].Stop(ctx)
	}
}

// ErrDrainExceeded mirrors the errs sentinel for API symmetry in this
// package; the canonical definition is errs.ErrDrainExceeded.
var ErrDrainExceeded = errs.ErrDrainExceeded
