// Package errs is RIFT's single error taxonomy. Class drives exactly three
// things — log level, metric label, retry eligibility — and nothing else
// (TECHNICAL_SPEC §1). Construct errors only via New/Wrap on data-plane
// paths; ClassOf is the only classifier in the tree.
package errs

import (
	"errors"
	"fmt"
)

// Class classifies an error for logging level, metric labeling, and retry
// eligibility. It is a small int: free on the hot path, Prometheus-safe as
// a label value.
type Class uint8

const (
	// ClassUnknown must be reported and is never silently retried.
	ClassUnknown Class = iota
	// ClassNetwork is a transport failure, retryable per policy.
	ClassNetwork
	// ClassTimeout is a deadline exceeded; its metric is distinct from
	// ClassNetwork so slow answers and dropped packets are never conflated.
	ClassTimeout
	// ClassPeerClosed is a normal EOF/RST, not a fault; excluded from
	// error-rate SLOs.
	ClassPeerClosed
	// ClassConfig is a startup-time config fault; exit code 2.
	ClassConfig
	// ClassSecurity is a deny-set hit, oversize, or smuggling attempt;
	// never retried.
	ClassSecurity
	// ClassResource means a limit was reached; shed load, never queue
	// unbounded.
	ClassResource
)

// String returns the closed label value used in metrics and logs.
func (c Class) String() string {
	switch c {
	case ClassNetwork:
		return "network"
	case ClassTimeout:
		return "timeout"
	case ClassPeerClosed:
		return "peer_closed"
	case ClassConfig:
		return "config"
	case ClassSecurity:
		return "security"
	case ClassResource:
		return "resource"
	default:
		return "unknown"
	}
}

// Error carries one Class plus wrapped context.
type Error struct {
	Class Class
	Op    string // dotted operation path, e.g. "lb.l4.copyLoop"
	Msg   string
	Err   error // wrapped; may be nil
}

// New constructs an Error with no wrapped cause.
func New(class Class, op, msg string) *Error {
	return &Error{Class: class, Op: op, Msg: msg}
}

// Wrap constructs an Error carrying an underlying cause.
func Wrap(err error, class Class, op, msg string) *Error {
	return &Error{Class: class, Op: op, Msg: msg, Err: err}
}

func (e *Error) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s: %s", e.Op, e.Msg)
	}
	return fmt.Sprintf("%s: %s: %v", e.Op, e.Msg, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// ClassOf returns the Class of err, searching the chain for an *Error.
// ClassUnknown if none is present. This is the only classifier in the tree;
// error-string matching is a CI-grep violation (T-34).
func ClassOf(err error) Class {
	if err == nil {
		return ClassUnknown
	}
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e.Class
		}
		err = errors.Unwrap(err)
	}
	return ClassUnknown
}

// ErrNotImplemented is returned by cmd/rift for subcommands whose ROADMAP
// phase has not landed (TECHNICAL_SPEC §1). A CI grep asserts it does not
// survive to v1; no stub silently pretends to work instead of returning it.
var ErrNotImplemented = errors.New("not implemented in this phase — see ROADMAP.md")

// ErrDrainExceeded is raised by lifecycle when the drain budget fires
// (TECHNICAL_SPEC §13). ExitCode maps it to 4.
var ErrDrainExceeded = errors.New("drain deadline exceeded")

// MatchDrain reports whether err is (or wraps) ErrDrainExceeded — the
// chain-tolerant check for the exit-4 condition.
func MatchDrain(err error) bool {
	return errors.Is(err, ErrDrainExceeded)
}

// ExitCode maps an error to a process exit code per CLI_SPEC §5: 0 clean,
// 1 runtime fault, 2 config fault at startup, 3 bind/resource failure,
// 4 drain deadline exceeded. 124 is reserved for bench/harness and is never
// returned here.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrDrainExceeded):
		return 4
	case errors.Is(err, ErrNotImplemented):
		return 1
	}
	switch ClassOf(err) {
	case ClassConfig:
		return 2
	case ClassResource:
		return 3
	default:
		return 1
	}
}
