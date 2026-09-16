package tlsmodel

import "time"

// Severity orders findings for reporting and alerting.
type Severity uint8

const (
	SevInfo Severity = iota
	SevWarning
	SevCritical
)

// String renders the closed severity label set.
func (s Severity) String() string {
	switch s {
	case SevInfo:
		return "info"
	case SevWarning:
		return "warning"
	case SevCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Finding codes. This is the CLOSED set: a free-form code would become an
// unbounded metric label, and an unbounded label is a memory-exhaustion
// primitive.
const (
	CodeExpired          = "EXPIRED"
	CodeNotYetValid      = "NOT_YET_VALID"
	CodeHostnameMismatch = "HOSTNAME_MISMATCH"
	CodeUntrustedRoot    = "UNTRUSTED_ROOT"
	CodeIncompleteChain  = "INCOMPLETE_CHAIN"
	CodeSelfSigned       = "SELF_SIGNED"
	CodeWeakKey          = "WEAK_KEY"
	CodeWeakSig          = "WEAK_SIG"
	CodeOldTLS           = "OLD_TLS"
	CodeHandshakeRefused = "HANDSHAKE_REFUSED"
	CodeConnRefused      = "CONN_REFUSED"
	CodeConnTimeout      = "CONN_TIMEOUT"
	CodeProtoMismatch    = "PROTO_MISMATCH"
	CodeNoPeerCert       = "NO_PEER_CERT"
)

// Finding is one detected problem.
type Finding struct {
	Severity Severity
	Code     string
	Detail   string
}

// CertInfo captures one presented certificate.
type CertInfo struct {
	Subject             string
	Issuer              string
	NotBefore, NotAfter time.Time
	SANs                []string
	Serial              string
	SigAlg              string
	IsCA                bool
}

// DaysUntilExpiry returns whole days until NotAfter, negative if expired.
func (c CertInfo) DaysUntilExpiry(now time.Time) int {
	return int(c.NotAfter.Sub(now).Hours() / 24)
}

// Report contains the certificates, negotiated connection details, and
// findings collected during a probe.
type Report struct {
	Target             string
	Timestamp          time.Time
	Chain              []CertInfo
	NegotiatedProtocol string
	NegotiatedCipher   string
	Findings           []Finding
}

// HasCritical reports whether the report contains any critical finding.
func (r *Report) HasCritical() bool {
	for _, f := range r.Findings {
		if f.Severity == SevCritical {
			return true
		}
	}
	return false
}

// Codes returns the finding codes present, in report order.
func (r *Report) Codes() []string {
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.Code)
	}
	return out
}

// Leaf returns the leaf certificate, or the zero value when absent.
func (r *Report) Leaf() (CertInfo, bool) {
	if len(r.Chain) == 0 {
		return CertInfo{}, false
	}
	return r.Chain[0], true
}
