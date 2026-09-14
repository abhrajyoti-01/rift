package lifecycle

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rift/rift/internal/platform/errs"
)

// mockService records lifecycle calls with optional failures.
type mockService struct {
	name      string
	startErr  error
	stopErr   error
	stopBlock time.Duration

	mu     sync.Mutex
	started bool
	stopped bool
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
	m.stopped = true
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
		t.Fatalf("initial phase %s, want init", a.Phase())
	}

	sig := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() { done <- a.RunWithSignal(context.Background(), func() <-chan os.Signal { return sig }) }()

	// Wait for serving.
	deadline := time.After(2 * time.Second)
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
		t.Errorf("final phase %s, want stopped", a.Phase())
	}
	for i, svc := range a.services {
		m := svc.(*mockService)
		if !m.started || !m.stopped {
			t.Errorf("service %d (%s): started=%v stopped=%v", i, m.name, m.started, m.stopped)
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
		t.Errorf("class %v, want ClassResource", errs.ClassOf(err))
	}
	if !first.started {
		t.Error("first service should have started")
	}
	if first.stopped != true {
		t.Error("rollback must stop the started service")
	}
	if third.started {
		t.Error("third service must not start after second's failure")
	}
}

func TestDrainDeadlineExitsFour(t *testing.T) {
	// A service whose Stop blocks past the whole shutdown budget must
	// produce ErrDrainExceeded → exit code 4 (CLI_SPEC §5). The default
	// budget is 15s; inject a small one so the test is fast (budget math
	// itself is covered by TestShutdownBudgetSplit).
	blocking := &mockService{name: "blocker", stopBlock: 30 * time.Second}
	a := New("test", blocking)
	a.SetShutdownTimeout(500 * time.Millisecond)

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
			t.Fatal("blocked drain must error, not hang")
		}
		if !errs.MatchDrain(err) {
			t.Errorf("expected ErrDrainExceeded, got %v", err)
		}
		if got := errs.ExitCode(err); got != 4 {
			t.Errorf("ExitCode = %d, want 4", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run hung past the shutdown budget — the exact bug this package exists to prevent")
	}
}

func TestShutdownBudgetSplit(t *testing.T) {
	drain, flush := ShutdownBudget(15 * time.Second)
	if drain <= 0 || flush <= 0 {
		t.Fatalf("split must be positive: drain=%v flush=%v", drain, flush)
	}
	if drain+flush > 15*time.Second {
		t.Errorf("split %v+%v exceeds total 15s", drain, flush)
	}
	// Documented formula: drain = T − 1s − 5 percent.
	if want := 15*time.Second - time.Second - 750*time.Millisecond; drain != want {
		t.Errorf("drain = %v, want %v (T minus 1s minus 5 percent)", drain, want)
	}
	// Degenerate totals stay sane.
	d, f := ShutdownBudget(0)
	if d <= 0 || f < 0 {
		t.Errorf("zero-total split degenerate: %v %v", d, f)
	}
}

func TestStopOrderIsReverse(t *testing.T) {
	var order []string
	var mu sync.Mutex
	svc := func(name string) Service {
		return &mockService{name: name, stopBlock: 0, startErr: nil, stopErr: nil, mu: sync.Mutex{}}
	}
	_ = svc
	// Use a recording wrapper to capture exact order.
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
		if order[i] != w {
			t.Errorf("stop order[%d] = %s, want %s (full order: %v)", i, order[i], w, order)
		}
	}
}

type recordingService struct {
	name  string
	order *[]string
	mu    *sync.Mutex
}

func (r *recordingService) Name() string { return r.name }
func (r *recordingService) Start(ctx context.Context) error { return nil }
func (r *recordingService) Stop(ctx context.Context) error {
	r.mu.Lock()
	*r.order = append(*r.order, r.name)
	r.mu.Unlock()
	return nil
}
