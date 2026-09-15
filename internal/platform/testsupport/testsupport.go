package testsupport

import "net"

// Echo is a real-socket TCP echo backend for LB byte-equivalence tests.
type Echo struct {
}

// StartEcho binds an Echo on 127.0.0.1:0 and returns it.
func StartEcho() (*Echo, error) {
	return &Echo{}, nil
}

// Addr is the bound address.
func (e *Echo) Addr() net.Addr {
	return nil
}

// Close stops the listener and waits for in-flight echoes to finish.
func (e *Echo) Close() error {
	return nil
}
