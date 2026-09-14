package errs

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassStringClosedSet(t *testing.T) {
	cases := map[Class]string{
		ClassUnknown:    "unknown",
		ClassNetwork:    "network",
		ClassTimeout:    "timeout",
		ClassPeerClosed: "peer_closed",
		ClassConfig:     "config",
		ClassSecurity:   "security",
		ClassResource:   "resource",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Errorf("Class(%d).String() = %q, want %q", c, got, want)
		}
	}
	// Out-of-range values must land in the closed default, never free-form
	// strings that leak into metric labels (OBSERVABILITY_SPEC §3.4).
	if got := Class(200).String(); got != "unknown" {
		t.Errorf("out-of-range Class.String() = %q, want %q", got, "unknown")
	}
}

func TestErrorFormatting(t *testing.T) {
	cause := errors.New("connection reset")
	e := Wrap(cause, ClassNetwork, "lb.l4.copyLoop", "upstream write failed")
	want := "lb.l4.copyLoop: upstream write failed: connection reset"
	if e.Error() != want {
		t.Errorf("Error() = %q, want %q", e.Error(), want)
	}
	if !errors.Is(e, cause) {
		t.Error("Wrap must keep the cause discoverable via errors.Is")
	}
	if Unwrap := e.Unwrap(); Unwrap != cause {
		t.Errorf("Unwrap() = %v, want %v", Unwrap, cause)
	}

	n := New(ClassConfig, "cli.config", "weight must be >= 1")
	if n.Error() != "cli.config: weight must be >= 1" {
		t.Errorf("New().Error() = %q", n.Error())
	}
}

func TestClassOfWalksChain(t *testing.T) {
	inner := New(ClassTimeout, "dns.resolver", "deadline exceeded")
	mid := fmt.Errorf("wrap: %w", inner)
	outer := fmt.Errorf("outer: %w", mid)
	if got := ClassOf(outer); got != ClassTimeout {
		t.Errorf("ClassOf(outer) = %v, want ClassTimeout", got)
	}
	if got := ClassOf(nil); got != ClassUnknown {
		t.Errorf("ClassOf(nil) = %v, want ClassUnknown", got)
	}
	if got := ClassOf(errors.New("plain")); got != ClassUnknown {
		t.Errorf("ClassOf(plain) = %v, want ClassUnknown", got)
	}
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{New(ClassConfig, "cli", "invalid"), 2},
		{New(ClassResource, "netx", "port in use"), 3},
		{ErrDrainExceeded, 4},
		{fmt.Errorf("wrapped: %w", ErrDrainExceeded), 4},
		{ErrNotImplemented, 1},
		{New(ClassNetwork, "lb", "dial failed"), 1},
		{errors.New("plain error"), 1},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Errorf("ExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}
