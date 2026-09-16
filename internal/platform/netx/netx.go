package netx

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// ListenOptions tunes listener construction. ReusePort is not currently
// supported and returns a configuration error when requested.
type ListenOptions struct {
	ReusePort bool
	Backlog   int
	KeepAlive time.Duration
}

// Listen builds a net.Listener.
func Listen(ctx context.Context, network, addr string, o ListenOptions) (net.Listener, error) {
	if o.ReusePort {
		return nil, errs.New(errs.ClassConfig, "netx.listen", "SO_REUSEPORT is not supported")
	}
	lc := net.ListenConfig{KeepAlive: o.KeepAlive}
	l, err := lc.Listen(ctx, network, addr)
	if err != nil {
		return nil, errs.Wrap(err, errs.ClassResource, "netx.listen", "bind failed: "+addr)
	}
	return l, nil
}

// ListenUDP binds a UDP socket for a data-plane listener. SO_REUSEPORT is
// deliberately not wired: it is gated on the accept-scaling experiment, and
// silently ignoring the option would hide that.
func ListenUDP(ctx context.Context, network, addr string) (*net.UDPConn, error) {
	lc := net.ListenConfig{}
	pc, err := lc.ListenPacket(ctx, network, addr)
	if err != nil {
		return nil, errs.Wrap(err, errs.ClassResource, "netx.listenudp", "bind failed: "+addr)
	}
	uc, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, errs.New(errs.ClassResource, "netx.listenudp", "not a UDP socket: "+addr)
	}
	return uc, nil
}

// denyRule pairs a prefix with the name used in refusal messages and logs.
type denyRule struct {
	prefix netip.Prefix
	name   string
}

// DefaultDenySet returns the SSRF deny-set: loopback, private
// v4 + ULA v6, link-local (incl. cloud metadata), multicast, unspecified,
// reserved, and CGNAT shared space.
func DefaultDenySet() []netip.Prefix {
	rs := defaultDenyRules()
	out := make([]netip.Prefix, len(rs))
	for i, r := range rs {
		out[i] = r.prefix
	}
	return out
}

func defaultDenyRules() []denyRule {
	must := func(cidr, name string) denyRule {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			panic("netx: invalid built-in deny prefix " + cidr)
		}
		return denyRule{p, name}
	}
	return []denyRule{
		must("127.0.0.0/8", "loopback"),
		must("::1/128", "loopback"),
		must("10.0.0.0/8", "private"),
		must("172.16.0.0/12", "private"),
		must("192.168.0.0/16", "private"),
		must("169.254.0.0/16", "link-local"),
		must("fe80::/10", "link-local"),
		must("fc00::/7", "ula"),
		must("224.0.0.0/4", "multicast"),
		must("ff00::/8", "multicast"),
		must("0.0.0.0/32", "unspecified"),
		must("::/128", "unspecified"),
		must("100.64.0.0/10", "cgnat"),
		must("240.0.0.0/4", "reserved"),
	}
}

// GuardOptions configures the outbound dial authorization. An empty Deny
// means "use the default deny-set" — never "allow all" (fail-closed).
type GuardOptions struct {
	Deny  []netip.Prefix
	Allow []netip.Prefix
}

// Guard is the single outbound dial authorization path.
type Guard struct {
	denyRules []denyRule
	allow     []netip.Prefix
	// resolve is swappable for tests; production resolves via the
	// DefaultResolver once per dial and never again (rebinding closure).
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	dialer  *net.Dialer
	denied  atomic.Uint64 // total denials; scraped into metrics wiring later
}

// NewGuard builds a Guard. Invalid prefixes are ClassConfig errors — a
// typo'd allow-list must never degrade into a weaker guard.
func NewGuard(o GuardOptions) (*Guard, error) {
	rules := defaultDenyRules()
	if len(o.Deny) > 0 {
		rules = rules[:0]
		for i, p := range o.Deny {
			if !p.IsValid() {
				return nil, errs.New(errs.ClassConfig, "netx.guard",
					fmt.Sprintf("invalid deny prefix at index %d", i))
			}
			rules = append(rules, denyRule{p, "configured"})
		}
	}
	for i, p := range o.Allow {
		if !p.IsValid() {
			return nil, errs.New(errs.ClassConfig, "netx.guard",
				fmt.Sprintf("invalid allow prefix at index %d", i))
		}
	}
	return &Guard{
		denyRules: rules,
		allow:     append([]netip.Prefix(nil), o.Allow...),
		resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dialer: &net.Dialer{},
	}, nil
}

// permitted reports whether addr is allowed: allow-list first (explicit
// operator override), then deny-set, then default-permit.
func (g *Guard) permitted(addr netip.Addr) (bool, string) {
	for _, p := range g.allow {
		if p.Contains(addr) {
			return true, ""
		}
	}
	for _, r := range g.denyRules {
		if r.prefix.Contains(addr) {
			return false, r.name
		}
	}
	return true, ""
}

// DeniedTotal reports how many dials this guard has refused.
func (g *Guard) DeniedTotal() uint64 { return g.denied.Load() }

// DialContext authorizes and dials. For hostnames it resolves once, checks
// every address, and dials the first permitted literal IP. Any denied
// address refuses the whole dial (split-horizon defense).
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, errs.Wrap(err, errs.ClassNetwork, "netx.guard",
			"malformed address: "+addr)
	}

	if ip, perr := netip.ParseAddr(host); perr == nil {
		if ok, rule := g.permitted(ip); !ok {
			g.denied.Add(1)
			return nil, g.denyErr(ip, rule)
		}
		return g.dialLiteral(ctx, network, ip, port)
	}

	ips, rerr := g.resolve(ctx, host)
	if rerr != nil {
		return nil, errs.Wrap(rerr, errs.ClassNetwork, "netx.guard",
			"resolution failed: "+host)
	}
	if len(ips) == 0 {
		return nil, errs.New(errs.ClassNetwork, "netx.guard",
			"no addresses for host: "+host)
	}

	// Strict multi-answer policy: every address must be permitted. One
	// denied answer refuses the dial — cherry-picking the permitted one
	// would trust a hostname that half-points at internal space.
	var chosen netip.Addr
	for _, ip := range ips {
		ok, rule := g.permitted(ip)
		if !ok {
			g.denied.Add(1)
			return nil, g.denyErr(ip, rule)
		}
		if !chosen.IsValid() {
			chosen = ip
		}
	}
	return g.dialLiteral(ctx, network, chosen, port)
}

func (g *Guard) dialLiteral(ctx context.Context, network string, ip netip.Addr, port string) (net.Conn, error) {
	conn, err := g.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	if err != nil {
		return nil, errs.Wrap(err, errs.ClassNetwork, "netx.guard",
			"dial failed: "+net.JoinHostPort(ip.String(), port))
	}
	return conn, nil
}

func (g *Guard) denyErr(ip netip.Addr, rule string) error {
	return errs.New(errs.ClassSecurity, "netx.guard",
		fmt.Sprintf("dial to %v refused by SSRF guard (rule: %s)", ip, rule))
}
