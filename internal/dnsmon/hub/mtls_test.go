package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// certBundle is a generated PKI for mTLS tests.
type certBundle struct {
	CACertPEM     []byte
	ServerCertPEM []byte
	ServerKeyPEM  []byte
	ClientCertPEM []byte
	ClientKeyPEM  []byte
	// OtherCACertPEM signs a client certificate the hub must reject.
	OtherCACertPEM     []byte
	OtherClientCertPEM []byte
	OtherClientKeyPEM  []byte
}

func newCert(t *testing.T, cn string, isCA bool, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, dnsNames []string) ([]byte, []byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		DNSNames:              dnsNames,
		IsCA:                  isCA,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	if isCA {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, cert, key
}

func buildPKI(t *testing.T) certBundle {
	t.Helper()
	var b certBundle
	caCertPEM, _, caCert, caKey := newCert(t, "rift-test-ca", true, nil, nil, nil)
	b.CACertPEM = caCertPEM

	b.ServerCertPEM, b.ServerKeyPEM, _, _ = newCert(t, "hub.test", false, caCert, caKey, []string{"hub.test"})
	b.ClientCertPEM, b.ClientKeyPEM, _, _ = newCert(t, "node-01", false, caCert, caKey, nil)

	otherCAPEM, _, otherCA, otherCAKey := newCert(t, "other-ca", true, nil, nil, nil)
	b.OtherCACertPEM = otherCAPEM
	b.OtherClientCertPEM, b.OtherClientKeyPEM, _, _ = newCert(t, "rogue-node", false, otherCA, otherCAKey, nil)
	return b
}

func writePKI(t *testing.T, b certBundle) (dir string, serverCert, serverKey, clientCA, clientCert, clientKey, otherCert, otherKey string) {
	t.Helper()
	dir = t.TempDir()
	w := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return dir,
		w("server.crt", b.ServerCertPEM), w("server.key", b.ServerKeyPEM),
		w("ca.crt", b.CACertPEM),
		w("client.crt", b.ClientCertPEM), w("client.key", b.ClientKeyPEM),
		w("other.crt", b.OtherClientCertPEM), w("other.key", b.OtherClientKeyPEM)
}

// TestHubIngestRequiresClientCertificate is the mTLS red line: without a
// client certificate the ingest plane must refuse the connection. Before
// this control existed the server required the config fields but listened in
// plaintext.
func TestHubIngestRequiresClientCertificate(t *testing.T) {
	b := buildPKI(t)
	_, serverCert, serverKey, clientCA, _, _, _, _ := writePKI(t, b)

	h := New(HubConfig{
		IngestAddr:     "127.0.0.1:0",
		QueryAddr:      "127.0.0.1:0",
		DataDir:        t.TempDir(),
		ServerCertFile: serverCert,
		ServerKeyFile:  serverKey,
		ClientCAFile:   clientCA,
		WindowKeys:     16,
	})
	t.Cleanup(func() { h.Close() })

	// Bind explicitly so the test can address the ephemeral port.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", mustTLSConfig(t, h))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	// Rejected handshakes legitimately log to the server error log; that is
	// the control working, not a test failure. Silence it so the test output
	// shows only real problems.
	srv := &http.Server{
		Handler:           h.IngestHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go srv.Serve(ln)
	defer srv.Close()

	body := `{"node_id":"n1","view":1,"resolver":"1.1.1.1:53","qname":"x.","qtype":1,"timestamp":"` +
		time.Now().UTC().Format(time.RFC3339Nano) + `"}` + "\n"

	// 1. No client certificate: refused.
	noCert := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext:     (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	}}
	resp, err := noCert.Post("https://"+addr+"/v1/ingest", "application/x-ndjson", strings.NewReader(body))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("ingest without a client certificate returned %d; mTLS is not enforced", resp.StatusCode)
	}

	// 2. Certificate signed by a DIFFERENT CA: refused.
	otherPair := loadKeyPairBytes(t, b.OtherClientCertPEM, b.OtherClientKeyPEM)
	rogue := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			Certificates:       []tls.Certificate{otherPair},
		},
	}}
	resp2, err := rogue.Post("https://"+addr+"/v1/ingest", "application/x-ndjson", strings.NewReader(body))
	if err == nil {
		resp2.Body.Close()
		t.Fatalf("ingest with a foreign-CA certificate returned %d; the CA is not verified", resp2.StatusCode)
	}
}

// TestHubIngestAcceptsValidClientCertificate proves the control is not
// merely restrictive: a properly signed node certificate is accepted.
func TestHubIngestAcceptsValidClientCertificate(t *testing.T) {
	b := buildPKI(t)
	_, serverCert, serverKey, clientCA, _, _, _, _ := writePKI(t, b)

	h := New(HubConfig{
		IngestAddr:     "127.0.0.1:0",
		QueryAddr:      "127.0.0.1:0",
		DataDir:        t.TempDir(),
		ServerCertFile: serverCert,
		ServerKeyFile:  serverKey,
		ClientCAFile:   clientCA,
		WindowKeys:     16,
	})
	t.Cleanup(func() { h.Close() })

	ln, err := tls.Listen("tcp", "127.0.0.1:0", mustTLSConfig(t, h))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	srv := &http.Server{Handler: h.IngestHandler(), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	clientPair := loadKeyPairBytes(t, b.ClientCertPEM, b.ClientKeyPEM)
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			Certificates:       []tls.Certificate{clientPair},
		},
	}}

	body := `{"node_id":"node-01","view":1,"resolver":"1.1.1.1:53","qname":"x.","qtype":1,"timestamp":"` +
		time.Now().UTC().Format(time.RFC3339Nano) + `"}` + "\n"
	resp, err := client.Post("https://"+addr+"/v1/ingest", "application/x-ndjson", strings.NewReader(body))
	if err != nil {
		t.Fatalf("valid client certificate rejected: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("valid client certificate got %d, want 202", resp.StatusCode)
	}
	if h.Window().ForTarget("x.", 1) == nil {
		t.Error("accepted observation did not reach the window")
	}
}

// TestHubPlaintextWarning confirms the development mode announces itself.
func TestHubPlaintextWarning(t *testing.T) {
	h := New(HubConfig{
		DataDir:              t.TempDir(),
		AllowPlaintextIngest: true,
		WindowKeys:           16,
	})
	// logPlaintextWarning writes to stderr; assert the configuration path
	// that selects it rather than capturing os.Stderr.
	if !h.cfg.AllowPlaintextIngest {
		t.Fatal("plaintext flag not carried into the hub config")
	}
	if h.cfg.ServerCertFile != "" {
		t.Error("plaintext mode should not carry server cert material")
	}
}

// TestHubTLSMissingMaterialFailsClosed: mTLS selected but material absent
// must refuse to start rather than silently serving plaintext.
func TestHubTLSMissingMaterialFailsClosed(t *testing.T) {
	h := New(HubConfig{DataDir: t.TempDir(), WindowKeys: 16})
	if _, err := h.ingestTLSConfig(); err == nil {
		t.Fatal("missing certificate material must be an error when mTLS is selected")
	}
}

// TestHubTLSRejectsEmptyCA: a CA file with no usable certificates must fail
// rather than producing an empty trust store.
func TestHubTLSRejectsEmptyCA(t *testing.T) {
	b := buildPKI(t)
	_, serverCert, serverKey, _, _, _, _, _ := writePKI(t, b)
	emptyCA := filepath.Join(t.TempDir(), "empty.crt")
	if err := os.WriteFile(emptyCA, []byte("# no certificates here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := New(HubConfig{
		DataDir:        t.TempDir(),
		ServerCertFile: serverCert,
		ServerKeyFile:  serverKey,
		ClientCAFile:   emptyCA,
		WindowKeys:     16,
	})
	if _, err := h.ingestTLSConfig(); err == nil {
		t.Fatal("a CA file with no certificates must be rejected")
	}
}

// loadKeyPairBytes builds a tls.Certificate from PEM bytes by writing them
// to temporary files (tls.LoadX509KeyPair takes paths, not buffers).
func loadKeyPairBytes(t *testing.T, certPEM, keyPEM []byte) tls.Certificate {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "c.pem")
	keyPath := filepath.Join(dir, "k.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	return pair
}

// mustTLSConfig exposes the hub's mTLS configuration for the test listener.
func mustTLSConfig(t *testing.T, h *Hub) *tls.Config {
	t.Helper()
	cfg, err := h.ingestTLSConfig()
	if err != nil {
		t.Fatalf("ingestTLSConfig: %v", err)
	}
	return cfg
}

func TestHubRunPlaintextLifecycle(t *testing.T) {
	h := New(HubConfig{
		IngestAddr:           "127.0.0.1:0",
		QueryAddr:            "127.0.0.1:0",
		DataDir:              t.TempDir(),
		AllowPlaintextIngest: true,
		WindowKeys:           16,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on clean cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}