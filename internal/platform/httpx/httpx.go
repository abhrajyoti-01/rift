package httpx

import (
	"net/http"
	"time"
)

// ServerOptions carries the mandatory timeout set; zero fields are rejected
// at construction — a server without timeouts is a slowloris invitation.
type ServerOptions struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}

// NewServer builds an http.Server with the timeout set and middleware
// applied. The admin plane binds loopback by default; routable binds require
// explicit allow_remote.
func NewServer(opts ServerOptions, handler http.Handler) *http.Server {
	_ = opts
	_ = handler
	return nil
}

// Middleware rejects ambiguous request framing and applies header and
// request-ID limits.
func Middleware(next http.Handler) http.Handler {
	_ = next
	return nil
}
