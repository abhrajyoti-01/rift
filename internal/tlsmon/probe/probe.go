package tlsprobe

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/abhrajyoti-01/rift/internal/platform/errs"
	"github.com/abhrajyoti-01/rift/internal/platform/netx"
	tlsmodel "github.com/abhrajyoti-01/rift/internal/tlsmon/model"
)

// Config configures a single TLS probe.
type Config struct {
	Host   string
	Port   int
	Floor  string // "1.2" | "1.3" (minimum acceptable negotiated version)
	Verify string // "report" (default) | "strict"
	// Root is "system" or a path to a CA bundle used to validate the chain.
	Root string
	// Timeout bounds the whole probe.
	Timeout time.Duration
	// Guard authorizes outbound dials (SSRF deny-set). Nil builds a default
	// guard, so a caller cannot accidentally probe an internal address.
	Guard *netx.Guard
}

// Prober probes TLS endpoints through the guarded dialer.
type Prober struct{}

// New builds a prober.
func New() *Prober { return &Prober{} }

// Probe performs one live TLS inspection of a "host" or "host:port" target
// with default settings.
func (p *Prober) Probe(ctx context.Context, target string) (*tlsmodel.Report, error) {
	host, port := splitTarget(target)
	return p.ProbeConfig(ctx, Config{Host: host, Port: port})
}

// ProbeConfig performs a probe with explicit settings.
//
// The handshake deliberately does NOT verify the certificate
// (InsecureSkipVerify): verification is performed afterwards so chain
// failures become reportable Findings rather than an opaque abort. That
// inversion is the entire point of this component — a target with a broken
// chain is a successfully monitored target.
func (p *Prober) ProbeConfig(ctx context.Context, cfg Config) (*tlsmodel.Report, error) {
	if cfg.Host == "" {
		return nil, errs.New(errs.ClassConfig, "tlsmon.probe", "empty target host")
	}
	if cfg.Port == 0 {
		cfg.Port = 443
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Floor == "" {
		cfg.Floor = "1.2"
	}
	if cfg.Verify == "" {
		cfg.Verify = "report"
	}
	if cfg.Root == "" {
		cfg.Root = "system"
	}
	guard := cfg.Guard
	if guard == nil {
		g, gerr := netx.NewGuard(netx.GuardOptions{})
		if gerr != nil {
			return nil, gerr
		}
		guard = g
	}

	target := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	now := time.Now()
	report := &tlsmodel.Report{Target: target, Timestamp: now.UTC()}

	var presented [][]byte
	tlsCfg := &tls.Config{
		ServerName:         cfg.Host,
		InsecureSkipVerify: true, // see doc comment; manual verification follows
		MinVersion:         tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			presented = make([][]byte, len(cs.PeerCertificates))
			for i, c := range cs.PeerCertificates {
				presented[i] = c.Raw
			}
			report.NegotiatedProtocol = tlsVersionName(cs.Version)
			report.NegotiatedCipher = tls.CipherSuiteName(cs.CipherSuite)
			return nil
		},
	}

	dctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	rawConn, derr := guard.DialContext(dctx, "tcp", target)
	if derr != nil {
		report.Findings = append(report.Findings, classifyDialError(derr))
		return report, nil
	}
	defer rawConn.Close()

	if dl, ok := rawConn.(interface{ SetDeadline(time.Time) error }); ok {
		dl.SetDeadline(now.Add(cfg.Timeout))
	}

	tlsConn := tls.Client(rawConn, tlsCfg)
	if herr := tlsConn.HandshakeContext(dctx); herr != nil {
		report.Findings = append(report.Findings, tlsmodel.Finding{
			Severity: tlsmodel.SevCritical,
			Code:     tlsmodel.CodeHandshakeRefused,
			Detail:   herr.Error(),
		})
		return report, nil
	}

	for _, raw := range presented {
		if ci, ok := parseCert(raw); ok {
			report.Chain = append(report.Chain, ci)
		}
	}
	if len(report.Chain) == 0 {
		report.Findings = append(report.Findings, tlsmodel.Finding{
			Severity: tlsmodel.SevCritical,
			Code:     tlsmodel.CodeNoPeerCert,
			Detail:   "server presented no certificate",
		})
		return report, nil
	}

	classify(report, cfg, presented, now)

	if cfg.Verify == "strict" && len(report.Findings) > 0 {
		return report, errs.New(errs.ClassSecurity, "tlsmon.probe",
			fmt.Sprintf("%s failed strict verification: %s", target, strings.Join(report.Codes(), ",")))
	}
	return report, nil
}

func classify(report *tlsmodel.Report, cfg Config, presented [][]byte, now time.Time) {
	leafInfo := report.Chain[0]

	if now.After(leafInfo.NotAfter) {
		report.Findings = append(report.Findings, tlsmodel.Finding{
			Severity: tlsmodel.SevCritical,
			Code:     tlsmodel.CodeExpired,
			Detail:   "expired " + leafInfo.NotAfter.UTC().Format(time.RFC3339),
		})
	}
	if now.Before(leafInfo.NotBefore) {
		report.Findings = append(report.Findings, tlsmodel.Finding{
			Severity: tlsmodel.SevCritical,
			Code:     tlsmodel.CodeNotYetValid,
			Detail:   "not valid until " + leafInfo.NotBefore.UTC().Format(time.RFC3339),
		})
	}

	var intermediates []*x509.Certificate
	for _, raw := range presented[1:] {
		if c, err := x509.ParseCertificate(raw); err == nil {
			intermediates = append(intermediates, c)
		}
	}
	leaf, err := x509.ParseCertificate(presented[0])
	if err == nil {
		if roots, rerr := rootPool(cfg.Root); rerr != nil {
			report.Findings = append(report.Findings, tlsmodel.Finding{
				Severity: tlsmodel.SevWarning,
				Code:     tlsmodel.CodeUntrustedRoot,
				Detail:   "root store unavailable: " + rerr.Error(),
			})
		} else {
			inter := x509.NewCertPool()
			for _, c := range intermediates {
				inter.AddCert(c)
			}
			opts := x509.VerifyOptions{
				Roots:         roots,
				Intermediates: inter,
				CurrentTime:   now,
				DNSName:       cfg.Host,
			}
			if _, verr := leaf.Verify(opts); verr != nil {
				report.Findings = append(report.Findings, classifyVerifyError(verr, cfg.Host))
			}
		}

		// Hostname validation is evaluated INDEPENDENTLY of chain trust:
		// x509.Verify returns UnknownAuthorityError before it ever gets to
		// the hostname check, so an untrusted-root finding must not mask a
		// wrong-host finding. A certificate for the wrong host is a
		// critical security finding on its own.
		if !nameMatches(leaf, cfg.Host) {
			report.Findings = append(report.Findings, tlsmodel.Finding{
				Severity: tlsmodel.SevCritical,
				Code:     tlsmodel.CodeHostnameMismatch,
				Detail:   "certificate does not cover " + cfg.Host,
			})
		}

		if len(presented) == 1 && leaf.CheckSignatureFrom(leaf) == nil {
			report.Findings = append(report.Findings, tlsmodel.Finding{
				Severity: tlsmodel.SevWarning,
				Code:     tlsmodel.CodeSelfSigned,
				Detail:   "leaf is self-signed",
			})
		}
		if f, ok := weakKeyFinding(leaf); ok {
			report.Findings = append(report.Findings, f)
		}
		if f, ok := weakSigFinding(leaf); ok {
			report.Findings = append(report.Findings, f)
		}
	}

	if len(presented) == 1 && !isSelfSigned(report.Chain[0]) {
		report.Findings = append(report.Findings, tlsmodel.Finding{
			Severity: tlsmodel.SevWarning,
			Code:     tlsmodel.CodeIncompleteChain,
			Detail:   "no intermediates presented",
		})
	}

	if belowFloor(report.NegotiatedProtocol, cfg.Floor) {
		report.Findings = append(report.Findings, tlsmodel.Finding{
			Severity: tlsmodel.SevWarning,
			Code:     tlsmodel.CodeOldTLS,
			Detail:   report.NegotiatedProtocol + " below floor " + cfg.Floor,
		})
	}
}

func isSelfSigned(ci tlsmodel.CertInfo) bool { return ci.Subject == ci.Issuer }

// nameMatches reports whether the leaf is valid for host, checking IP
// literals against IP SANs and DNS names against DNS SANs (with wildcard
// support via x509's own verifier).
func nameMatches(leaf *x509.Certificate, host string) bool {
	if leaf.VerifyHostname(host) == nil {
		return true
	}
	// An IP-literal probe target must be present in the IP SANs; a DNS name
	// never satisfies it.
	if ip := net.ParseIP(host); ip != nil {
		return leaf.VerifyHostname(ip.String()) == nil
	}
	return false
}

func classifyVerifyError(err error, host string) tlsmodel.Finding {
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return tlsmodel.Finding{Severity: tlsmodel.SevCritical, Code: tlsmodel.CodeHostnameMismatch,
			Detail: "certificate does not cover " + host}
	}
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &unknownAuth) {
		return tlsmodel.Finding{Severity: tlsmodel.SevWarning, Code: tlsmodel.CodeUntrustedRoot,
			Detail: "chain does not terminate at a trusted root"}
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		if invalid.Reason == x509.Expired {
			return tlsmodel.Finding{Severity: tlsmodel.SevCritical, Code: tlsmodel.CodeExpired, Detail: invalid.Error()}
		}
		return tlsmodel.Finding{Severity: tlsmodel.SevWarning, Code: tlsmodel.CodeIncompleteChain, Detail: invalid.Error()}
	}
	return tlsmodel.Finding{Severity: tlsmodel.SevWarning, Code: tlsmodel.CodeIncompleteChain, Detail: err.Error()}
}

func weakKeyFinding(c *x509.Certificate) (tlsmodel.Finding, bool) {
	switch pub := c.PublicKey.(type) {
	case *rsa.PublicKey:
		if pub.N.BitLen() < 2048 {
			return tlsmodel.Finding{Severity: tlsmodel.SevWarning, Code: tlsmodel.CodeWeakKey,
				Detail: fmt.Sprintf("RSA %d bits", pub.N.BitLen())}, true
		}
	case *ecdsa.PublicKey:
		if pub.Curve.Params().BitSize < 256 {
			return tlsmodel.Finding{Severity: tlsmodel.SevWarning, Code: tlsmodel.CodeWeakKey,
				Detail: fmt.Sprintf("ECDSA %d bits", pub.Curve.Params().BitSize)}, true
		}
	}
	return tlsmodel.Finding{}, false
}

func weakSigFinding(c *x509.Certificate) (tlsmodel.Finding, bool) {
	switch c.SignatureAlgorithm {
	case x509.MD5WithRSA, x509.SHA1WithRSA, x509.ECDSAWithSHA1, x509.DSAWithSHA1:
		return tlsmodel.Finding{Severity: tlsmodel.SevWarning, Code: tlsmodel.CodeWeakSig,
			Detail: c.SignatureAlgorithm.String()}, true
	}
	return tlsmodel.Finding{}, false
}

func belowFloor(version, floor string) bool {
	rank := map[string]int{"": 0, "TLS 1.0": 10, "TLS 1.1": 11, "TLS 1.2": 12, "TLS 1.3": 13}
	v := rank[version]
	f := rank["TLS "+floor]
	return v > 0 && f > 0 && v < f
}

func classifyDialError(err error) tlsmodel.Finding {
	msg := err.Error()
	if errs.ClassOf(err) == errs.ClassSecurity {
		return tlsmodel.Finding{Severity: tlsmodel.SevWarning, Code: tlsmodel.CodeConnRefused, Detail: msg}
	}
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline") {
		return tlsmodel.Finding{Severity: tlsmodel.SevCritical, Code: tlsmodel.CodeConnTimeout, Detail: msg}
	}
	return tlsmodel.Finding{Severity: tlsmodel.SevCritical, Code: tlsmodel.CodeConnRefused, Detail: msg}
}

func parseCert(raw []byte) (tlsmodel.CertInfo, bool) {
	c, err := x509.ParseCertificate(raw)
	if err != nil {
		return tlsmodel.CertInfo{}, false
	}
	return tlsmodel.CertInfo{
		Subject:   c.Subject.String(),
		Issuer:    c.Issuer.String(),
		NotBefore: c.NotBefore,
		NotAfter:  c.NotAfter,
		SANs:      c.DNSNames,
		Serial:    c.SerialNumber.String(),
		SigAlg:    c.SignatureAlgorithm.String(),
		IsCA:      c.IsCA,
	}, true
}

func rootPool(rootSpec string) (*x509.CertPool, error) {
	if rootSpec == "system" || rootSpec == "" {
		return x509.SystemCertPool()
	}
	pem, err := os.ReadFile(rootSpec)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("no certificates parsed from " + rootSpec)
	}
	return pool, nil
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("unknown(0x%04x)", v)
	}
}

// splitTarget splits "host:port" or bare "host" (default port 443).
func splitTarget(target string) (string, int) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return target, 443
	}
	port, perr := strconv.Atoi(portStr)
	if perr != nil {
		return host, 443
	}
	return host, port
}
