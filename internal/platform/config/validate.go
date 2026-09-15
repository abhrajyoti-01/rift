package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// checkHostPort validates a "host:port" bind/addr string. The host may be
// empty (meaning all interfaces), an IP literal, or a hostname.
func checkHostPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("must be host:port (got " + strconv.Quote(addr) + ")")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return errors.New("port must be within [0, 65535] (got " + strconv.Quote(port) + ")")
	}
	if host == "" {
		return nil
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	return checkHostname(host)
}

// isLoopbackHostPort reports whether addr binds a loopback address (or
// the unspecified address, which is NOT loopback and therefore not safe
// for the admin plane).
func isLoopbackHostPort(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		// Hostname binds are treated as non-loopback (conservative: the
		// admin plane should be bound by IP, and "localhost" resolves
		// unpredictably across systems).
		return false
	}
	return ip.IsLoopback()
}

// checkLiteralIPPort requires "ip:port" with a literal IP host. A hostname
// is rejected: the DNS monitor must not depend on DNS to bootstrap itself.
func checkLiteralIPPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("must be ip:port (got " + strconv.Quote(addr) + ")")
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return errors.New("host must be a literal IP, not a hostname (got " + strconv.Quote(host) + ")")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return errors.New("port must be within [1, 65535] (got " + strconv.Quote(port) + ")")
	}
	return nil
}

// parseCIDROrIP accepts either an IP or a CIDR prefix.
func parseCIDROrIP(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Prefix{}, errors.New("must not be empty")
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		bits := 32
		if a.Is6() {
			bits = 128
		}
		return netip.PrefixFrom(a, bits), nil
	}
	return netip.Prefix{}, errors.New("must be an IP or CIDR prefix (got " + strconv.Quote(s) + ")")
}

// checkDNSName validates a DNS name: total length ≤ 253, labels 1..63
// octets, no empty labels, optional trailing dot.
func checkDNSName(name string) error {
	if name == "" {
		return errors.New("must not be empty")
	}
	const maxName = 253
	trimmed := strings.TrimSuffix(name, ".")
	if trimmed == "" {
		// Root "." is legal.
		return nil
	}
	if len(trimmed) > maxName {
		return fmt.Errorf("exceeds %d octets", maxName)
	}
	for _, label := range strings.Split(trimmed, ".") {
		if label == "" {
			return errors.New("contains an empty label")
		}
		if len(label) > 63 {
			return errors.New("label exceeds 63 octets")
		}
	}
	return nil
}

// checkHostname validates a hostname suitable for a TLS probe target.
func checkHostname(host string) error {
	if net.ParseIP(host) != nil {
		return nil // IP literals are acceptable probe targets
	}
	if strings.ContainsAny(host, " \t\r\n/\\@") {
		return errors.New("contains invalid characters")
	}
	if len(host) > 253 {
		return errors.New("exceeds 253 octets")
	}
	return nil
}

// ParseByteSize parses "1MiB", "64KiB", "1024", "4M" into bytes. Exported so
// the CLI and media server share one parser rather than two that drift.
func ParseByteSize(s string) (int64, error) { return parseByteSize(s) }

// parseByteSize parses a byte size with an optional unit suffix.
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("must not be empty")
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') {
		i++
	}
	if i == 0 {
		return 0, errors.New("must start with a number (got " + strconv.Quote(s) + ")")
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, errors.New("invalid number in " + strconv.Quote(s))
	}
	unit := strings.ToLower(strings.TrimSpace(s[i:]))
	mult := int64(1)
	switch unit {
	case "", "b":
		mult = 1
	case "k", "kb", "kib":
		mult = 1 << 10
	case "m", "mb", "mib":
		mult = 1 << 20
	case "g", "gb", "gib":
		mult = 1 << 30
	default:
		return 0, errors.New("unknown unit " + strconv.Quote(unit) + " (want B, KiB, MiB, GiB)")
	}
	return n * mult, nil
}