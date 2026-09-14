// Package wire is RIFT's own RFC 1035 DNS message engine (TECHNICAL_SPEC
// §5.1). Own implementation is a locked decision (AD-4): stdlib exposes no
// TTL (net.NS is {Host string}, net.MX is {Host, Pref}, LookupCNAME returns
// bare strings — verified against the pinned toolchain), no RD=0, no TC
// visibility, no raw message access for cross-resolver diffing.
//
// Hardening rules that shape this contract: decode is iterative with a
// strictly-earlier pointer rule (loops structurally impossible); size caps
// by transport (512 legacy / 1232 EDNS0 / 65535 TCP); queries are sent
// uncompressed (compression is parsed, never produced).
//
// Phase 3 (ROADMAP.md). This file pins the public contract; the codec
// lands with its fuzz corpus.
package wire

import "net/netip"

// Type is an RR TYPE code.
type Type uint16

const (
	TypeA     Type = 1
	TypeNS    Type = 2
	TypeCNAME Type = 5
	TypeMX    Type = 15
	TypeTXT   Type = 16
	TypeAAAA  Type = 28
)

// Question is one QDCOUNT entry. Class is IN (1) in all v1 usage.
type Question struct {
	Name  string
	Type  Type
	Class uint16
}

// RData is the parsed RR payload. Unknown types are forwarded as raw bytes,
// never guessed.
type RData interface{ rdata() }

// A carries an IPv4 address.
type A struct{ Addr netip.Addr }

// AAAA carries an IPv6 address.
type AAAA struct{ Addr netip.Addr }

// CNAME carries the canonical name.
type CNAME struct{ Target string }

// MX carries preference and exchange.
type MX struct {
	Pref uint16
	Host string
}

// TXT carries character-strings.
type TXT struct{ Strings []string }

// NS carries the nameserver name.
type NS struct{ Host string }

// Unknown forwards an unrecognized type verbatim.
type Unknown struct{ Raw []byte }

func (A) rdata()     {}
func (AAAA) rdata()  {}
func (CNAME) rdata() {}
func (MX) rdata()    {}
func (TXT) rdata()   {}
func (NS) rdata()    {}
func (Unknown) rdata() {}

// RR is one resource record with the TTL stdlib never exposes.
type RR struct {
	Name  string
	Type  Type
	Class uint16
	TTL   uint32
	Data  RData
}

// Message is a decoded DNS message. Bool flags map 1:1 to header bits.
type Message struct {
	ID     uint16
	Opcode uint8
	RD, TC, AA, RA bool
	RCode  uint8
	Question   []Question
	Answers    []RR
	Authority  []RR
	Additional []RR
}

// EncodeQuery builds an uncompressed query with EDNS0. One allocation: the
// message buffer.
func EncodeQuery(q Question, ednsUDPSize uint16) []byte {
	// Phase 3 (ROADMAP.md).
	_, _ = q, ednsUDPSize
	return nil
}

// DecodeMessage parses a response within the transport-appropriate size
// cap. Error values are ClassSecurity-classified by the caller per the
// caps table; ErrMessageTooLarge/ErrBadPointer/ErrNameTooLong are the
// sentinel names.
func DecodeMessage(buf []byte) (*Message, error) {
	// Phase 3 (ROADMAP.md).
	_ = buf
	return nil, nil
}

// RCodeString renders the rcode as the closed string set used in metric
// labels ("NOERROR", "SERVFAIL", ...).
func (m *Message) RCodeString() string {
	// Phase 3 (ROADMAP.md).
	_ = m
	return ""
}
