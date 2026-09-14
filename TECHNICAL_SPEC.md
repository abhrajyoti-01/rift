# TECHNICAL_SPEC.md — RIFT Technical Specification

Status: **Revision A — normative**. This document is the identifier oracle: the
skeleton and all later code must compile against the names defined here. Where
this document and `ARCHITECTURE.md` overlap, `ARCHITECTURE.md` owns boundaries
and data flow, this file owns names, types, algorithms, and concurrency detail.
Toolchain: Go 1.25.5. Cross-references use stable IDs (`AD-n`, `FR-n`, `NFR-n`,
`UD-n`, `E-n`) so renames of documents do not sever them.

---

## 1. Error Model — `internal/platform/errs`

`Class` drives exactly three things — log level, metric label, retry eligibility
— and nothing else. It is a small int: free on the hot path, Prometheus-safe as a
label value.

```go
package errs

type Class uint8

const (
    ClassUnknown    Class = iota // must be reported; never silently retried
    ClassNetwork                 // transport failure; retryable per policy
    ClassTimeout                 // deadline exceeded; metric distinct from ClassNetwork
    ClassPeerClosed              // normal EOF/RST; not a fault, excluded from error-rate SLO
    ClassConfig                  // startup-time config fault; exit 2
    ClassSecurity                // deny-set hit, oversize, smuggling; never retried
    ClassResource                // limit reached; shed load, never queue unbounded
)

// Error carries one Class plus wrapped context. Construct via Wrap, never fmt.Errorf.
type Error struct {
    Class Class
    Op    string // dotted operation path: "lb.l4.copyLoop"
    Msg   string
    Err   error  // wrapped; may be nil
}

func New(class Class, op, msg string) *Error
func Wrap(err error, class Class, op, msg string) *Error
func (e *Error) Error() string   // "op: msg: err"
func (e *Error) Unwrap() error
func ClassOf(err error) Class    // ClassUnknown if not *Error and no *Error in chain
```

Rules:

- Data-plane code paths construct errors only via `errs.New`/`errs.Wrap`
  (allocation budget: error construction happens on failure paths only —
  zero-cost on the happy path is a hard budget).
- `ClassOf` is the *only* classifier. No error-string matching anywhere in the
  tree (CI grep check `T-34`).
- Exit-code mapping lives in `errs.ExitCode(err)`: 0 clean, 1 runtime fault,
  2 `ClassConfig` at startup, 3 bind/resource failure, 4 drain deadline exceeded.
  `124` is reserved for the bench harness timeout and set by `bench/harness`,
  not by `errs`.

Two sentinels ship with the Phase-0 skeleton and belong to this contract:

- `ErrNotImplemented` — returned by `cmd/rift` for subcommands whose ROADMAP
  phase has not landed. A CI grep asserts it does not survive to v1.
- `ErrDrainExceeded` — the `ClassResource` sentinel that `ExitCode` maps to
  exit code 4; raised by `lifecycle` when the drain budget fires (§13).

---

## 2. Platform Concurrency Primitives

### 2.1 Bounded executor — `internal/platform/pool`

Used where the work unit is *not* a socket: health checks, DNS queries, loadgen
workers. Never used in front of a blocking `Read` (AR: §6 of ARCHITECTURE.md).

```go
package pool

var ErrClosed = errors.New("pool: closed")
var ErrFull   = errors.New("pool: queue full")

type Executor[T any] struct{ /* shards: chans of T */ }

// NewExecutor builds a bounded executor. workers ≥ 1; queuePerWorker bounds the
// buffered channel feeding each worker. Submit returns ErrFull rather than
// blocking the caller — backpressure is the caller's visible decision, never a
// hidden one.
func NewExecutor[T any](workers, queuePerWorker int) *Executor[T]
func (e *Executor[T]) Submit(t T) error      // ErrFull | ErrClosed
func (e *Executor[T]) Run(ctx context.Context, fn func(ctx context.Context, t T))
func (e *Executor[T]) Close(ctx context.Context) error // drains within ctx budget
```

### 2.2 Sharded rate limiter — `internal/platform/ratelimit`

Per-key token bucket over a sharded key map with bounded cardinality (an
unbounded key map is a memory-exhaustion primitive reachable by spoofed
sources).

```go
package ratelimit

type Rate struct { PerSecond float64; Burst int }

type Sharded struct{ /* shards []struct{ mu sync.Mutex; m *lru } */ }

// NewSharded: shards rounded to power of two, ≥ 8. capacity bounds distinct
// tracked keys globally; evicted keys lose their history (documented, counted
// in rift_ratelimit_keys_evicted_total).
func NewSharded(shards, capacity int) *Sharded
func (s *Sharded) Allow(key string) bool        // consumes 1 token
func (s *Sharded) AllowN(key string, n int) bool
func (s *Sharded) Set(key string, r Rate)       // config-time only
```

Key hashing: FNV-1a 64 → shard by high bits, so distinct spoofed sources spread
across shards instead of serializing on one mutex.

### 2.3 Circuit breaker — `internal/platform/circuit`

```go
package circuit

type Config struct {
    FailureThreshold int           // consecutive failures to open (default 5)
    RecoveryCooldown time.Duration // open → half-open (default 10s)
    SuccessThreshold int           // half-open successes to close (default 2)
}

type Breaker struct{ /* now func() time.Time injected for tests */ }
func New(cfg Config, now func() time.Time) *Breaker
func (b *Breaker) Allow() bool      // false while open
func (b *Breaker) Success()
func (b *Breaker) Failure()
```

Clock injection is mandatory (AR-8): breaker timing tests run under
`testing/synctest`, never with real sleeps.

### 2.4 Retry policy — `internal/platform/retry`

The policy is a *type*, not a service. Each subsystem decides where it applies;
the LB applies it only through the closed table in §5.6.

```go
package retry

type Backoff struct { Base, Cap time.Duration; Jitter float64 } // Jitter ∈ [0,1]

type Policy struct {
    MaxAttempts int    // ≥ 1; 1 disables
    Backoff     Backoff
    RetryOn     func(err error) bool // must consult errs.ClassOf, never strings
}

func (p Policy) Attempt(ctx context.Context, n int, err error) (time.Duration, bool)
```

`Attempt` returns the delay for attempt `n+1` and whether to proceed. It never
sleeps — the caller sleeps under its own context so synctest can drive it.

---

## 3. Service Lifecycle — `internal/platform/lifecycle`

Fixed shutdown ordering (ARCHITECTURE §6). Phases are observable on `/healthz`
and in the exit log line.

```go
package lifecycle

type Phase int32
const (
    PhaseInit Phase = iota
    PhaseStarting
    PhaseServing
    PhaseDraining   // not-ready set; accepting stopped; in-flight work bounded-drains
    PhaseFlushing   // metrics + hub segments flushed; upstream pools closed
    PhaseStopped
)

type Service interface {
    Name() string
    // Start returns once the service is ready to serve, or with an error that
    // aborts startup (mapped through errs.ExitCode).
    Start(ctx context.Context) error
    // Stop receives a ctx carrying this service's drain budget and must return
    // by its deadline. Exceeding it is reported, not hidden.
    Stop(ctx context.Context) error
}

type App struct{ /* services []Service; phase atomic.Int32 */ }

func New(name string, services ...Service) *App
func (a *App) Run(ctx context.Context) error // blocks until signal or fatal
func (a *App) Phase() Phase
```

Budget math: total shutdown budget `T` (config `shutdown_timeout`, default 15s)
splits top-down: stop-accepting is immediate; drain receives `T − 1s − 5%`;
flush receives the remainder. If drain fires its deadline the process exits 4
with a `"drain deadline exceeded"` log carrying in-flight counts — a visible
failure, never a hang (FR-3).

---

## 4. Load Balancer — `internal/lb`

### 4.1 Model — `internal/lb/model`

```go
type PoolID string
type ListenerID string

// Backend is the unit of health and load accounting. Immutable identity
// (ID, Addr, Weight); mutable state is atomic only. Backends are shared
// across Snapshots when unchanged (copy-on-write on reload).
type Backend struct {
    ID     string
    Addr   string // host:port, validated at config time
    Weight int    // ≥ 1

    Conns  atomic.Int64  // in-flight for least-connections; ± on connect/close
    Health atomic.Bool   // written only by the health system
}

type Pool struct {
    ID       PoolID
    Backends []*Backend // ordered, immutable per snapshot
}

type ListenerSpec struct {
    ID       ListenerID
    Proto    string // "tcp" | "udp" | "http"
    Bind     string // host:port
    Pool     PoolID
    MaxConns int
    // TLS: nil terminates cleartext
    TLS *TLSConfig
}

type Snapshot struct {
    Version   uint64      // increments per accepted reload
    Listeners []ListenerSpec
    Pools     map[PoolID]*Pool
}
```

Data plane reads the snapshot through one `atomic.Pointer[Snapshot]` owned by
`lb/control`. **No mutex is shared between control and data planes** (AD-6).

### 4.2 Pickers — `internal/lb/picker`

```go
package picker

var ErrNoUpstream = errors.New("lb: no healthy upstream")

// PickHint carries only what pickers may need; v1 pickers read backend state
// directly and ignore the hint. It exists so a hash-based picker (future)
// does not change the signature.
type PickHint struct {
    SourceIP netip.Addr // may be invalid (zero) for non-IP transports
}

// Picker implementations MUST be safe for concurrent use and MUST NOT
// allocate on the pick path (NFR-2: alloc-free, p99 < 1µs @ 1000 backends).
type Picker interface {
    Pick(ctx context.Context, hint PickHint) (*model.Backend, error)
}

// New builds from config; kind ∈ round_robin | weighted_round_robin |
// least_connections. Backends slice is read-only after construction.
func New(kind string, pool *model.Pool) (Picker, error)
```

**round_robin** — single `atomic.Uint64` counter; index modulo healthy count.
Skips unhealthy backends without allocating.

**weighted_round_robin** — smooth WRR (nginx algorithm): per-backend current
weight `cw[i] += w[i]`; pick `argmax(cw)`; `cw[arg] -= total`. Properties:
interleaves heavy backends (no burst of k consecutive picks for weight k),
max-min fair, one mutex for the small integer array — the mutex is *measured*
(§7 of PERFORMANCE_SPEC); if contention shows superlinear at G≥16, a sharded
counter variant replaces it, decided only by that benchmark.

**least_connections** — scan `pool.Backends`, track min `Conns` among healthy;
tie-break by round-robin counter. No lock: `Conns` is atomic per backend.

Picker state is rebuilt on reload (distribution resets). Documented trade-off:
reload frequency is an operator event, not a steady-state cost.

### 4.3 Active health — `internal/lb/health`

```go
package health

type CheckResult struct {
    Healthy bool
    Latency time.Duration
    Detail  string // for logs only; never a metric label (closed label set)
}

type Checker interface {
    Check(ctx context.Context, b *model.Backend) CheckResult
}

type TCPChecker struct{ Timeout time.Duration }        // connect success
type HTTPChecker struct {                              // GET, expect status set
    Timeout  time.Duration
    Method, Path, HostHeader string
    Expect   []int // e.g. {200, 204}; empty = {200}
}
func New(cfg Config) Checker

type Config struct {
    Interval time.Duration // default 5s
    Timeout  time.Duration // default 2s
    Rise     int           // default 2  — healthy after N passes
    Fall     int           // default 3  — unhealthy after N fails
    Kind     string        // tcp | http
}
```

One scheduler goroutine per pool; checks execute on a `pool.Executor` (workers
= min(len(backends), 8)); state transitions apply rise/fall thresholds before
writing `Backend.Health`. All timing via injected ticker (synctest-tested).

### 4.4 L4 TCP forwarding — `internal/lb/l4`

```go
type Forwarder struct{ /* snapshot source, limits, metrics */ }
func New(cfg Config, snap func() *model.Snapshot) *Forwarder
func (f *Forwarder) Serve(ctx context.Context, l net.Listener) error

type Config struct {
    DialTimeout     time.Duration // default 3s
    IdleTimeout     time.Duration // default 60s, per-direction
    CopyDeadline    time.Duration // default 30s, reset per Read/Write
    MaxConns        int           // per listener; admission gate
    Splice          bool          // true: ReadFrom path; false: pooled 8KiB copy
}
```

Connection lifecycle (one goroutine per accepted conn — ARCHITECTURE §6):

1. Admission: `maxConns` exceeded → immediate close + counter
   `rift_lb_admissions_rejected_total{reason}` — **never a queue**.
2. `SetNoDelay(true)`, `SetKeepAliveConfig(KeepAliveConfig{Enable: true, Idle: 30s, Interval: 15s})`.
3. Pick backend; dial upstream under `DialTimeout`; dial failure → pick next
   healthy backend, at most `MaxRetries` (default 2) attempts, each attempt on a
   *different* backend.
4. Forward bidirectionally with `dst.ReadFrom(src)` when `Splice` (verified
   available: `*net.TCPConn.ReadFrom` takes the runtime splice fast path for
   socket sources on Linux; E1 confirms by `strace -c` before the path ships,
   otherwise the pooled-copy loop is the shipped implementation).
5. Half-close: client EOF → `CloseWrite` upstream, keep draining the response
   until upstream EOF or idle deadline. Skipping this truncates large responses
   behind request-body-fin.
6. Teardown: `CloseRead`-drain, conn accounting `−1`, metrics.

### 4.5 UDP sessions — `internal/lb/udp`

```go
type SessionID string // hex(srcaddr)|hex(dstaddr) — 5-tuple-normalized

type Session struct {
    ID       SessionID
    Upstream *net.UDPConn   // one connected socket per session
    LastSeen atomic.Int64   // unix nanos
    UpPkts, UpBytes, DownPkts, DownBytes atomic.Uint64
}

type Server struct{ /* session map, sweeper, picker */ }
func New(cfg Config, snap func() *model.Snapshot) *Server
func (s *Server) Serve(ctx context.Context, conn *net.UDPConn) error
```

- One read-loop goroutine on the shared listener socket; per-session one
  upstream socket plus one reply goroutine (upstream→client).
- Backend affinity is per-session (re-picking mid-flow breaks application flow
  state — AD-14). Backend re-pick happens only if the session's backend goes
  unhealthy *and* the client sends again after a sweep marked it.
- `max_sessions` (default 4096) is a hard admission gate; overflow → drop with
  `rift_lb_udp_dropped_total{reason="table_full"}`, distinct from
  `reason="idle_sweep"`. Unbounded session tables are a memory bomb reachable
  by one `sendto` loop.
- Idle sweep at 30s; TTL `session_ttl` default 120s.
- UDP has no congestion signal: drop-with-counter is the *only* backpressure;
  the spec says so rather than pretending flow control exists.

### 4.6 L7 HTTP — `internal/lb/l7`

`httputil.ReverseProxy` with three owned extension points (AD-3): our
`http.Transport`, a `sync.Pool[[]byte]` buffer pool, `ModifyResponse`/`ErrorHandler`
hooks. Framing stays stdlib-owned: CL+TE ambiguity is a smuggling class we
reject upstream of the proxy (`httpx.Middleware`), never re-implement.

Transport config derived from pool config: `MaxIdleConnsPerHost` per backend
(default 32), `MaxIdleConns` global, `IdleConnTimeout`, `ExpectContinueTimeout`,
`ForceAttemptHTTP2: false` (backend cleartext H2 off by default —
`Server.Protocols`/`Transport` gate; enabling it logs a warning, AD-3).

`X-Forwarded-For`: append-only; inbound value trusted for attribution only when
the peer is in `trusted_proxies` (config, default empty — trust nobody). Full
`Forwarded` (RFC 7239) emitted when `forwarded: rfc7239` is set.

### 4.7 Retry gate (closed table)

Retry eligibility is method-keyed and phase-aware. Phase = `connect` (failure
before any byte written to the backend socket, established via `httptrace`
`WroteRequest`/`GotConn` events) or `post-write`. Blanket retry is rejected (AD-15).

| Method | Connect-phase failure | Post-write failure |
|---|---|---|
| GET, HEAD, OPTIONS, TRACE | retry ≤ `max_attempts` (default 2) | retry, same budget |
| POST | retry | **never** |
| PUT, DELETE | retry | once, only if `idempotent_put_delete: true` in config |
| PATCH | retry | **never** |
| any other | **never** | **never** |

Every retry increments `rift_lb_retries_total{pool,backend,outcome}` with
`outcome ∈ retried | final | refused_nonidempotent`.

### 4.8 Control plane — `internal/lb/control`

Admin API (loopback by default; `allow_remote` + token required otherwise):
`POST /v1/admin/reload`, `GET /v1/snapshot`, `GET /v1/pools/{id}/backends`.
Reload path: read → validate → build `Snapshot` → `atomic.Pointer.Store` → log
`config.Diff` → drain removed backends (stop picking; keep established conns
until idle deadline). `SIGHUP` and the API entry point hit the same code; a
config that fails validation can reach the data plane through neither.

---

## 5. DNS Monitoring — `internal/dnsmon`

### 5.1 Wire engine — `internal/dnsmon/wire`

Own RFC 1035 implementation (AD-4: stdlib exposes no TTL — `net.NS` is
`{Host string}`, `net.MX` is `{Host, Pref}`, `LookupCNAME` returns bare
strings — verified against the pinned toolchain; rcode and TC are invisible in
`net.DNSError`).

```go
package wire

type Type uint16
const (
    TypeA     Type = 1
    TypeNS    Type = 2
    TypeCNAME Type = 5
    TypeMX    Type = 15
    TypeTXT   Type = 16
    TypeAAAA  Type = 28
)

type Question struct { Name string; Type Type; Class uint16 /* IN=1 */ }

type RData interface{ rdata() }
type A struct{ Addr netip.Addr }
type AAAA struct{ Addr netip.Addr }
type CNAME struct{ Target string }
type MX struct{ Pref uint16; Host string }
type TXT struct{ Strings []string }
type NS struct{ Host string }
type Unknown struct{ Raw []byte } // forward unparsed; never guess

type RR struct { Name string; Type Type; Class uint16; TTL uint32; Data RData }

type Message struct {
    ID     uint16
    Opcode uint8
    RD, TC, AA, RA bool
    RCode  uint8
    Question []Question
    Answers, Authority, Additional []RR
}

func EncodeQuery(q Question, ednsUDPSize uint16) []byte // 1 alloc (the buffer)
func DecodeMessage(buf []byte) (*Message, error)        // ≤ 3 allocs
func (m *Message) RCodeString() string                  // "NOERROR" | "SERVFAIL" | ...
```

Hardening rules (each is a fuzz corpus entry class — TESTING_SPEC §6):

- **Size caps**: decode accepts ≤ 512 bytes for legacy UDP responses, ≤ 1232
  with EDNS0 advertised, ≤ 65535 over TCP. Larger → `ErrMessageTooLarge`
  (`ClassSecurity`) and connection discard.
- **Compression pointers**: decode is an iterative loop (no recursion — no
  stack risk). A pointer **must target a strictly earlier offset**; any forward
  or self pointer is `ErrBadPointer`. Chained backwards pointers are bounded by
  message size, so loops are structurally impossible. Name length ≤ 255
  octets (RFC 1035 §2.3.4); violations are `ErrNameTooLong`.
- **Queries are sent uncompressed** (encode does not compress — compression is
  only ever parsed, never produced; saves the encoder the whole pointer-jump
  correctness surface).
- **Transaction matching**: one connected UDP socket per in-flight query
  (ephemeral source port, kernel-filters the source), response matched on full
  socket + ID + question. ID is `crypto/rand`-sourced 16-bit. No shared
  query socket in v1 — isolation beats a marginal socket saving at monitoring
  QPS.
- **TCP fallback**: on `TC=1`, re-query over TCP with a 2-byte length prefix
  (RFC 1035 §4.2.2, RFC 7766). Both attempts carry the same question; a fresh
  ID is fine.
- Labels: 0–63 octets each; total ≤ 255; `.` root is length 0.

### 5.2 Resolver engine — `internal/dnsmon/resolver`

```go
type EngineConfig struct {
    Resolver   netip.AddrPort // e.g. 1.1.1.1:53
    Concurrency int           // per-resolver worker cap, default 8
    Timeout     time.Duration // per attempt, default 2s
    EDNSUDPSize uint16         // 1232
}

type Engine struct{ /* pool.Executor, stats atomics, breaker */ }
func NewEngine(cfg EngineConfig, now func() time.Time) *Engine

// Query returns the final message (post TCP fallback if any), the measured
// resolver latency (first byte to last byte of the response), and a classified
// error. Never retries internally — scheduling owns retry.
func (e *Engine) Query(ctx context.Context, q wire.Question) (wire.Message, time.Duration, error)
```

Per-resolver stats: queries, timeouts, truncated, rcodes (fixed label set),
in-flight gauge. A resolver exceeding failure threshold opens its breaker
(circuit.Breaker) and is skipped until half-open — a slow authoritative server
must not consume the node's whole budget.

### 5.3 Views and probing — `internal/dnsmon/probe`

```go
type View uint8
const (
    ViewRecursive     View = iota + 1 // RD=1 → configured public resolvers
    ViewAuthoritative                 // RD=0 → zone NS servers directly
)
```

- **Recursive view** answers "what do these resolvers serve clients right now".
- **Authoritative view**: discover the zone's NS set (query parent zones up to
  the TLD for referrals, then query each NS with `RD=0`), then query the NS
  servers. v1 caps NS servers queried per zone (`max_ns_probe`, default 4) —
  FD safety first (UD-5).
- **Client-facing view** is *documented only* in v1: what an actual client
  population sees depends on ISP resolvers, cache layers and TTLs we cannot see
  from these vantage points. This is a stated limitation, not a gap to paper
  over. Dashboards label the two implemented views separately and never merge
  them.

### 5.4 Observation and propagation — `internal/dnsmon/model`

```go
type Target struct {
    Zone  string // "example.com."
    Name  string // "www.example.com."
    Type  wire.Type
    View  View
}

type Answer struct { Name string; Type wire.Type; TTL uint32; Data string }

type Observation struct {
    NodeID     string        // from node identity
    View       View
    Resolver   string        // "1.1.1.1:53" or "ns1.example.com:53"
    QName      string
    QType      wire.Type
    RCode      uint8
    Answers    []Answer      // canonical: sorted by (Name, Type, Data)
    Truncated  bool
    Transport  string        // "udp" | "tcp"
    Latency    time.Duration
    Timestamp  time.Time     // UTC
    ErrClass   errs.Class    // 0 when RCode is a real answer
}

type PropagationState uint8
const (
    PropStateUnknown PropagationState = iota // no data in window
    PropInsufficientCoverage                  // < min_responding distinct resolvers
    PropUnresolvable                          // authoritative view itself failing
    PropDivergent                             // ≥1 resolver fingerprint ≠ reference
    PropConverging                            // reference changed recently; partial agreement
    PropConverged                             // all responding resolvers match reference
)
```

**Fingerprint** (the comparison unit): sorted set of `Answer.Data` strings for
the target, from a single observation. TTL is deliberately *excluded* from the
fingerprint (TTL decay alone must not read as propagation divergence), but is
recorded per observation.

Classifier (evaluated by the hub over the trailing window, default 10 min):

1. Authoritative observations exist and succeeded? No → `PropUnresolvable`.
2. Distinct responsive resolvers ≥ `min_responding` (default 3)? No →
   `PropInsufficientCoverage`.
3. Reference = fingerprint from the freshest successful authoritative
   observation. Every recursive observation's fingerprint compared to reference;
   any mismatch → `PropDivergent`.
4. Reference timestamp within `convergence_window` (default 5 min) of newest
   observation → `PropConverging` unless all match; all match → `PropConverged`.

The state machine emits `rift_dns_propagation_state{target,view}` as a numeric
gauge (enum value) plus `rift_dns_view_divergence_seconds` — elapsed since the
authoritative change until every responding resolver matches. **No code path
anywhere emits a "propagated worldwide" verdict** (G4; enforced by a test that
greps the enum — `T-51`).

### 5.5 Node — `internal/dnsmon/node`

```go
type NodeConfig struct {
    NodeID    string   // operator-assigned identity, e.g. "ws2-home-01"
    Location  string   // operator-assigned location label, closed set in config
    Resolvers []resolver.EngineConfig
    Targets   []model.Target
    Interval  time.Duration // per target, default 60s
    RingCap  int           // observations; default 65536; hard memory ceiling
    ShipEvery time.Duration // batch trigger, default 5s
    ShipSize  int           // batch trigger, default 512 observations
    HubURL    string       // https://hub:9001
    SpoolMax  int64        // offline JSONL spool bytes; default 64 MiB; hard cap
}
```

Scheduler goroutine per target set; per-resolver `Engine` with its own
executor. Ring buffer: fixed capacity, `Put` drops-oldest and increments
`rift_dns_observations_dropped_total{reason="ring_full"}` — loss is counted,
never silent. Shipper: batches on size *or* age trigger, POSTs
`/v1/ingest` over mTLS; on 429/5xx backs off with `retry.Policy`; if the ring
is full *and* shipping is failing, observations spill to the bounded disk spool
(a monitor must degrade by reporting gaps, not by lying — ARCHITECTURE §13).

### 5.6 Hub — `internal/dnsmon/hub`

```go
type HubConfig struct {
    IngestAddr  string // ":9001" TLS+mTLS
    QueryAddr   string // ":9002" TLS
    WindowKeys  int    // per (target,view) observations retained in memory; default 4096
    SegmentBytes int64 // JSONL segment rotation size; default 128 MiB
    DataDir     string
    MinResponding int
    ConvergenceWindow time.Duration
}

type Hub struct{ /* sharded window map, segment writer, classifier, alerts */ }
```

- Ingest: per-request goroutine; sharded (target,view) window map, bounded
  `WindowKeys` per shard; segment writer appends JSONL with periodic `fsync`
  (every batch), rotating at `SegmentBytes`. Segment filenames carry a UTC
  timestamp range + monotonic index.
- Query API (`/v1/observations`, `/v1/targets`, `/v1/propagation`) reads the
  window for live state and streams segments for history (`rift dns replay`
  re-materializes windows from segments offline).
- Alert evaluator (per target, every 30s): conditions in §7 of
  `DNS_SSL_TRACKER_SPEC.md`.

---

## 6. TLS Monitoring — `internal/tlsmon`

```go
package model

type Severity uint8
const (SevInfo Severity = iota; SevWarning; SevCritical)

type Finding struct { Severity Severity; Code string; Detail string } // Code from closed set

type CertInfo struct {
    Subject, Issuer string
    NotBefore, NotAfter time.Time
    SANs []string
    Serial, SigAlg string
    IsCA bool
}

type Report struct {
    Target string; Timestamp time.Time
    Chain []CertInfo // leaf first, as presented by the peer
    NegotiatedProtocol string  // e.g. "TLS 1.3"
    NegotiatedCipher string
    Findings []Finding
}
```

Probe flow (`tlsmon/probe`):

1. `tls.Dial` with `InsecureSkipVerify: true` **plus** `VerifyConnection`
   capturing the peer-presented chain — handshake succeeds so we can *report*
   rather than abort (AD-5; this inversion is the point of the component).
2. Chain build: leaf + intermediates → our pool; verify against the configured
   root set (system roots by default) with `x509.VerifyOptions{CurrentTime:
   now()}`; `VerifyHostname(target)` on the leaf.
3. Each failure becomes a `Finding` (closed code set: `EXPIRED`,
   `NOT_YET_VALID`, `HOSTNAME_MISMATCH`, `UNTRUSTED_ROOT`, `INCOMPLETE_CHAIN`,
   `SELF_SIGNED`, `WEAK_KEY`, `WEAK_SIG`, `OLD_TLS`), never an abort — the
   report is the product.
4. `OLD_TLS`: negotiated version < TLS 1.2 (configurable floor).
5. Alerts: `not_after − now < warn_days (14d)` → SevWarning; `< crit_days (3d)`
   → SevCritical; ≥ `consecutive_failures (3)` handshake/connection failures →
   SevCritical (a target that stopped answering TLS is an outage signal).
6. `verify: strict` config flips the probe to failing (not reporting) mode —
   compliance check posture.

`tlsmon` must not share TLS config code with `lb` (ARCHITECTURE §7 — the shared
helper is exactly how `InsecureSkipVerify` leaks into a data plane).

---

## 7. Media Server — `internal/media`

### 7.1 Model — `internal/media/model`

```go
type Asset struct {
    Path    string        // logical name, URL-safe
    ETag    string        // strong: quoted hex(size)-hex(mtime.UnixNano())
    Size    int64
    ModTime time.Time
}

type Index struct{ /* map[string]*Asset + RWMutex; rebuilt on SIGHUP/reload */ }
func (ix *Index) Lookup(name string) (*Asset, bool)

// RangeSpec is normalized: Start ∈ [0,size), Length ≥ 1, Start+Length ≤ size.
type RangeSpec struct { Start, Length int64 }
func ParseRange(header string, size int64) (RangeSpec, bool, RangeParseOutcome)

type RangeParseOutcome uint8
const (
    RangeNone RangeParseOutcome = iota // absent → full 200
    RangeOK                            // single range parsed
    RangeMulti                         // multipart: v1 ignores, serves 200 full (RFC 7233 §3.1 MAY), counted
    RangeInvalid                       // 416
)
```

Semantics (RFC 7233 exactly):

- `bytes=s-e` | `bytes=s-` | `bytes=-n` (suffix). Single range only in v1;
  multi-range responses are a documented milestone.
- Invalid/unsatisfiable → `416` with `Content-Range: bytes */{size}`.
- `206` with `Content-Range: bytes s-e/size`, `Accept-Ranges: bytes` on every
  response. `HEAD` mirrors headers, no body.
- `If-Range`: honored only with a strong ETag that matches → serve 206; else
  full 200. Weak validators never claimed (`ETag` is strong; `Last-Modified`
  served as a courtesy, not used for `If-Range`).
- Range on a 0-byte asset is always unsatisfiable → 416.

### 7.2 Server — `internal/media/server`

```go
type Config struct {
    Bind string
    Root string
    MaxStreams int // global concurrent; default 64
    MaxStreamsPerClient int // default 4
    ReadHeaderTimeout, IdleTimeout time.Duration
    WriteTimeout time.Duration // per whole stream; seek resets deadline
    IOMode string   // "sendfile" (default) | "buffered"
    Readahead int    // bytes, buffered mode only; default 1 MiB
    RateLimitPerClient string // e.g. "120/s"
}
```

- Admission: global + per-client semaphores; exhausted → `429` with
  `Retry-After`, counter `rift_media_admissions_rejected_total{scope}`.
- **sendfile mode (default)**: `io.Copy(w, io.NewSectionReader(f, start, len))`
  → runtime `*os.File→TCPConn` fast path (sendfile). `posix_fadvise(
  POSIX_FADV_SEQUENTIAL)` on the fd (`golang.org/x/sys/unix`, Linux only,
  feature-detected). No per-stream buffer exists in this mode — memory per
  stream is socket buffers only.
- **buffered mode**: explicit readahead buffer per stream; E5 measures both and
  the shipped default is whichever the benchmark justifies (the config comment
  carries the measured numbers, per AR-5).
- Per-stream accounting: bytes, duration, first-byte latency, send-stall
  (accumulated time `Write` blocked > 100ms), fed to metrics in §4 of
  `OBSERVABILITY_SPEC.md`.
- Index built from filesystem metadata (name, size, mtime) only — no container
  parsing (AD-9). 100k assets < 2s (NFR-8).
- Slow-client policy: `WriteTimeout` per stream; a stalled write is culled at
  deadline with `rift_media_client_stalls_total` — bounded goroutines/FDs is a
  correctness property, not a nicety.

Performance ceilings are stated **as ratios against the measured local
ceiling** (`iperf3` NIC path, `fio` disk path) from the environment card —
absolute numbers are environment-bound and WSL2-distorted (AD-11).

---

## 8. Load Generator and Player Model — `internal/bench`

### 8.1 Environment card — `internal/bench/env`

`rift bench env` emits JSON + rendered text; `rift bench report` **refuses** to
produce a report if the card is missing (a number without its environment is
not a fact). Fields: OS/kernel, CPU model/cores, governor, `GOMAXPROCS`, cgroup
v2 `cpu.max`/`memory.max`, NIC + MTU, filesystem + mount options, `ulimit -n`,
THP, mitigations, WSL2 flag + Windows build, measured `iperf3` and `fio`
ceilings for media fixtures.

### 8.2 Load generator — `internal/bench/loadgen`

```go
type Scenario struct {
    Name      string        // stable ID, e.g. "lb.http.small.1k"
    Tool      string        // "rift" | "hey" | "wrk2" | "nginx-baseline"
    Target    string
    Duration  time.Duration // steady-state measurement window
    Warmup    time.Duration
    Loop      string        // "closed" (N conns) | "open" (fixed rate)
    Conns     int           // closed loop
    RatePerSec float64      // open loop
    Payload   int           // bytes
    Method    string
    KeepAlive float64       // fraction
}
```

- Open loop: absolute-time scheduling — each request starts at its intended
  deadline regardless of completion (the coordinated-omission fix); both the
  intended and actual start times are recorded per request in raw samples.
- Histograms: `hdrhistogram-go` per worker, merged; **provisional** (UD-4, E6).
- Raw samples: NDJSON, one line per request: `intended_start, actual_start,
  latency, status, bytes, conn_id`.
- Outputs feed `bench/harness` for compare/report and the CI regression gate
  (tolerance bands per metric, fail the build outside them).

### 8.3 Player model — `internal/bench/playersim`

The only honest instrument for "rebuffer rate" — a server log cannot see a
client stall (P3 in `PRD.md`).

```go
type PlayerConfig struct {
    Bitrate float64      // bytes/s of media consumption
    InitialBuffer time.Duration // startup threshold of buffered media
    PlayoutCap int64     // playout buffer bytes cap
}
type PlayerResult struct {
    StartupLatency time.Duration // request → first byte + initial buffer filled
    FirstByte      time.Duration
    Stalls         int           // playout buffer hit empty mid-stream, not at EOF
    UnderrunTime   time.Duration // accumulated stall duration
    Bytes          int64         // media bytes received over the run
    Duration        time.Duration // wall time of the run
}
```

Model: playout buffer consumes at `Bitrate` from a fake playback clock
advancing with real time; network feed appends bytes as received. Startup
completes when the buffer holds `InitialBuffer` of media. A stall is a
playback-clock tick with an empty buffer while the asset is not exhausted.

---

## 9. Networking Utility — `internal/platform/netx`

```go
// Listener with optional SO_REUSEPORT (Linux; feature-detected, off by
// default until E2 justifies shipping it) and TCP keepalive.
func Listen(ctx context.Context, network, addr string, o ListenOptions) (net.Listener, error)
type ListenOptions struct { ReusePort bool; Backlog int; KeepAlive time.Duration }

// Guard is the single outbound dial authorization path (AD-10). Subsystems
// dial through Guard.DialContext or they do not dial — CI depguard enforces
// the import surface.
type Guard struct{ /* deny, allow []netip.Prefix */ }
type GuardOptions struct {
    Deny  []netip.Prefix // default deny-set in SECURITY_SPEC §2
    Allow []netip.Prefix // operator allow-list, overrides deny
}
func NewGuard(o GuardOptions) (*Guard, error)

// DialContext resolves (once), guards EVERY resolved address (not just the
// first — TOCTOU), then dials the literal IP with SNI preserved. A denied
// address yields errs.ClassSecurity — logged at warn level, never retried.
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error)
```

Default deny-set (v1): loopback v4/v6, private v4 + ULA v6, link-local (both
families, incl. `169.254.169.254`), multicast, unspecified. The LB's
*configured backends* are exempt (operator-owned trust); *client-supplied*
destinations never are — that distinction is the open-proxy boundary.

---

## 10. Metrics Naming Contract — `internal/platform/metrics`

Registry construction and label-set enforcement live here; the full metric
inventory is `OBSERVABILITY_SPEC.md` §3 (single source — this section defines
only the rules the package enforces in code):

- Prefix `rift_`, snake_case, units in the name (`_seconds`, `_bytes`,
  `_total`).
- Labels from a **closed enum per metric**, asserted by a unit test that walks
  the registered collectors and fails on any unbounded label (e.g., a label
  that would take `qname` verbatim).
- Gauges on hot paths are **sampled** by a ticker (e.g. active connections),
  not incremented per event.
- `rift_build_info{version,commit,goversion}` registered unconditionally — a
  benchmark result without its build labels is not reproducible.

---

## 11. Allocation and Latency Budgets (normative)

Measured by `testing.B` and enforced by assertion in the same test run (not by
aspiration). Set from Phase 0/1 baselines, tightened only with a benchstat
table per AR-5.

| Path | Budget | Asserted by |
|---|---|---|
| L4 accept → first forwarded byte | ≤ 2 allocs | `BenchmarkL4Accept` |
| L4 per KiB forwarded (splice mode) | 0 allocs | `BenchmarkL4Forward` |
| `Picker.Pick` @ 1000 backends | 0 allocs, p99 < 1 µs | `BenchmarkPick` |
| `wire.EncodeQuery` | 1 alloc | `BenchmarkEncodeQuery` |
| `wire.DecodeMessage` (typical A response) | ≤ 3 allocs | `BenchmarkDecode` |
| Hub ingest per observation | ≤ 2 allocs | `BenchmarkIngest` |
| Media per GiB served (sendfile) | ≤ 100 MiB cumulative | `BenchmarkServeFile` + `MemProfileRate` |
| L7 overhead vs direct `http.Server` | ≤ 15% at plateau | `BenchmarkL7Overhead` A/B |

---

## 12. Trace ID and Request ID — `internal/platform/logging`

Closed log field set: `ts, level, msg, svc, comp, rid, tid, dur_ms` plus
per-site fields enumerated in `OBSERVABILITY_SPEC.md` §5. `rid` = per
connection/request (16 hex, crypto/rand); `tid` = per logical operation
propagated across node→hub via `X-Rift-Tid` (tracing-deferred correlation,
AD-13). Cardinality rule: no unbounded value in any log field — DNS names
capped at 253 chars, no raw URLs from user input, no byte dumps (log fields
that accept attacker input are a disk-exhaustion primitive).

---

## 13. Shutdown Ordering (reference)

Normative for every service, budgets from §3:

1. `PhaseDraining` — set not-ready (readiness flips false), close listeners
   (stop accepting), stop schedulers (no new scheduled work).
2. Drain — in-flight conns/streams/queries continue under per-item deadlines;
   LB removed backends stop being picked but keep established conns; hub stops
   ingesting, node stops shipping.
3. `PhaseFlushing` — close upstream pools, fsync hub segments, final metrics
   scrape possible.
4. Exit: 0 clean · 4 if any drain budget fired (with counts in the log line) ·
   1 on internal error during shutdown.
```
```
