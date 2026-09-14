package netx

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/rift/rift/internal/platform/errs"
)

func newTestGuard(t *testing.T, allow []netip.Prefix) *Guard {
	t.Helper()
	g, err := NewGuard(GuardOptions{Allow: allow})
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	return g
}

func TestGuardDeniesDefaultSet(t *testing.T) {
	g := newTestGuard(t, nil)
	denied := []string{
		"127.0.0.1:80", "[::1]:80",
		"10.0.0.1:443", "172.16.0.5:53", "192.168.1.1:8080",
		"169.254.169.254:80", "[fe80::1]:80", "[fc00::1]:53",
		"224.0.0.1:80", "[ff02::1]:80",
		"0.0.0.0:80", "[::]:80",
		"100.64.0.1:80", "240.0.0.1:80",
	}
	for _, addr := range denied {
		_, err := g.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			t.Errorf("dial %s: expected SSRF denial, got conn", addr)
			continue
		}
		if errs.ClassOf(err) != errs.ClassSecurity {
			t.Errorf("dial %s: class = %v, want ClassSecurity (%v)", addr, errs.ClassOf(err), err)
		}
	}
	if n := g.DeniedTotal(); n != uint64(len(denied)) {
		t.Errorf("DeniedTotal = %d, want %d", n, len(denied))
	}
}

// TestGuardPermitsPublicPolicy asserts the *policy* (permitted()), not
// connectivity: unit tests must not dial the internet (network-restricted
// CI, 20s+ Windows dial timeouts). End-to-end dialing is covered hermetically
// by TestGuardAllowOverridesDeny via a local listener.
func TestGuardPermitsPublicPolicy(t *testing.T) {
	g := newTestGuard(t, nil)
	pub, err := netip.ParseAddr("93.184.216.34")
	if err != nil {
		t.Fatal(err)
	}
	if ok, rule := g.permitted(pub); !ok {
		t.Errorf("public IP must be permitted, denied by rule %q", rule)
	}
	if g.DeniedTotal() != 0 {
		t.Errorf("DeniedTotal = %d, want 0 (policy check must not count as denial)", g.DeniedTotal())
	}
}

func TestGuardEveryAnswerChecked(t *testing.T) {
	g := newTestGuard(t, nil)
	// Multi-answer resolution where ANY answer is internal → whole dial
	// refused, even though a public address is also available.
	g.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		pub, _ := netip.ParseAddr("93.184.216.34")
		priv, _ := netip.ParseAddr("10.0.0.7")
		return []netip.Addr{pub, priv}, nil
	}
	_, err := g.DialContext(context.Background(), "tcp", "evil.example.com:443")
	if err == nil || errs.ClassOf(err) != errs.ClassSecurity {
		t.Fatalf("split-horizon host must be ClassSecurity-refused, got %v", err)
	}
	if g.DeniedTotal() != 1 {
		t.Errorf("DeniedTotal = %d, want 1", g.DeniedTotal())
	}
}

func TestGuardAllowOverridesDeny(t *testing.T) {
	// Hermetic end-to-end: allow loopback through the guard, then dial a
	// real local listener. Proves allow-overrides-deny AND the full
	// resolve→guard→literal-dial path without touching the network.
	allow, err := netip.ParsePrefix("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	g := newTestGuard(t, []netip.Prefix{allow})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("local listener: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	addr := ln.Addr().String()
	conn, derr := g.DialContext(context.Background(), "tcp", addr)
	if derr != nil {
		t.Fatalf("allow-listed local dial failed: %v", derr)
	}
	conn.Close()
	if g.DeniedTotal() != 0 {
		t.Errorf("DeniedTotal = %d, want 0", g.DeniedTotal())
	}
}

func TestGuardNoReResolution(t *testing.T) {
	// Rebinding-window check on the success path: resolve must run exactly
	// once per dial. Hermetic via allow-listed loopback + local listener.
	allow, err := netip.ParsePrefix("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	g := newTestGuard(t, []netip.Prefix{allow})
	calls := 0
	lo, _ := netip.ParseAddr("127.0.0.1")
	g.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		calls++
		return []netip.Addr{lo}, nil
	}

	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatalf("local listener: %v", lerr)
	}
	defer ln.Close()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			c.Close()
		}
	}()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	conn, derr := g.DialContext(context.Background(), "tcp", net.JoinHostPort("rebind.example.com", port))
	if derr != nil {
		t.Fatalf("dial through fake resolve failed: %v", derr)
	}
	conn.Close()
	if calls != 1 {
		t.Errorf("resolve called %d times for one dial — rebinding window open", calls)
	}
}

func TestGuardInvalidConfigFailsClosed(t *testing.T) {
	bad, _ := netip.ParsePrefix("10.0.0.1/33")
	if _, err := NewGuard(GuardOptions{Allow: []netip.Prefix{bad}}); err == nil {
		t.Error("invalid allow prefix must be a config error")
	} else if errs.ClassOf(err) != errs.ClassConfig {
		t.Errorf("invalid allow class = %v, want ClassConfig", errs.ClassOf(err))
	}
}

func TestGuardMalformedAddress(t *testing.T) {
	g := newTestGuard(t, nil)
	_, err := g.DialContext(context.Background(), "tcp", "no-port-here")
	if err == nil || errs.ClassOf(err) != errs.ClassNetwork {
		t.Errorf("malformed address class = %v, want ClassNetwork", errs.ClassOf(err))
	}
}

func TestDefaultDenySetCoversMetadata(t *testing.T) {
	for _, cidr := range DefaultDenySet() {
		if !cidr.IsValid() {
			t.Errorf("deny prefix %v invalid", cidr)
		}
	}
	// The cloud-metadata address must be in the set by construction
	// (link-local), asserted explicitly so a future refactor cannot drop it.
	g := newTestGuard(t, nil)
	_, err := g.DialContext(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil || errs.ClassOf(err) != errs.ClassSecurity {
		t.Error("169.254.169.254 (cloud metadata) must always be denied")
	}
}
