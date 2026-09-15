package dnsnode

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dnsmodel "github.com/abhrajyoti-01/rift/internal/dnsmon/model"
	"github.com/abhrajyoti-01/rift/internal/dnsmon/wire"
	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// TestHubClientRejectsPartialMTLSConfig: providing client material without
// the CA (or the reverse) must fail loudly rather than shipping in a silently
// degraded mode.
func TestHubClientRejectsPartialMTLSConfig(t *testing.T) {
	cases := []NodeConfig{
		{ClientCertFile: "c.pem"},
		{ClientKeyFile: "k.pem"},
		{CACertFile: "ca.pem"},
		{ClientCertFile: "c.pem", ClientKeyFile: "k.pem"},
		{CACertFile: "ca.pem", ClientCertFile: "c.pem"},
	}
	for i, cfg := range cases {
		if _, err := newHubClient(cfg); err == nil {
			t.Errorf("case %d: partial mTLS material must be rejected", i)
		} else if errs.ClassOf(err) != errs.ClassConfig {
			t.Errorf("case %d: class = %v, want ClassConfig", i, errs.ClassOf(err))
		}
	}
}

// TestHubClientPlaintextWhenUnconfigured: no material means a plain client,
// which is what a development hub needs.
func TestHubClientPlaintextWhenUnconfigured(t *testing.T) {
	c, err := newHubClient(NodeConfig{})
	if err != nil {
		t.Fatalf("plaintext client should build: %v", err)
	}
	if c == nil {
		t.Fatal("nil client")
	}
	if c.Transport != nil {
		t.Error("no material should leave the default transport in place")
	}
}

// TestHubClientRejectsEmptyCA: a CA file with no usable certificates must be
// an error, not an empty trust store that silently trusts nothing (or worse,
// is bypassed).
func TestHubClientRejectsEmptyCA(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "c.pem")
	key := filepath.Join(dir, "k.pem")
	ca := filepath.Join(dir, "ca.pem")
	// Generate a throwaway self-signed pair for the cert/key requirement.
	mustSelfSignedPair(t, cert, key)
	if err := os.WriteFile(ca, []byte("# empty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := newHubClient(NodeConfig{ClientCertFile: cert, ClientKeyFile: key, CACertFile: ca})
	if err == nil {
		t.Fatal("empty CA must be rejected")
	}
	if !strings.Contains(err.Error(), "no usable certificates") {
		t.Errorf("error should explain: %v", err)
	}
}

// TestHubClientRejectsMissingFiles: a path that does not exist is a config
// error at startup, not a shipping failure later.
func TestHubClientRejectsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	_, err := newHubClient(NodeConfig{
		ClientCertFile: filepath.Join(dir, "nope.crt"),
		ClientKeyFile:  filepath.Join(dir, "nope.key"),
		CACertFile:     filepath.Join(dir, "nope-ca.crt"),
	})
	if err == nil {
		t.Fatal("missing files must be rejected")
	}
}

// TestNodeShipsOverTLS proves the mTLS client path works end to end: a node
// configured with client material posts batches to an HTTPS hub that
// requires and verifies a client certificate.
func TestNodeShipsOverTLS(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "peer.crt")
	keyPath := filepath.Join(dir, "peer.key")
	// One self-signed certificate acts as both the server's identity and the
	// trust anchor, which keeps the test focused on the client path.
	mustSelfSignedPair(t, certPath, keyPath)

	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		t.Fatal("could not build client CA pool")
	}

	var mu sync.Mutex
	var got int
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got++
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		// Rejected handshakes are the control working; keep test output clean.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	defer srv.Close()

	hubURL := "https://" + ln.Addr().String()

	n := New(NodeConfig{
		NodeID:         "node-tls",
		HubURL:         hubURL,
		RingCap:        16,
		ShipEvery:      20 * time.Millisecond,
		ClientCertFile: certPath,
		ClientKeyFile:  keyPath,
		CACertFile:     certPath,
	})
	n.ring.Put(dnsmodel.Observation{
		NodeID: "node-tls", QName: "x.", QType: wire.TypeA,
		Timestamp: time.Now().UTC(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); n.shipLoop(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := got > 0
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	delivered := got
	mu.Unlock()
	if delivered == 0 {
		t.Fatal("node did not deliver over mTLS")
	}
	if n.Metrics().Shipped.Load() == 0 {
		t.Error("Shipped counter not incremented on the mTLS path")
	}
}

// TestNodeTLSRejectsUntrustedServer: the node verifies the hub's certificate
// rather than accepting any certificate, so an untrusted hub fails shipping
// (and the batch is spooled) instead of silently sending data to a stranger.
func TestNodeTLSRejectsUntrustedServer(t *testing.T) {
	dir := t.TempDir()
	serverCert := filepath.Join(dir, "server.crt")
	serverKey := filepath.Join(dir, "server.key")
	mustSelfSignedPair(t, serverCert, serverKey)

	// A DIFFERENT certificate is what the node trusts.
	otherCert := filepath.Join(dir, "other.crt")
	otherKey := filepath.Join(dir, "other.key")
	mustSelfSignedPair(t, otherCert, otherKey)

	pair, err := tls.LoadX509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202) }),
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	defer srv.Close()

	spool := t.TempDir()
	n := New(NodeConfig{
		NodeID:         "node-tls",
		HubURL:         "https://" + ln.Addr().String(),
		RingCap:        16,
		ShipEvery:      20 * time.Millisecond,
		ClientCertFile: otherCert,
		ClientKeyFile:  otherKey,
		CACertFile:     otherCert, // trusts a different CA than the server uses
		SpoolDir:       spool,
		SpoolMax:       1 << 20,
	})
	n.ring.Put(dnsmodel.Observation{
		NodeID: "n", QName: "x.", QType: wire.TypeA, Timestamp: time.Now().UTC(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); n.shipLoop(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if n.Metrics().ShipFailures.Load() == 0 {
		t.Error("an untrusted hub certificate must fail shipping")
	}
	if n.Metrics().Shipped.Load() != 0 {
		t.Error("data must not be counted as shipped to an untrusted hub")
	}
}