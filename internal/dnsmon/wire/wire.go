package wire

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

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

// TypeName renders a record type as its canonical mnemonic. Unknown types
// render as "TYPE<n>" per RFC 3597 so the result is always usable as a
// fingerprint component and never empty.
func TypeName(t Type) string {
	switch t {
	case TypeA:
		return "A"
	case TypeNS:
		return "NS"
	case TypeCNAME:
		return "CNAME"
	case TypeMX:
		return "MX"
	case TypeTXT:
		return "TXT"
	case TypeAAAA:
		return "AAAA"
	default:
		return "TYPE" + strconv.Itoa(int(t))
	}
}

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

func (A) rdata()       {}
func (AAAA) rdata()    {}
func (CNAME) rdata()   {}
func (MX) rdata()      {}
func (TXT) rdata()     {}
func (NS) rdata()      {}
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
	ID                 uint16
	Opcode             uint8
	QR, RD, TC, AA, RA bool
	RCode              uint8
	Question           []Question
	Answers            []RR
	Authority          []RR
	Additional         []RR
}

var (
	ErrMessageTooLarge = errors.New("dns message too large")
	ErrBadPointer      = errors.New("dns compression pointer is invalid")
	ErrNameTooLong     = errors.New("dns name is too long")
	ErrMalformed       = errors.New("malformed dns message")
)

// EncodeQuery builds an uncompressed query with EDNS0. One allocation: the
// message buffer.
func EncodeQuery(q Question, ednsUDPSize uint16) []byte {
	nameLen, ok := nameWireLen(q.Name)
	if !ok {
		return nil
	}
	if ednsUDPSize == 0 {
		ednsUDPSize = 1232
	}
	if ednsUDPSize < 512 {
		ednsUDPSize = 512
	}
	if ednsUDPSize > 1232 {
		ednsUDPSize = 1232
	}
	class := q.Class
	if class == 0 {
		class = 1
	}

	// Header + QNAME/QTYPE/QCLASS + empty OPT record. Queries are never
	// compressed, so the size is known before the single message allocation.
	buf := make([]byte, 12+nameLen+4+11)
	if _, err := rand.Read(buf[:2]); err != nil {
		// Failure of the system CSPRNG is exceptionally rare, but a query must
		// not be emitted with a predictable zero transaction ID.
		return nil
	}
	binary.BigEndian.PutUint16(buf[2:4], 0x0100) // RD=1
	binary.BigEndian.PutUint16(buf[4:6], 1)      // QDCOUNT
	binary.BigEndian.PutUint16(buf[10:12], 1)    // ARCOUNT (OPT)
	off := 12
	off = appendName(buf, off, q.Name)
	binary.BigEndian.PutUint16(buf[off:off+2], uint16(q.Type))
	binary.BigEndian.PutUint16(buf[off+2:off+4], class)
	off += 4
	// OPT: root name, TYPE=41, advertised UDP size, TTL=0, RDLEN=0.
	buf[off] = 0
	binary.BigEndian.PutUint16(buf[off+1:off+3], 41)
	binary.BigEndian.PutUint16(buf[off+3:off+5], ednsUDPSize)
	return buf
}

// DecodeMessage parses a response within the transport-appropriate size
// cap. Error values are ClassSecurity-classified by the caller per the
// caps table; ErrMessageTooLarge/ErrBadPointer/ErrNameTooLong are the
// sentinel names.
func DecodeMessage(buf []byte) (*Message, error) {
	return decodeMessage(buf, 1232)
}

// DecodeMessageWithLimit parses a message within maxSize.
func DecodeMessageWithLimit(buf []byte, maxSize int) (*Message, error) {
	if maxSize <= 0 || maxSize > 65535 {
		maxSize = 65535
	}
	return decodeMessage(buf, maxSize)
}

// RCodeString renders the rcode as the closed string set used in metric
// labels ("NOERROR", "SERVFAIL", ...).
func (m *Message) RCodeString() string {
	if m == nil {
		return "UNKNOWN"
	}
	switch m.RCode {
	case 0:
		return "NOERROR"
	case 1:
		return "FORMERR"
	case 2:
		return "SERVFAIL"
	case 3:
		return "NXDOMAIN"
	case 4:
		return "NOTIMP"
	case 5:
		return "REFUSED"
	case 6:
		return "YXDOMAIN"
	case 7:
		return "YXRRSET"
	case 8:
		return "NXRRSET"
	case 9:
		return "NOTAUTH"
	case 10:
		return "NOTZONE"
	default:
		return "UNKNOWN"
	}
}

func encodeName(name string) ([]byte, bool) {
	length, ok := nameWireLen(name)
	if !ok {
		return nil, false
	}
	out := make([]byte, length)
	appendName(out, 0, name)
	return out, true
}

func nameWireLen(name string) (int, bool) {
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return 1, true
	}
	length, labels := 1, 0
	for start := 0; start < len(name); {
		end := strings.IndexByte(name[start:], '.')
		if end < 0 {
			end = len(name)
		} else {
			end += start
		}
		labelLen := end - start
		if labelLen == 0 || labelLen > 63 {
			return 0, false
		}
		length += 1 + labelLen
		labels++
		if labels > 127 || length > 255 {
			return 0, false
		}
		if end == len(name) {
			break
		}
		start = end + 1
	}
	return length, true
}

func appendName(dst []byte, off int, name string) int {
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		dst[off] = 0
		return off + 1
	}
	for start := 0; start < len(name); {
		end := strings.IndexByte(name[start:], '.')
		if end < 0 {
			end = len(name)
		} else {
			end += start
		}
		dst[off] = byte(end - start)
		off++
		copy(dst[off:], name[start:end])
		off += end - start
		if end == len(name) {
			break
		}
		start = end + 1
	}
	dst[off] = 0
	return off + 1
}

func decodeMessage(buf []byte, maxSize int) (*Message, error) {
	if len(buf) > maxSize || len(buf) > 65535 {
		return nil, ErrMessageTooLarge
	}
	if maxSize <= 1232 && len(buf) > 512 {
		// The legacy limit can be relaxed only by an OPT record. The record
		// still gets parsed below so malformed oversized packets are rejected
		// deterministically, but they are never accepted as legacy responses.
		if len(buf) > 1232 {
			return nil, ErrMessageTooLarge
		}
	}
	if len(buf) < 12 {
		return nil, fmt.Errorf("%w: header truncated", ErrMalformed)
	}
	flags := binary.BigEndian.Uint16(buf[2:4])
	m := &Message{
		ID:     binary.BigEndian.Uint16(buf[:2]),
		Opcode: uint8(flags >> 11 & 0xf),
		QR:     flags&0x8000 != 0,
		RD:     flags&0x0100 != 0,
		TC:     flags&0x0200 != 0,
		AA:     flags&0x0400 != 0,
		RA:     flags&0x0080 != 0,
		RCode:  uint8(flags & 0xf),
	}
	qd, an, ns, ar := binary.BigEndian.Uint16(buf[4:6]), binary.BigEndian.Uint16(buf[6:8]), binary.BigEndian.Uint16(buf[8:10]), binary.BigEndian.Uint16(buf[10:12])
	if uint64(qd)+uint64(an)+uint64(ns)+uint64(ar) > uint64(len(buf)) {
		return nil, fmt.Errorf("%w: record count exceeds message", ErrMalformed)
	}
	off := 12
	for i := 0; i < int(qd); i++ {
		name, next, err := decodeName(buf, off)
		if err != nil {
			return nil, err
		}
		if next+4 > len(buf) {
			return nil, fmt.Errorf("%w: question truncated", ErrMalformed)
		}
		m.Question = append(m.Question, Question{
			Name:  name,
			Type:  Type(binary.BigEndian.Uint16(buf[next : next+2])),
			Class: binary.BigEndian.Uint16(buf[next+2 : next+4]),
		})
		off = next + 4
	}

	var err error
	if m.Answers, off, err = decodeRecords(buf, off, an); err != nil {
		return nil, err
	}
	if m.Authority, off, err = decodeRecords(buf, off, ns); err != nil {
		return nil, err
	}
	if m.Additional, off, err = decodeRecords(buf, off, ar); err != nil {
		return nil, err
	}
	if off != len(buf) {
		return nil, fmt.Errorf("%w: trailing bytes", ErrMalformed)
	}
	if len(buf) > 512 && maxSize <= 1232 && !hasEDNS(m) {
		return nil, ErrMessageTooLarge
	}
	return m, nil
}

func decodeRecords(buf []byte, off int, count uint16) ([]RR, int, error) {
	if count == 0 {
		return nil, off, nil
	}
	records := make([]RR, 0, count)
	for i := 0; i < int(count); i++ {
		name, next, err := decodeName(buf, off)
		if err != nil {
			return nil, 0, err
		}
		if next+10 > len(buf) {
			return nil, 0, fmt.Errorf("%w: record header truncated", ErrMalformed)
		}
		rr := RR{
			Name:  name,
			Type:  Type(binary.BigEndian.Uint16(buf[next : next+2])),
			Class: binary.BigEndian.Uint16(buf[next+2 : next+4]),
			TTL:   binary.BigEndian.Uint32(buf[next+4 : next+8]),
		}
		rdlen := int(binary.BigEndian.Uint16(buf[next+8 : next+10]))
		dataStart := next + 10
		dataEnd := dataStart + rdlen
		if dataEnd < dataStart || dataEnd > len(buf) {
			return nil, 0, fmt.Errorf("%w: record data truncated", ErrMalformed)
		}
		rr.Data, err = decodeRData(buf, rr.Type, dataStart, dataEnd)
		if err != nil {
			return nil, 0, err
		}
		records = append(records, rr)
		off = dataEnd
	}
	return records, off, nil
}

func decodeRData(buf []byte, typ Type, start, end int) (RData, error) {
	switch typ {
	case TypeA:
		if end-start != 4 {
			return nil, fmt.Errorf("%w: A rdata length", ErrMalformed)
		}
		return A{Addr: netip.AddrFrom4([4]byte{buf[start], buf[start+1], buf[start+2], buf[start+3]})}, nil
	case TypeAAAA:
		if end-start != 16 {
			return nil, fmt.Errorf("%w: AAAA rdata length", ErrMalformed)
		}
		var a [16]byte
		copy(a[:], buf[start:end])
		return AAAA{Addr: netip.AddrFrom16(a)}, nil
	case TypeCNAME, TypeNS:
		name, next, err := decodeName(buf, start)
		if err != nil {
			return nil, err
		}
		if next != end {
			return nil, fmt.Errorf("%w: name rdata length", ErrMalformed)
		}
		if typ == TypeCNAME {
			return CNAME{Target: name}, nil
		}
		return NS{Host: name}, nil
	case TypeMX:
		if end-start < 3 {
			return nil, fmt.Errorf("%w: MX rdata truncated", ErrMalformed)
		}
		name, next, err := decodeName(buf, start+2)
		if err != nil {
			return nil, err
		}
		if next != end {
			return nil, fmt.Errorf("%w: MX name rdata length", ErrMalformed)
		}
		return MX{Pref: binary.BigEndian.Uint16(buf[start : start+2]), Host: name}, nil
	case TypeTXT:
		var stringsOut []string
		for pos := start; pos < end; {
			n := int(buf[pos])
			pos++
			if pos+n > end {
				return nil, fmt.Errorf("%w: TXT string truncated", ErrMalformed)
			}
			stringsOut = append(stringsOut, string(buf[pos:pos+n]))
			pos += n
		}
		return TXT{Strings: stringsOut}, nil
	default:
		raw := make([]byte, end-start)
		copy(raw, buf[start:end])
		return Unknown{Raw: raw}, nil
	}
}

func decodeName(buf []byte, start int) (string, int, error) {
	if start < 0 || start >= len(buf) {
		return "", 0, fmt.Errorf("%w: name offset", ErrMalformed)
	}
	labels := make([]string, 0, 4)
	pos, next, expanded, jumps := start, start, 0, 0
	jumped := false
	for {
		if pos >= len(buf) {
			return "", 0, fmt.Errorf("%w: name truncated", ErrMalformed)
		}
		length := buf[pos]
		switch length & 0xc0 {
		case 0:
			if length == 0 {
				expanded++
				if expanded > 255 {
					return "", 0, ErrNameTooLong
				}
				if !jumped {
					next = pos + 1
				}
				if len(labels) == 0 {
					return ".", next, nil
				}
				return strings.Join(labels, ".") + ".", next, nil
			}
			if length > 63 || pos+1+int(length) > len(buf) {
				return "", 0, fmt.Errorf("%w: label", ErrMalformed)
			}
			expanded += 1 + int(length)
			if expanded > 255 {
				return "", 0, ErrNameTooLong
			}
			labels = append(labels, string(buf[pos+1:pos+1+int(length)]))
			pos += 1 + int(length)
		case 0xc0:
			if pos+1 >= len(buf) {
				return "", 0, fmt.Errorf("%w: pointer truncated", ErrMalformed)
			}
			target := int(length&0x3f)<<8 | int(buf[pos+1])
			if target >= pos {
				return "", 0, ErrBadPointer
			}
			if !jumped {
				next = pos + 2
				jumped = true
			}
			pos = target
			jumps++
			if jumps > len(buf) {
				return "", 0, ErrBadPointer
			}
		default:
			return "", 0, fmt.Errorf("%w: label tag", ErrMalformed)
		}
	}
}

func hasEDNS(m *Message) bool {
	for _, rr := range m.Additional {
		if rr.Type == 41 {
			return true
		}
	}
	return false
}
