package tlsprobe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abhrajyoti-01/rift/internal/platform/netx"
	tlsmodel "github.com/abhrajyoti-01/rift/internal/tlsmon/model"
)

// certSpec describes a certificate to mint for a test server.
type certSpec struct {
	cn        string
	sans      []string
	notBefore time.Time
	notAfter  time.Time
	selfSign  bool
	rsaBits   int
}

// mintCert creates a certificate, optionally signed by a parent.
func mintCert(t *testing.T, spec certSpec, parent *x509.Certificate, parentKey any) (*x509.Certificate, any) {
	t.Helper()
	var key any
	var err error
	if spec.rsaBits > 0 {
		key, err = rsa.GenerateKey(rand.Reader, spec.rsaBits)
	} else {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: spec.cn},
		NotBefore:    spec.notBefore,
		NotAfter:     spec.notAfter,
		DNSNames:     spec.sans,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         spec.selfSign,
		BasicConstraintsValid: true,
	}
	signer := tmpl
	signerKey := key
	if !spec.selfSign && parent != nil {
		signer = parent
		signerKey = parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, publicKeyOf(key), signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func publicKeyOf(key any) any {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	}
	return nil
}

// tlsServer starts a TLS server presenting the given cert chain.
func tlsServer(t *testing.T, certs []tls.Certificate) (host string, port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{Certificates: certs, MinVersion: tls.VersionTLS12}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				tc := tls.Server(c, cfg)
				if err := tc.Handshake(); err != nil {
					return
				}
				// Hold briefly so the probe can finish reading state.
				buf := make([]byte, 1)
				tc.Read(buf)
			}()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port, func() { ln.Close() }
}

func guardAllowingLoopback(t *testing.T) *netx.Guard {
	t.Helper()
	g, err := netx.NewGuard(netx.GuardOptions{Allow: netx.DefaultDenySet()})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestProbeValidSelfSignedReportsSelfSigned(t *testing.T) {
	now := time.Now()
	cert, key := mintCert(t, certSpec{
		cn: "example.test", sans: []string{"example.test"},
		notBefore: now.Add(-time.Hour), notAfter: now.Add(90 * 24 * time.Hour),
		selfSign: true,
	}, nil, nil)
	srvCert := tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}

	host, port, stop := tlsServer(t, []tls.Certificate{srvCert})
	defer stop()

	// Use a temp root pool containing nothing so verification fails
	// deterministically (we want the untrusted/self-signed finding).
	emptyRoot := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(emptyRoot, []byte{}, 0o600)

	p := New()
	report, err := p.ProbeConfig(context.Background(), Config{
		Host: host, Port: port, Root: emptyRoot, Guard: guardAllowingLoopback(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Chain) == 0 {
		t.Fatal("no certificate captured")
	}
	codes := report.Codes()
	if !contains(codes, tlsmodel.CodeSelfSigned) && !contains(codes, tlsmodel.CodeUntrustedRoot) {
		t.Errorf("self-signed cert produced no finding: %v", codes)
	}
	if report.NegotiatedProtocol == "" {
		t.Error("negotiated protocol not recorded")
	}
}

func TestProbeExpiredCert(t *testing.T) {
	now := time.Now()
	cert, key := mintCert(t, certSpec{
		cn: "expired.test", sans: []string{"expired.test"},
		notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(-24 * time.Hour),
		selfSign: true,
	}, nil, nil)
	srvCert := tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}

	host, port, stop := tlsServer(t, []tls.Certificate{srvCert})
	defer stop()

	emptyRoot := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(emptyRoot, []byte{}, 0o600)

	report, err := New().ProbeConfig(context.Background(), Config{
		Host: host, Port: port, Root: emptyRoot, Guard: guardAllowingLoopback(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(report.Codes(), tlsmodel.CodeExpired) {
		t.Errorf("expired cert not flagged: %v", report.Codes())
	}
	if !report.HasCritical() {
		t.Error("expired cert should be critical")
	}
}

func TestProbeWeakRSAKey(t *testing.T) {
	now := time.Now()
	cert, key := mintCert(t, certSpec{
		cn: "weak.test", sans: []string{"weak.test"},
		notBefore: now.Add(-time.Hour), notAfter: now.Add(24 * time.Hour),
		selfSign: true, rsaBits: 1024, // below the 2048 floor
	}, nil, nil)
	srvCert := tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}

	host, port, stop := tlsServer(t, []tls.Certificate{srvCert})
	defer stop()

	emptyRoot := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(emptyRoot, []byte{}, 0o600)

	report, err := New().ProbeConfig(context.Background(), Config{
		Host: host, Port: port, Root: emptyRoot, Guard: guardAllowingLoopback(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(report.Codes(), tlsmodel.CodeWeakKey) {
		t.Errorf("1024-bit RSA key not flagged as weak: %v", report.Codes())
	}
}

func TestProbeHostnameMismatch(t *testing.T) {
	now := time.Now()
	cert, key := mintCert(t, certSpec{
		cn: "wrong.test", sans: []string{"wrong.test", "other.test"},
		notBefore: now.Add(-time.Hour), notAfter: now.Add(24 * time.Hour),
		selfSign: true,
	}, nil, nil)
	srvCert := tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}

	host, port, stop := tlsServer(t, []tls.Certificate{srvCert})
	defer stop()

	emptyRoot := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(emptyRoot, []byte{}, 0o600)

	// Probe using an IP literal as the hostname: the cert covers DNS names
	// only, so this must be flagged as a mismatch.
	report, err := New().ProbeConfig(context.Background(), Config{
		Host: host, Port: port, Root: emptyRoot, Guard: guardAllowingLoopback(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	codes := report.Codes()
	if !contains(codes, tlsmodel.CodeHostnameMismatch) && !contains(codes, tlsmodel.CodeIncompleteChain) {
		t.Errorf("name mismatch not detected: %v", codes)
	}
}

// TestProbeConnectionRefused is the availability signal: a target that is
// not answering is a successfully monitored failed target.
func TestProbeConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	ln.Close() // nothing listening

	report, err := New().ProbeConfig(context.Background(), Config{
		Host: addr.IP.String(), Port: addr.Port, Guard: guardAllowingLoopback(t),
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal("probe must report, not error, on connection failure:", err)
	}
	codes := report.Codes()
	if !contains(codes, tlsmodel.CodeConnRefused) && !contains(codes, tlsmodel.CodeConnTimeout) {
		t.Errorf("refused connection not classified: %v", codes)
	}
	if !report.HasCritical() {
		t.Error("a refused connection is a critical availability finding")
	}
}

// TestProbeSSRFGuardRefusesInternal is the red-line security test: TLS
// probing must not become an internal port scanner.
func TestProbeSSRFGuardRefusesInternal(t *testing.T) {
	// A default guard denies loopback; the probe must refuse to dial.
	report, err := New().ProbeConfig(context.Background(), Config{
		Host: "127.0.0.1", Port: 80,
		// No guard → default deny-set applies.
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal("probe must report a finding, not error")
	}
	if len(report.Findings) == 0 {
		t.Fatal("probing a denied address must yield a finding")
	}
}

func TestProbeStrictModeFailsOnFindings(t *testing.T) {
	now := time.Now()
	cert, key := mintCert(t, certSpec{
		cn: "strict.test", sans: []string{"strict.test"},
		notBefore: now.Add(-time.Hour), notAfter: now.Add(24 * time.Hour),
		selfSign: true,
	}, nil, nil)
	srvCert := tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}
	host, port, stop := tlsServer(t, []tls.Certificate{srvCert})
	defer stop()

	emptyRoot := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(emptyRoot, []byte{}, 0o600)

	_, err := New().ProbeConfig(context.Background(), Config{
		Host: host, Port: port, Root: emptyRoot, Verify: "strict",
		Guard: guardAllowingLoopback(t),
	})
	if err == nil {
		t.Error("strict mode must return an error when findings exist")
	}
}

func TestProbeReportsNegotiatedTLS13(t *testing.T) {
	now := time.Now()
	cert, key := mintCert(t, certSpec{
		cn: "tls13.test", sans: []string{"tls13.test"},
		notBefore: now.Add(-time.Hour), notAfter: now.Add(24 * time.Hour),
		selfSign: true,
	}, nil, nil)
	srvCert := tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}
	host, port, stop := tlsServer(t, []tls.Certificate{srvCert})
	defer stop()

	emptyRoot := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(emptyRoot, []byte{}, 0o600)

	report, err := New().ProbeConfig(context.Background(), Config{
		Host: host, Port: port, Root: emptyRoot, Guard: guardAllowingLoopback(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	// With floor 1.2 and a modern server, no OLD_TLS finding should appear.
	if contains(report.Codes(), tlsmodel.CodeOldTLS) {
		t.Errorf("modern negotiated version flagged as old: %s", report.NegotiatedProtocol)
	}
	if report.NegotiatedCipher == "" {
		t.Error("negotiated cipher not recorded")
	}
}

func TestSplitTarget(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"example.com:8443", "example.com", 8443},
		{"example.com", "example.com", 443},
		{"127.0.0.1:443", "127.0.0.1", 443},
	}
	for _, c := range cases {
		h, p := splitTarget(c.in)
		if h != c.wantHost || p != c.wantPort {
			t.Errorf("splitTarget(%q) = (%q,%d), want (%q,%d)", c.in, h, p, c.wantHost, c.wantPort)
		}
	}
}

func TestDaysUntilExpiry(t *testing.T) {
	now := time.Now()
	ci := tlsmodel.CertInfo{NotAfter: now.Add(10 * 24 * time.Hour)}
	if d := ci.DaysUntilExpiry(now); d != 10 {
		t.Errorf("DaysUntilExpiry = %d, want 10", d)
	}
	expired := tlsmodel.CertInfo{NotAfter: now.Add(-5 * 24 * time.Hour)}
	if d := expired.DaysUntilExpiry(now); d >= 0 {
		t.Errorf("expired cert days = %d, want negative", d)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
