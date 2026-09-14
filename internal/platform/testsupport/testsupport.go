// Package testsupport provides real-socket fakes and fault injectors for
// the whole tree (TESTING_SPEC §1): Echo, Blackhole, SlowConn, Chaos, and
// the goleak TestMain wiring. Fault tests that only assert "no panic" are
// rejected in review — these helpers exist so they can assert
// classification, teardown, and observability instead.
//
// Phase 1 (ROADMAP.md). This file pins the public contract; the fixtures
// and injectors land with the phases that consume them.
package testsupport

import "net"

// Echo is a real-socket TCP echo backend for LB byte-equivalence tests.
type Echo struct {
	// Phase 2 (ROADMAP.md): net.Listener + accept loop.
}

// StartEcho binds an Echo on 127.0.0.1:0 and returns it.
func StartEcho() (*Echo, error) {
	// Phase 2 (ROADMAP.md).
	return &Echo{}, nil
}

// Addr is the bound address.
func (e *Echo) Addr() net.Addr {
	// Phase 2 (ROADMAP.md).
	return nil
}

// Close stops the listener and waits for in-flight echoes to finish.
func (e *Echo) Close() error {
	// Phase 2 (ROADMAP.md).
	return nil
}
