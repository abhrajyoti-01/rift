package lifecycle

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

type mockService struct {
	name      string
	startErr  error
	stopErr   error
	stopBlock time.Duration

	mu      sync.Mutex
	started bool
	stopped int
}

func (m *mockService) Name() string { return m.name }
func (m *mockService) Start(ctx context.Context) error {
	m.mu.Lock()
	m.started = true
	m.mu.Unlock()
	return m.startErr
}
func (m *mockService) Stop(ctx context.Context) error {
	if m.stopBlock > 0 {
		select {
		case <-time.After(m.stopBlock):
		case <-ctx.Done():
		}
	}
	m.mu.Lock()
	m.stopped++
	m.mu.Unlock()
	return m.stopErr
}

func TestPhaseSequenceAndOrderedStop(t *testing.T) {
	a := New("test",
		&mockService{name: "first"},
		&mockService{name: "second"},
		&mockService{name: "third"},
	)
	if a.Phase() != PhaseInit {
		t.Fatalf("initial phase = %s, want init", a.Phase())
	}

	sig := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- a.RunWithSignal(context.Background(), func() <-chan os.Signal { return sig })
	}()

	deadline := time.After(3 * time.Second)
	for a.Phase() != PhaseServing {
		select {
		case <-deadline:
			t.Fatalf("never reached serving (phase %s)", a.Phase())
		case <-time.After(5 * time.Millisecond):
		}
	}
	sig <- os.Interrupt
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if a.Phase() != PhaseStopped {
		t.Errorf("final phase = %s, want stopped", a.Phase())
	}
	for _, svc := range a.services {
		m := svc.(*mockService)
		if !m.started {
			t.Errorf("%s never started", m.name)
		}
		// Stop must be called exactly once per service: calling it twice
		// (for example once for "drain" and again for "flush") is a real
		// bug that this assertion catches.
		if m.stopped != 1 {
			t.Errorf("%s Stop called %d times, want exactly 1", m.name, m.stopped)
		}
	}
}

func TestStartupFailureRollsBack(t *testing.T) {
	first := &mockService{name: "first"}
	second := &mockService{name: "second", startErr: errs.New(errs.ClassConfig, "t", "bad config")}
	third := &mockService{name: "third"}
	a := New("test", first, second, third)

	err := a.RunWithSignal(context.Background(), func() <-chan os.Signal {
		return make(chan os.Signal) // never fires
	})
	if err == nil {
		t.Fatal("startup failure must return an error")
	}
	if errs.ClassOf(err) != errs.ClassResource {
		t.Errorf("class = %v, want ClassResource", errs.ClassOf(err))
	}
	if !first.started {
		t.Error("first service should have started")
	}
	if first.stopped != 1 {
		t.Error("rollback must stop the started service exactly once")
	}
	if third.started {
		t.Error("a service after the failure must not start")
	}
}

// TestDrainDeadlineExitsFour: a service blocking past the shutdown budget
// must produce ErrDrainExceeded, which maps to exit code 4.
func TestDrainDeadlineExitsFour(t *testing.T) {
	blocking := &mockService{name: "blocker", stopBlock: 30 * time.Second}
	a := New("test", blocking)
	a.SetShutdownTimeout(300 * time.Millisecond) // fast test; budget math is separate

	sig := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- a.RunWithSignal(context.Background(), func() <-chan os.Signal { return sig })
	}()
	for a.Phase() != PhaseServing {
		time.Sleep(5 * time.Millisecond)
	}
	sig <- os.Interrupt

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a blocked drain must error, not hang")
		}
		if !errs.MatchDrain(err) {
			t.Errorf("expected ErrDrainExceeded, got %v", err)
		}
		if got := errs.ExitCode(err); got != 4 {
			t.Errorf("ExitCode = %d, want 4", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run hung past the shutdown budget")
	}
}

func TestShutdownBudgetSplit(t *testing.T) {
	drain, flush := ShutdownBudget(15 * time.Second)
	if drain <= 0 || flush <= 0 {
		t.Fatalf("split must be positive: drain=%v flush=%v", drain, flush)
	}
	if drain+flush > 15*time.Second {
		t.Errorf("split %v+%v exceeds the total", drain, flush)
	}
	// Documented formula: drain = T - 1s - 5 percent.
	want := 15*time.Second - time.Second - 750*time.Millisecond
	if drain != want {
		t.Errorf("drain = %v, want %v", drain, want)
	}
	// Degenerate input must stay sane rather than going negative.
	d, f := ShutdownBudget(0)
	if d <= 0 || f < 0 {
		t.Errorf("zero-total split degenerate: %v / %v", d, f)
	}
}

func TestStopOrderIsReverse(t *testing.T) {
	var order []string
	var mu sync.Mutex
	rec := func(name string) Service {
		return &recordingService{name: name, order: &order, mu: &mu}
	}
	a := New("test", rec("a"), rec("b"), rec("c"))

	sig := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- a.RunWithSignal(context.Background(), func() <-chan os.Signal { return sig })
	}()
	for a.Phase() != PhaseServing {
		time.Sleep(5 * time.Millisecond)
	}
	sig <- os.Interrupt
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{"c", "b", "a"}
	for i, w := range want {
		if i >= len(order) || order[i] != w {
			t.Fatalf("stop order = %v, want %v", order, want)
		}
	}
}

type recordingService struct {
	name  string
	order *[]string
	mu    *sync.Mutex
}

func (r *recordingService) Name() string                     { return r.name }
func (r *recordingService) Start(ctx context.Context) error  { return nil }
func (r *recordingService) Stop(ctx context.Context) error {
	r.mu.Lock()
	*r.order = append(*r.order, r.name)
	r.mu.Unlock()
	return nil
}

func TestPhaseStringClosedSet(t *testing.T) {
	cases := map[Phase]string{
		PhaseInit: "init", PhaseStarting: "starting", PhaseServing: "serving",
		PhaseDraining: "draining", PhaseFlushing: "flushing", PhaseStopped: "stopped",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("Phase(%d) = %q, want %q", p, got, want)
		}
	}
	if got := Phase(99).String(); got != "unknown" {
		t.Errorf("out-of-range Phase = %q, want unknown", got)
	}
}

func TestContextCancelStopsApp(t *testing.T) {
	a := New("test", &mockService{name: "svc"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.RunWithSignal(ctx, func() <-chan os.Signal { return make(chan os.Signal) })
	}()
	for a.Phase() != PhaseServing {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on context cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}