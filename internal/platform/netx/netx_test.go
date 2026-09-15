package netx

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

func newGuard(t *testing.T, allow []netip.Prefix) *Guard {
	t.Helper()
	g, err := NewGuard(GuardOptions{Allow: allow})
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	return g
}

// TestGuardDeniesDefaultSet is the SSRF red line: internal space must be
// refused by classification, before any connection is attempted.
func TestGuardDeniesDefaultSet(t *testing.T) {
	g := newGuard(t, nil)
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
			t.Errorf("dial %s: expected denial, got a connection", addr)
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

func TestGuardPermitsPublicPolicy(t *testing.T) {
	// Policy check only: unit tests must not dial the internet.
	g := newGuard(t, nil)
	pub, err := netip.ParseAddr("93.184.216.34")
	if err != nil {
		t.Fatal(err)
	}
	if ok, rule := g.permitted(pub); !ok {
		t.Errorf("public IP denied by rule %q", rule)
	}
	if g.DeniedTotal() != 0 {
		t.Error("a policy check must not register a denial")
	}
}

// TestGuardEveryAnswerChecked: a hostname resolving to even one internal
// address is refused entirely (split-horizon is a bypass signal).
func TestGuardEveryAnswerChecked(t *testing.T) {
	g := newGuard(t, nil)
	g.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		pub, _ := netip.ParseAddr("93.184.216.34")
		priv, _ := netip.ParseAddr("10.0.0.7")
		return []netip.Addr{pub, priv}, nil
	}
	_, err := g.DialContext(context.Background(), "tcp", "split.example.com:443")
	if err == nil || errs.ClassOf(err) != errs.ClassSecurity {
		t.Fatalf("split-horizon host must be ClassSecurity-refused, got %v", err)
	}
}

// TestGuardAllowOverridesDeny proves the full dial path hermetically: allow
// loopback, dial a real local listener.
func TestGuardAllowOverridesDeny(t *testing.T) {
	allow, _ := netip.ParsePrefix("127.0.0.0/8")
	g := newGuard(t, []netip.Prefix{allow})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
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

	conn, derr := g.DialContext(context.Background(), "tcp", ln.Addr().String())
	if derr != nil {
		t.Fatalf("allow-listed dial failed: %v", derr)
	}
	conn.Close()
	if g.DeniedTotal() != 0 {
		t.Errorf("DeniedTotal = %d, want 0", g.DeniedTotal())
	}
}

// TestGuardResolvesOnce closes the rebinding window: one dial, one resolve.
func TestGuardResolvesOnce(t *testing.T) {
	allow, _ := netip.ParsePrefix("127.0.0.0/8")
	g := newGuard(t, []netip.Prefix{allow})

	calls := 0
	lo, _ := netip.ParseAddr("127.0.0.1")
	g.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		calls++
		return []netip.Addr{lo}, nil
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
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

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	conn, derr := g.DialContext(context.Background(), "tcp", net.JoinHostPort("rebind.example.com", port))
	if derr != nil {
		t.Fatalf("dial: %v", derr)
	}
	conn.Close()
	if calls != 1 {
		t.Errorf("resolve called %d times for one dial — rebinding window open", calls)
	}
}

func TestGuardInvalidConfigFailsClosed(t *testing.T) {
	bad, _ := netip.ParsePrefix("10.0.0.1/33")
	_, err := NewGuard(GuardOptions{Allow: []netip.Prefix{bad}})
	if err == nil {
		t.Fatal("invalid allow prefix must be a config error")
	}
	if errs.ClassOf(err) != errs.ClassConfig {
		t.Errorf("class = %v, want ClassConfig", errs.ClassOf(err))
	}
}

func TestGuardMalformedAddress(t *testing.T) {
	g := newGuard(t, nil)
	_, err := g.DialContext(context.Background(), "tcp", "no-port")
	if err == nil || errs.ClassOf(err) != errs.ClassNetwork {
		t.Errorf("class = %v, want ClassNetwork", errs.ClassOf(err))
	}
}

func TestDefaultDenySetIncludesMetadata(t *testing.T) {
	for _, p := range DefaultDenySet() {
		if !p.IsValid() {
			t.Errorf("invalid deny prefix %v", p)
		}
	}
	g := newGuard(t, nil)
	_, err := g.DialContext(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil || errs.ClassOf(err) != errs.ClassSecurity {
		t.Error("cloud metadata address must always be denied")
	}
}

func TestListenUDPBinds(t *testing.T) {
	uc, err := ListenUDP(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	if uc.LocalAddr() == nil {
		t.Error("no local address")
	}
}

func TestListenRejectsReusePort(t *testing.T) {
	// Gated on the accept-scaling experiment: requesting it must fail loudly
	// rather than silently do nothing.
	_, err := Listen(context.Background(), "tcp", "127.0.0.1:0", ListenOptions{ReusePort: true})
	if err == nil {
		t.Fatal("ReusePort must be refused until the experiment admits it")
	}
	if errs.ClassOf(err) != errs.ClassConfig {
		t.Errorf("class = %v, want ClassConfig", errs.ClassOf(err))
	}
}