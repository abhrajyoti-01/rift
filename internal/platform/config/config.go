package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/abhrajyoti-01/rift/internal/platform/errs"
)

// Schema is the validated root configuration document: one file per
// deployment with service sections inside it.
type Schema struct {
	Observability Observability `yaml:"observability"`
	LB            LB            `yaml:"lb"`
	DNS           DNS           `yaml:"dns"`
	TLS           TLS           `yaml:"tls"`
	Media         Media         `yaml:"media"`
	Bench         Bench         `yaml:"bench"`
}

// Observability is the shared logging/metrics/admin configuration.
type Observability struct {
	LogLevel        string   `yaml:"log_level"`
	LogFormat       string   `yaml:"log_format"`
	AdminAddr       string   `yaml:"admin_addr"`
	AllowRemote     bool     `yaml:"allow_remote"`
	AdminTokenFile  string   `yaml:"admin_token_file"` // path; never inline
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`
}

// LB is the load balancer section.
type LB struct {
	Listeners           []Listener `yaml:"listeners"`
	Pools               []Pool     `yaml:"pools"`
	TrustedProxies      []string   `yaml:"trusted_proxies"`
	Forwarded           string     `yaml:"forwarded"`
	IdempotentPutDelete bool       `yaml:"idempotent_put_delete"`
}

// Listener binds one data-plane socket to one pool.
type Listener struct {
	ID       string   `yaml:"id"`
	Proto    string   `yaml:"proto"` // tcp|udp|http
	Bind     string   `yaml:"bind"`
	Pool     string   `yaml:"pool"`
	MaxConns int      `yaml:"max_conns"`
	TLS      *TLSTerm `yaml:"tls"`
}

// TLSTerm terminates TLS on a listener (key material by path only).
type TLSTerm struct {
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	MinVersion string `yaml:"min_version"`
}

// Pool is a backend set plus its picker, health, timeouts, retry, limits.
type Pool struct {
	ID          string        `yaml:"id"`
	Picker      string        `yaml:"picker"`
	Backends    []Backend     `yaml:"backends"`
	Health      HealthCheck   `yaml:"health"`
	Timeouts    PoolTimeouts  `yaml:"timeouts"`
	Retry       RetryPolicy   `yaml:"retry"`
	RateLimit   PoolRateLimit `yaml:"rate_limit"`
	MaxSessions int           `yaml:"max_sessions"` // UDP only
	SessionTTL  Duration      `yaml:"session_ttl"`  // UDP only
}

// Backend is one upstream target.
type Backend struct {
	ID     string `yaml:"id"`
	Addr   string `yaml:"addr"`
	Weight int    `yaml:"weight"`
}

// HealthCheck configures active checks.
type HealthCheck struct {
	Kind     string   `yaml:"kind"` // tcp|http
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
	Rise     int      `yaml:"rise"`
	Fall     int      `yaml:"fall"`
	Method   string   `yaml:"method"`
	Path     string   `yaml:"path"`
	Expect   []int    `yaml:"expect"`
}

// PoolTimeouts is the per-pool timeout set.
type PoolTimeouts struct {
	Dial  Duration `yaml:"dial"`
	Idle  Duration `yaml:"idle"`
	Copy  Duration `yaml:"copy"`
	Write Duration `yaml:"write"`
}

// RetryPolicy bounds retries per the closed method table.
type RetryPolicy struct {
	MaxAttempts int      `yaml:"max_attempts"`
	Backoff     Duration `yaml:"backoff"`
	MaxBackoff  Duration `yaml:"max_backoff"`
}

// PoolRateLimit is per-source and per-pool token buckets.
type PoolRateLimit struct {
	PerSource RateLimit `yaml:"per_source"`
	PerPool   RateLimit `yaml:"per_pool"`
}

// RateLimit is one token-bucket configuration.
type RateLimit struct {
	Rate  float64 `yaml:"rate"`
	Burst int     `yaml:"burst"`
}

// DNS is the DNS monitoring section (node or hub role).
type DNS struct {
	Role       string      `yaml:"role"`
	NodeID     string      `yaml:"node_id"`
	Location   string      `yaml:"location"`
	Resolvers  []Resolver  `yaml:"resolvers"`
	Targets    []DNSTarget `yaml:"targets"`
	Interval   Duration    `yaml:"interval"`
	MaxNSProbe int         `yaml:"max_ns_probe"`
	RingCap    int         `yaml:"ring_cap"`
	ShipEvery  Duration    `yaml:"ship_every"`
	ShipSize   int         `yaml:"ship_size"`
	SpoolMax   ByteSize    `yaml:"spool_max"`
	// SpoolDir is where observations are written while the hub is
	// unreachable. Empty disables the spool entirely (shipping failures then
	// count against dropped_ring_full instead of being buffered to disk).
	SpoolDir  string    `yaml:"spool_dir"`
	Hub       HubRef    `yaml:"hub"`
	HubServer HubServer `yaml:"hub_server"`
}

// Resolver is one upstream DNS resolver engine configuration.
type Resolver struct {
	Addr        string   `yaml:"addr"`
	Concurrency int      `yaml:"concurrency"`
	Timeout     Duration `yaml:"timeout"`
}

// DNSTarget is one monitored (zone, name, type, view) tuple.
type DNSTarget struct {
	Zone string `yaml:"zone"`
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	View string `yaml:"view"`
}

// HubRef points a node at its hub.
type HubRef struct {
	URL            string `yaml:"url"`
	ClientCertFile string `yaml:"client_cert_file"`
	ClientKeyFile  string `yaml:"client_key_file"`
	CACertFile     string `yaml:"ca_cert_file"`
}

// HubServer configures the hub's own listeners.
type HubServer struct {
	IngestAddr     string `yaml:"ingest_addr"`
	QueryAddr      string `yaml:"query_addr"`
	ServerCertFile string `yaml:"server_cert_file"`
	ServerKeyFile  string `yaml:"server_key_file"`
	ClientCAFile   string `yaml:"client_ca_file"`
	// AllowPlaintextIngest opts out of mTLS for ingest. It exists only for
	// local development; starting with it set logs a prominent warning, and
	// a routable ingest bind with it set is refused outright.
	AllowPlaintextIngest bool     `yaml:"allow_plaintext_ingest"`
	DataDir              string   `yaml:"data_dir"`
	WindowKeys           int      `yaml:"window_keys"`
	SegmentBytes         ByteSize `yaml:"segment_bytes"`
	MinResponding        int      `yaml:"min_responding"`
	ConvergenceWindow    Duration `yaml:"convergence_window"`
}

// TLS is the TLS monitoring section.
type TLS struct {
	Targets             []TLSTarget `yaml:"targets"`
	Interval            Duration    `yaml:"interval"`
	ExpiryWarnDays      int         `yaml:"expiry_warn_days"`
	ExpiryCritDays      int         `yaml:"expiry_crit_days"`
	ConsecutiveFailures int         `yaml:"consecutive_failures"`
	Root                string      `yaml:"root"`
}

// TLSTarget is one monitored TLS endpoint.
type TLSTarget struct {
	Host   string `yaml:"host"`
	Port   int    `yaml:"port"`
	Floor  string `yaml:"floor"`
	Verify string `yaml:"verify"`
}

// Media is the media server section.
type Media struct {
	Bind                string   `yaml:"bind"`
	Root                string   `yaml:"root"`
	MaxStreams          int      `yaml:"max_streams"`
	MaxStreamsPerClient int      `yaml:"max_streams_per_client"`
	IOMode              string   `yaml:"io_mode"`
	Readahead           string   `yaml:"readahead"`
	RateLimitPerClient  float64  `yaml:"rate_limit_per_client"`
	ReadHeaderTimeout   Duration `yaml:"read_header_timeout"`
	IdleTimeout         Duration `yaml:"idle_timeout"`
	WriteTimeout        Duration `yaml:"write_timeout"`
	FollowSymlinks      bool     `yaml:"follow_symlinks"`
	DiskHighWater       float64  `yaml:"disk_high_water"`
}

// Bench is the benchmark harness section.
type Bench struct {
	ScenarioDir string   `yaml:"scenario_dir"`
	ResultsDir  string   `yaml:"results_dir"`
	Baseline    string   `yaml:"baseline"`
	Timeout     Duration `yaml:"timeout"`
}

// Duration is a time.Duration that unmarshals from a Go duration string
// ("5s", "1m30s") or a bare number of seconds.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			*d = 0
			return nil
		}
		parsed, perr := time.ParseDuration(s)
		if perr != nil {
			return fmt.Errorf("invalid duration %q (want e.g. 5s, 1m30s)", s)
		}
		*d = Duration(parsed)
		return nil
	}
	var n int64
	if err := node.Decode(&n); err == nil {
		*d = Duration(time.Duration(n) * time.Second)
		return nil
	}
	return errors.New("invalid duration (want string like \"5s\" or seconds)")
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// ByteSize is a byte count that unmarshals from a plain integer or a
// suffixed string ("64MiB", "128KiB", "1G"), so a config can express units
// the way an operator thinks about them.
type ByteSize int64

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	var n int64
	if err := node.Decode(&n); err == nil {
		*b = ByteSize(n)
		return nil
	}
	var s string
	if err := node.Decode(&s); err != nil {
		return errors.New("invalid byte size (want an integer or e.g. \"64MiB\")")
	}
	parsed, perr := parseByteSize(s)
	if perr != nil {
		return perr
	}
	*b = ByteSize(parsed)
	return nil
}

// Bytes returns the size as an int64.
func (b ByteSize) Bytes() int64 { return int64(b) }

// Source names where a Schema came from.
type Source struct {
	Path       string
	EnvApplied []string
}

// Options tunes loading behavior.
type Options struct {
	// SkipEnv disables environment overrides (used by diff/replay tools
	// that must see the file exactly as written).
	SkipEnv bool
}

// Load reads, parses, and validates the YAML at path with environment
// overrides applied. Unknown fields are a hard error (typo safety beats
// forward-compatibility for a single-operator config).
func Load(path string) (*Schema, *Source, error) {
	return LoadWithOptions(path, Options{})
}

// LoadWithOptions is Load with explicit options.
func LoadWithOptions(path string, opts Options) (*Schema, *Source, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, errs.Wrap(err, errs.ClassConfig, "config.load", "read "+path)
	}
	src := &Source{Path: path}
	s, derr := Decode(raw)
	if derr != nil {
		return nil, src, derr
	}
	if !opts.SkipEnv {
		src.EnvApplied = ApplyEnv(s)
	}
	if err := Validate(s); err != nil {
		return nil, src, err
	}
	return s, src, nil
}

// Decode parses YAML into a Schema with strict unknown-field rejection.
func Decode(raw []byte) (*Schema, error) {
	var s Schema
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, errs.Wrap(err, errs.ClassConfig, "config.decode",
			"YAML parse or unknown field rejected")
	}
	return &s, nil
}

// ApplyEnv applies documented scalar env overrides (RIFT_<SECTION>_<KEY>)
// and returns the applied names, sorted. Undocumented keys do nothing —
// an env var that silently does nothing is the enemy of operators, so the
// set is explicit and small.
func ApplyEnv(s *Schema) []string {
	var applied []string
	str := func(env string, target *string) {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			*target = v
			applied = append(applied, env)
		}
	}
	boolean := func(env string, target *bool) {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*target = b
				applied = append(applied, env)
			}
		}
	}
	str("RIFT_OBSERVABILITY_LOG_LEVEL", &s.Observability.LogLevel)
	str("RIFT_OBSERVABILITY_ADMIN_ADDR", &s.Observability.AdminAddr)
	boolean("RIFT_OBSERVABILITY_ALLOW_REMOTE", &s.Observability.AllowRemote)
	str("RIFT_DNS_NODE_ID", &s.DNS.NodeID)
	str("RIFT_DNS_LOCATION", &s.DNS.Location)
	str("RIFT_DNS_HUB_URL", &s.DNS.Hub.URL)
	str("RIFT_MEDIA_ROOT", &s.Media.Root)
	str("RIFT_MEDIA_BIND", &s.Media.Bind)
	str("RIFT_BENCH_RESULTS_DIR", &s.Bench.ResultsDir)
	sort.Strings(applied)
	return applied
}

// Validate checks a Schema and returns every violation with a field path.
func Validate(s *Schema) error {
	if s == nil {
		return errs.New(errs.ClassConfig, "config.validate", "nil schema")
	}
	var v violations
	v.observability(&s.Observability)
	v.lb(&s.LB)
	v.dns(&s.DNS)
	v.tls(&s.TLS)
	v.media(&s.Media)
	v.bench(&s.Bench)
	if len(v) == 0 {
		return nil
	}
	return v.err()
}

// violations accumulates field-path-addressed problems.
type violations []string

func (v *violations) add(path, msg string) { *v = append(*v, path+": "+msg) }

func (v violations) err() error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d configuration violation(s):", len(v))
	for _, item := range v {
		b.WriteString("\n  - ")
		b.WriteString(item)
	}
	return errs.New(errs.ClassConfig, "config.validate", b.String())
}

func (v *violations) observability(o *Observability) {
	switch o.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		v.add("observability.log_level", "must be debug|info|warn|error, got "+strconv.Quote(o.LogLevel))
	}
	switch o.LogFormat {
	case "", "json", "text":
	default:
		v.add("observability.log_format", "must be json|text, got "+strconv.Quote(o.LogFormat))
	}
	if o.AdminAddr != "" {
		if err := checkHostPort(o.AdminAddr); err != nil {
			v.add("observability.admin_addr", err.Error())
		} else if !o.AllowRemote && !isLoopbackHostPort(o.AdminAddr) {
			v.add("observability.admin_addr",
				"routable admin bind requires allow_remote: true (refuse-to-start beats an accidentally open admin plane)")
		}
	}
}

func (v *violations) lb(l *LB) {
	poolIDs := map[string]bool{}
	seenPool := map[string]bool{}
	for i := range l.Pools {
		p := &l.Pools[i]
		base := fmt.Sprintf("lb.pools[%d]", i)
		if p.ID == "" {
			v.add(base+".id", "required")
		} else if seenPool[p.ID] {
			v.add(base+".id", "duplicate pool id "+strconv.Quote(p.ID))
		} else {
			seenPool[p.ID] = true
			poolIDs[p.ID] = true
		}
		switch p.Picker {
		case "", "round_robin", "weighted_round_robin", "least_connections":
		default:
			v.add(base+".picker", "must be round_robin|weighted_round_robin|least_connections, got "+strconv.Quote(p.Picker))
		}
		if len(p.Backends) == 0 {
			v.add(base+".backends", "at least one backend required")
		}
		seenBackend := map[string]bool{}
		for j := range p.Backends {
			b := &p.Backends[j]
			bb := fmt.Sprintf("%s.backends[%d]", base, j)
			if b.ID == "" {
				v.add(bb+".id", "required")
			} else if seenBackend[b.ID] {
				v.add(bb+".id", "duplicate backend id "+strconv.Quote(b.ID))
			} else {
				seenBackend[b.ID] = true
			}
			if err := checkHostPort(b.Addr); err != nil {
				v.add(bb+".addr", err.Error())
			}
			if b.Weight < 0 {
				v.add(bb+".weight", "must be >= 1 when set")
			}
		}
		if p.Health.Interval.D() < 0 || p.Health.Timeout.D() < 0 {
			v.add(base+".health", "interval/timeout must not be negative")
		}
		if p.Health.Timeout.D() > 0 && p.Health.Interval.D() > 0 && p.Health.Timeout.D() > p.Health.Interval.D() {
			v.add(base+".health.timeout", "must be <= interval")
		}
		switch p.Health.Kind {
		case "", "tcp", "http":
		default:
			v.add(base+".health.kind", "must be tcp|http, got "+strconv.Quote(p.Health.Kind))
		}
		if p.Health.Rise < 0 {
			v.add(base+".health.rise", "must be >= 0")
		}
		if p.Health.Fall < 0 {
			v.add(base+".health.fall", "must be >= 0")
		}
		for j, code := range p.Health.Expect {
			if code < 100 || code > 599 {
				v.add(fmt.Sprintf("%s.health.expect[%d]", base, j), "must be a valid HTTP status")
			}
		}
		if p.Retry.MaxAttempts < 0 {
			v.add(base+".retry.max_attempts", "must be >= 0")
		}
		v.rateLimit(base+".rate_limit.per_source", &p.RateLimit.PerSource)
		v.rateLimit(base+".rate_limit.per_pool", &p.RateLimit.PerPool)
		if p.MaxSessions < 0 {
			v.add(base+".max_sessions", "must be >= 0")
		}
	}

	seenListener := map[string]bool{}
	for i := range l.Listeners {
		ln := &l.Listeners[i]
		base := fmt.Sprintf("lb.listeners[%d]", i)
		if ln.ID == "" {
			v.add(base+".id", "required")
		} else if seenListener[ln.ID] {
			v.add(base+".id", "duplicate listener id "+strconv.Quote(ln.ID))
		} else {
			seenListener[ln.ID] = true
		}
		switch ln.Proto {
		case "tcp", "udp", "http":
		default:
			v.add(base+".proto", "must be tcp|udp|http, got "+strconv.Quote(ln.Proto))
		}
		if err := checkHostPort(ln.Bind); err != nil {
			v.add(base+".bind", err.Error())
		}
		if ln.Pool == "" {
			v.add(base+".pool", "required")
		} else if !poolIDs[ln.Pool] {
			v.add(base+".pool", "references unknown pool "+strconv.Quote(ln.Pool))
		}
		if ln.MaxConns < 0 {
			v.add(base+".max_conns", "must be >= 0")
		}
		if ln.TLS != nil {
			if ln.Proto == "udp" {
				v.add(base+".tls", "UDP listeners cannot terminate TLS")
			}
			if ln.TLS.CertFile == "" || ln.TLS.KeyFile == "" {
				v.add(base+".tls", "cert_file and key_file are both required")
			}
			v.tlsVersion(base+".tls.min_version", ln.TLS.MinVersion)
		}
	}
	switch l.Forwarded {
	case "", "rfc7239":
	default:
		v.add("lb.forwarded", "must be empty or rfc7239, got "+strconv.Quote(l.Forwarded))
	}
	for i, p := range l.TrustedProxies {
		if _, err := parseCIDROrIP(p); err != nil {
			v.add(fmt.Sprintf("lb.trusted_proxies[%d]", i), err.Error())
		}
	}
}

func (v *violations) rateLimit(path string, r *RateLimit) {
	if r.Rate < 0 {
		v.add(path+".rate", "must be >= 0")
	}
	if r.Burst < 0 {
		v.add(path+".burst", "must be >= 0")
	}
	if r.Rate > 0 && r.Burst > 0 && float64(r.Burst) < r.Rate {
		// A burst below one second of steady rate is a config smell that
		// makes the limiter effectively stricter than written.
		v.add(path+".burst", "should be >= rate (one second of tokens) to match intent")
	}
}

func (v *violations) dns(d *DNS) {
	switch d.Role {
	case "":
		// Role may be empty when no DNS service runs.
	case "node":
		if d.NodeID == "" {
			v.add("dns.node_id", "required for role node")
		}
		if len(d.Resolvers) == 0 {
			v.add("dns.resolvers", "at least one resolver required for role node")
		}
		if len(d.Targets) == 0 {
			v.add("dns.targets", "at least one target required for role node")
		}
		if d.Hub.URL == "" {
			v.add("dns.hub.url", "required for role node")
		}
		if d.RingCap != 0 && d.RingCap < 1024 {
			v.add("dns.ring_cap", "must be >= 1024 (hard memory ceiling below this is a config error)")
		}
		if d.SpoolMax < 0 || d.SpoolMax > 1<<30 {
			v.add("dns.spool_max", "must be within [0, 1 GiB]")
		}
		// A spool_dir that looks like a URL is a config mistake: the spool is
		// a filesystem path, and a URL here would create a directory named
		// after the scheme (or fail outright), silently disabling the offline
		// buffer that protects against hub outages.
		if d.SpoolDir != "" {
			if strings.Contains(d.SpoolDir, "://") {
				v.add("dns.spool_dir", "must be a filesystem path, not a URL")
			}
		}
		if d.Interval.D() < 0 {
			v.add("dns.interval", "must not be negative")
		}
		seen := map[string]bool{}
		for i := range d.Resolvers {
			r := &d.Resolvers[i]
			base := fmt.Sprintf("dns.resolvers[%d]", i)
			// Resolver addresses must be literal IP:port: the monitor must
			// not depend on DNS to bootstrap itself.
			if err := checkLiteralIPPort(r.Addr); err != nil {
				v.add(base+".addr", err.Error())
			}
			if seen[r.Addr] {
				v.add(base+".addr", "duplicate resolver "+strconv.Quote(r.Addr))
			}
			seen[r.Addr] = true
			if r.Concurrency < 0 || r.Concurrency > 1024 {
				v.add(base+".concurrency", "must be within [0, 1024]")
			}
			if r.Timeout.D() < 0 {
				v.add(base+".timeout", "must not be negative")
			}
		}
		for i := range d.Targets {
			t := &d.Targets[i]
			base := fmt.Sprintf("dns.targets[%d]", i)
			if err := checkDNSName(t.Name); err != nil {
				v.add(base+".name", err.Error())
			}
			if t.Zone != "" {
				if err := checkDNSName(t.Zone); err != nil {
					v.add(base+".zone", err.Error())
				}
			}
			switch strings.ToUpper(t.Type) {
			case "A", "AAAA", "CNAME", "MX", "TXT", "NS":
			default:
				v.add(base+".type", "must be A|AAAA|CNAME|MX|TXT|NS, got "+strconv.Quote(t.Type))
			}
			switch t.View {
			case "", "recursive", "authoritative", "both":
			default:
				v.add(base+".view", "must be recursive|authoritative|both, got "+strconv.Quote(t.View))
			}
		}
	case "hub":
		if d.HubServer.IngestAddr != "" {
			if err := checkHostPort(d.HubServer.IngestAddr); err != nil {
				v.add("dns.hub_server.ingest_addr", err.Error())
			}
		}
		if d.HubServer.QueryAddr != "" {
			if err := checkHostPort(d.HubServer.QueryAddr); err != nil {
				v.add("dns.hub_server.query_addr", err.Error())
			}
		}
		if d.HubServer.DataDir == "" {
			v.add("dns.hub_server.data_dir", "required for role hub")
		}
		// Ingest is mTLS by default. The plaintext opt-out is a development
		// affordance with teeth: it is refused on a routable bind.
		if d.HubServer.AllowPlaintextIngest {
			if d.HubServer.IngestAddr != "" && !isLoopbackHostPort(d.HubServer.IngestAddr) {
				v.add("dns.hub_server.allow_plaintext_ingest",
					"refused on a routable ingest bind: plaintext ingest is loopback-only")
			}
		} else {
			if d.HubServer.ServerCertFile == "" || d.HubServer.ServerKeyFile == "" {
				v.add("dns.hub_server", "server_cert_file and server_key_file required (ingest is mTLS)")
			}
			if d.HubServer.ClientCAFile == "" {
				v.add("dns.hub_server.client_ca_file", "required: node ingest is mTLS")
			}
		}
		if d.HubServer.WindowKeys != 0 && d.HubServer.WindowKeys < 16 {
			v.add("dns.hub_server.window_keys", "must be >= 16")
		}
		if d.HubServer.SegmentBytes < 0 {
			v.add("dns.hub_server.segment_bytes", "must be >= 0")
		}
		if d.HubServer.MinResponding < 0 {
			v.add("dns.hub_server.min_responding", "must be >= 0")
		}
	default:
		v.add("dns.role", "must be node|hub, got "+strconv.Quote(d.Role))
	}
}

func (v *violations) tls(t *TLS) {
	if t.ExpiryWarnDays < 0 || t.ExpiryCritDays < 0 {
		v.add("tls.expiry_warn_days", "must be >= 0")
	}
	if t.ExpiryWarnDays > 0 && t.ExpiryCritDays > 0 && t.ExpiryCritDays >= t.ExpiryWarnDays {
		v.add("tls.expiry_crit_days", "must be < expiry_warn_days")
	}
	if t.ConsecutiveFailures < 0 {
		v.add("tls.consecutive_failures", "must be >= 0")
	}
	if t.Interval.D() < 0 {
		v.add("tls.interval", "must not be negative")
	}
	for i := range t.Targets {
		tg := &t.Targets[i]
		base := fmt.Sprintf("tls.targets[%d]", i)
		if tg.Host == "" {
			v.add(base+".host", "required")
		} else if err := checkHostname(tg.Host); err != nil {
			v.add(base+".host", err.Error())
		}
		if tg.Port < 0 || tg.Port > 65535 {
			v.add(base+".port", "must be within [0, 65535]")
		}
		v.tlsVersion(base+".floor", tg.Floor)
		switch tg.Verify {
		case "", "report", "strict":
		default:
			v.add(base+".verify", "must be report|strict, got "+strconv.Quote(tg.Verify))
		}
	}
}

func (v *violations) tlsVersion(path, ver string) {
	switch ver {
	case "", "1.2", "1.3":
	default:
		v.add(path, "must be 1.2|1.3, got "+strconv.Quote(ver))
	}
}

func (v *violations) media(m *Media) {
	if m.Bind != "" {
		if err := checkHostPort(m.Bind); err != nil {
			v.add("media.bind", err.Error())
		}
	}
	// media.root is required only when the media server is actually
	// configured (bind or any other media field set). A config that never
	// runs the media server must not be forced to declare a root.
	mediaConfigured := m.Bind != "" || m.Root != "" || m.MaxStreams != 0 ||
		m.IOMode != "" || m.Readahead != ""
	if mediaConfigured && m.Root == "" {
		v.add("media.root", "required when the media server is configured")
	}
	if m.MaxStreams < 0 || m.MaxStreamsPerClient < 0 {
		v.add("media.max_streams", "must be >= 0")
	}
	if m.MaxStreams > 0 && m.MaxStreamsPerClient > m.MaxStreams {
		v.add("media.max_streams_per_client", "must be <= max_streams")
	}
	switch m.IOMode {
	case "", "sendfile", "buffered":
	default:
		v.add("media.io_mode", "must be sendfile|buffered, got "+strconv.Quote(m.IOMode))
	}
	if m.Readahead != "" {
		if _, err := parseByteSize(m.Readahead); err != nil {
			v.add("media.readahead", err.Error())
		}
	}
	if m.RateLimitPerClient < 0 {
		v.add("media.rate_limit_per_client", "must be >= 0")
	}
	if m.DiskHighWater < 0 || m.DiskHighWater > 1 {
		v.add("media.disk_high_water", "must be within (0, 1]")
	}
}

func (v *violations) bench(b *Bench) {
	if b.Timeout.D() < 0 {
		v.add("bench.timeout", "must not be negative")
	}
}

// Redacted returns a deep copy with every secret-bearing field replaced by
// a fixed marker. This is the ONLY rendering any echo path (diff, snapshot
// API, error messages, logs) may use.
func Redacted(s *Schema) *Schema {
	if s == nil {
		return nil
	}
	cp := *s
	redactSecret(&cp)
	return &cp
}

const redactedMarker = "<redacted>"

func redactSecret(s *Schema) {
	if s.Observability.AdminTokenFile != "" {
		s.Observability.AdminTokenFile = redactedMarker
	}
	if s.LB.Listeners != nil {
		ls := make([]Listener, len(s.LB.Listeners))
		copy(ls, s.LB.Listeners)
		for i := range ls {
			if ls[i].TLS != nil {
				t := *ls[i].TLS
				if t.KeyFile != "" {
					t.KeyFile = redactedMarker
				}
				ls[i].TLS = &t
			}
		}
		s.LB.Listeners = ls
	}
	hs := s.DNS.HubServer
	if hs.ServerKeyFile != "" {
		hs.ServerKeyFile = redactedMarker
	}
	s.DNS.HubServer = hs
	h := s.DNS.Hub
	if h.ClientKeyFile != "" {
		h.ClientKeyFile = redactedMarker
	}
	s.DNS.Hub = h
}

// Diff is a stable, field-path-addressed difference between two Schemas.
type Diff struct {
	Changes []FieldChange
}

// FieldChange is one changed field, addressed by its config path. Values
// are rendered from the REDACTED schema so a diff never leaks a secret.
type FieldChange struct {
	Path string
	From string
	To   string
}

// Empty reports whether the diff has no changes.
func (d *Diff) Empty() bool { return d == nil || len(d.Changes) == 0 }

// String renders the diff one change per line, sorted by path.
func (d *Diff) String() string {
	if d.Empty() {
		return "(no changes)"
	}
	var b strings.Builder
	for i, c := range d.Changes {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s: %s -> %s", c.Path, c.From, c.To)
	}
	return b.String()
}

// DiffSchemas computes the field-path diff between two schemas. Both sides
// are redacted first, so secrets never appear even in the diff.
func DiffSchemas(oldS, newS *Schema) *Diff {
	d := &Diff{}
	if oldS == nil || newS == nil {
		return d
	}
	o := Redacted(oldS)
	n := Redacted(newS)
	walkDiff(reflect.ValueOf(o).Elem(), reflect.ValueOf(n).Elem(), "", d)
	sort.Slice(d.Changes, func(i, j int) bool { return d.Changes[i].Path < d.Changes[j].Path })
	return d
}

func walkDiff(o, n reflect.Value, path string, d *Diff) {
	switch o.Kind() {
	case reflect.Pointer:
		if o.IsNil() && n.IsNil() {
			return
		}
		if o.IsNil() != n.IsNil() {
			d.Changes = append(d.Changes, FieldChange{path, fmtVal(o), fmtVal(n)})
			return
		}
		walkDiff(o.Elem(), n.Elem(), path, d)
	case reflect.Struct:
		t := o.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := f.Tag.Get("yaml")
			if name == "" || name == "-" {
				name = strings.ToLower(f.Name)
			}
			name = strings.Split(name, ",")[0]
			child := name
			if path != "" {
				child = path + "." + name
			}
			walkDiff(o.Field(i), n.Field(i), child, d)
		}
	case reflect.Slice:
		if o.Len() != n.Len() {
			d.Changes = append(d.Changes, FieldChange{path, fmt.Sprintf("len=%d", o.Len()), fmt.Sprintf("len=%d", n.Len())})
			return
		}
		for i := 0; i < o.Len(); i++ {
			walkDiff(o.Index(i), n.Index(i), fmt.Sprintf("%s[%d]", path, i), d)
		}
	default:
		if !reflect.DeepEqual(o.Interface(), n.Interface()) {
			d.Changes = append(d.Changes, FieldChange{path, fmtVal(o), fmtVal(n)})
		}
	}
}

func fmtVal(v reflect.Value) string {
	if !v.IsValid() {
		return "<invalid>"
	}
	return fmt.Sprintf("%v", v.Interface())
}
