// Package model holds the TLS monitoring data contracts (TECHNICAL_SPEC
// §6). tlsmon must not share TLS config code with lb (ARCHITECTURE §7):
// lb terminates TLS and must verify; tlsmon must NOT verify so it can
// report chain failures — that inversion is the point of the component.
package tlsmodel

import "time"

// Severity orders findings for reporting and alerting.
type Severity uint8

const (
	SevInfo Severity = iota
	SevWarning
	SevCritical
)

// Finding is one detected problem. Code is from the closed set (EXPIRED,
// NOT_YET_VALID, HOSTNAME_MISMATCH, UNTRUSTED_ROOT, INCOMPLETE_CHAIN,
// SELF_SIGNED, WEAK_KEY, WEAK_SIG, OLD_TLS, HANDSHAKE_REFUSED,
// CONN_REFUSED, CONN_TIMEOUT, PROTO_MISMATCH) — never free-form.
type Finding struct {
	Severity Severity
	Code     string
	Detail   string
}

// CertInfo captures one presented certificate.
type CertInfo struct {
	Subject, Issuer string
	NotBefore, NotAfter time.Time
	SANs   []string
	Serial, SigAlg string
	IsCA   bool
}

// Report is the probe product: every failure is a Finding, never an abort
// — a target with a bad chain is a successfully monitored target (AD-5).
type Report struct {
	Target    string
	Timestamp time.Time
	Chain     []CertInfo // leaf first, as presented
	NegotiatedProtocol string
	NegotiatedCipher    string
	Findings  []Finding
}
