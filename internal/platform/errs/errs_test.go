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
	if got := Class(200).String(); got != "unknown" {
		t.Errorf("out-of-range Class.String() = %q, want unknown (labels must stay bounded)", got)
	}
}

func TestWrapKeepsCauseAndFormat(t *testing.T) {
	cause := errors.New("connection reset")
	e := Wrap(cause, ClassNetwork, "lb.l4.copyLoop", "upstream write failed")
	if want := "lb.l4.copyLoop: upstream write failed: connection reset"; e.Error() != want {
		t.Errorf("Error() = %q, want %q", e.Error(), want)
	}
	if !errors.Is(e, cause) {
		t.Error("Wrap must keep the cause discoverable")
	}
	if e.Unwrap() != cause {
		t.Error("Unwrap must return the cause")
	}
}

func TestClassOfWalksChain(t *testing.T) {
	inner := New(ClassTimeout, "dns.resolver", "deadline exceeded")
	outer := fmt.Errorf("outer: %w", fmt.Errorf("mid: %w", inner))
	if got := ClassOf(outer); got != ClassTimeout {
		t.Errorf("ClassOf = %v, want ClassTimeout", got)
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
		{errors.New("plain"), 1},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Errorf("ExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestMatchDrain(t *testing.T) {
	if !MatchDrain(fmt.Errorf("x: %w", ErrDrainExceeded)) {
		t.Error("MatchDrain must match a wrapped sentinel")
	}
	if MatchDrain(errors.New("other")) {
		t.Error("MatchDrain must not match unrelated errors")
	}
}