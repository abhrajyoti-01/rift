# RIFT Architecture

RIFT is a single Go module (`github.com/abhrajyoti-01/rift`) producing a single
binary (`rift`) that runs three network services on top of one shared platform
layer. This document describes the boundaries, the data flow, and the
concurrency model. It is the authoritative reference for package
responsibilities and dependency direction.

- [1. Design Principles](#1-design-principles)
- [2. System Overview](#2-system-overview)
- [3. Package Map](#3-package-map)
- [4. Data Flow](#4-data-flow)
- [5. Concurrency Model](#5-concurrency-model)
- [6. Error Model](#6-error-model)
- [7. Configuration Model](#7-configuration-model)
- [8. Dependency Direction](#8-dependency-direction)
- [9. Dependencies](#9-dependencies)
- [10. Failure Modes](#10-failure-modes)

---

## 1. Design Principles

| # | Principle | Consequence |
|---|---|---|
| P1 | **Stdlib first.** A dependency must justify the stdlib gap it fills. | The module currently needs only YAML, `x/sys`, and Prometheus client. |
| P2 | **No speculative abstraction.** A package exists because it has a real consumer or a genuinely hard algorithm. | There is no `platform/buffer`, no generic repository interface, no plugin registry. |
| P3 | **Boundaries validate; interior trusts.** | Network input, config files, and operator targets are validated. Internal calls are not re-validated. |
| P4 | **Every optimization is measured.** | No tuning code lands without a benchmark in its commit message. |
| P5 | **Deferred features are named as deferred.** | An unimplemented command reports which surface is missing; it never fabricates a plausible response. |
| P6 | **Clock and transport are injectable.** | Timing logic is testable without sleeping; tests never dial the public internet. |
| P7 | **Fail closed.** | Unknown config fields, unset tokens on routable binds, missing environment cards: each refuses rather than guesses. |

---

## 2. System Overview

RIFT is one binary with four executable surfaces and a shared platform:

```
                         ┌──────────────────────────────────────────┐
                         │              cmd/rift                    │
                         │   flags → config → wire → run            │
                         │   (composition root: no business logic)   │
                         └───┬─────────┬──────────┬──────────┬──────┘
                             │         │          │          │
                    ┌────────▼──┐ ┌────▼──────┐ ┌─▼───────┐ ┌▼────────┐
                    │ internal/ │ │ internal/ │ │internal/│ │internal/│
                    │    lb     │ │  dnsmon   │ │ tlsmon  │ │  media  │
                    │           │ │           │ │         │ │         │
                    │ l4  (TCP) │ │ node      │ │ probe   │ │ server  │
                    │ udp       │ │ hub       │ │ model   │ │ model   │
                    │ l7  (HTTP)│ │ probe     │ │         │ │ (range) │
                    │ picker    │ │ resolver  │ │         │ │         │
                    │ health    │ │ wire      │ │         │ │         │
                    │ control   │ │ model     │ │         │ │         │
                    └─────┬─────┘ └─────┬─────┘ └────┬────┘ └────┬────┘
                          │             │            │           │
   ═══════════════════════▼═════════════▼════════════▼═══════════▼═══════
                        internal/platform   (the dependency sink)
   errs · config · netx · pool · ratelimit · circuit · retry · lifecycle
   logging · metrics · health · httpx · testsupport
   ═══════════════════════════════════════════════════════════════════════
                          │
                          ▼
                 Go stdlib + 3 admitted dependencies
```

Subsystems are **peers**. They do not import one another; `cmd/rift` wires them
together. This keeps each service independently runnable, independently
testable, and independently benchmarkable.

---

## 3. Package Map

```
cmd/rift/                     composition root: dispatch and wiring

internal/platform/            shared substrate
  errs/          error Class taxonomy, wrapping, exit-code mapping
  config/        YAML load, strict validation, redaction, diff, env overrides
  netx/          listeners, the SSRF dial guard, UDP bind
  pool/          bounded executor (work units that are not sockets)
  ratelimit/     sharded token buckets with bounded key cardinality
  circuit/       closed / open / half-open breaker
  retry/         attempt policy, jittered backoff, class-based eligibility
  lifecycle/     phases, ordered start/stop, drain budgets (exit 4)
  logging/       structured logging and request/trace IDs
  metrics/       registry, bucket sets, label-set enforcement
  health/        liveness / readiness state machine
  httpx/         HTTP server factory with the mandatory timeout set
  testsupport/   real-socket fakes and fault injectors

internal/lb/
  model/         Backend, Pool, ListenerSpec, Snapshot
  picker/        round-robin, smooth weighted RR, least-connections
  health/        TCP and HTTP active checks with rise/fall hysteresis
  l4/            TCP forwarding (zero-copy path + pooled copy fallback)
  udp/           UDP session table with per-session backend affinity
  l7/            HTTP reverse proxy + the closed retry table
  control/       snapshot store, reload, admin API

internal/dnsmon/
  wire/          RFC 1035 codec (own implementation; TTLs, rcode, TC, RD=0)
  resolver/      per-resolver query engine (UDP → TCP fallback, breaker)
  probe/         recursive-view probing, canonicalization, fingerprints
  model/         Observation, Target, and the propagation classifier
  node/          scheduler, bounded ring, batch shipper, offline spool
  hub/           ingest, bounded window, JSONL segments, query API

internal/tlsmon/
  model/         Report, Finding, the closed finding-code set
  probe/         handshake capture + manual chain/hostname verification

internal/media/
  model/         Asset, Index, RFC 7233 range parsing, strong ETags
  server/        range handler, admission limits, index builder

internal/bench/
  env/           environment card (the reproducibility record)
  loadgen/       open/closed-loop runner with raw NDJSON samples
  playersim/     playback-clock client model (startup, stalls, underrun)
  harness/       run / compare / report

experiments/     toolchain probes (syscall and scaling measurements)
bench/           scenarios/, baselines/, results/, fixtures/
deploy/          docker/, compose/, grafana/, prometheus/, nginx/
scripts/         local development helpers
```

Each subsystem package exists because it has a distinct concurrency boundary or
a distinct test surface — not as a file-organization device. `picker` is
separate from `l4`/`l7` because both consume it and it has its own benchmark
loop. `probe` is separate from `resolver` because scheduling policy and
transport have different test matrices.

---

## 4. Data Flow

### 4.1 Load balancer (L4 TCP)

```
 client ──TCP──► netx.Listen ──accept──► one goroutine per connection
                     │                        │
                     │                        ├─ admission: MaxConns (refuse, never queue)
                     │                        ├─ SetNoDelay + TCP keepalive
                     │                        ├─ picker.Pick() ──► *Backend
                     │                        ├─ dial with connect-phase repicks
                     │                        ├─ bidirectional copy
                     │                        └─ half-close propagation
                     │
        ┌────────────┴─────────────┐
        │ health.Checker goroutines│  writes Backend.Health (atomic)
        │ control.Reload (SIGHUP)  │  swaps atomic.Pointer[Snapshot]
        └──────────────────────────┘
```

The data plane reads configuration through a single
`atomic.Pointer[Snapshot]`. **No lock is shared between the control plane and
the data plane**, so a reload cannot stall a request and a removed backend
keeps serving its established connections.

### 4.2 Load balancer (L7 HTTP)

```
 client ──HTTP──► http.Server ──► Handler
                                    │
                                    ├─ reject oversized Content-Length (413)
                                    ├─ reject CL+TE ambiguity (400)
                                    ├─ append X-Forwarded-For / Forwarded
                                    └─ httputil.ReverseProxy
                                            │  Director: pick backend (pool only)
                                            │  Transport: owned, keep-alive pool
                                            │  BufferPool: bounded sync.Pool
                                            └─ ModifyResponse / ErrorHandler
```

Request framing stays stdlib-owned: HTTP/1.1 chunking, `Expect: 100-continue`,
trailers and 1xx relay are exactly where request-smuggling bugs live, so RIFT
does not reimplement them.

### 4.3 DNS monitoring

```
 scheduler ──target──► resolver.Engine (bounded concurrency, breaker)
                            │   wire query: UDP → (TC=1) → TCP
                            ▼
                       Observation ──► node ring (fixed capacity, drops counted)
                            │                │
                            │                ▼
                            │           batch shipper ──mTLS──► hub /v1/ingest
                            │                                        │
                            │                    ┌───────────────────┼──────────────────┐
                            │                    ▼                   ▼                  ▼
                            │              bounded window      JSONL segments     classifier
                            │              (per-key cap)       (append-only)      (propagation)
                            ▼                                                        
                      offline spool (bounded)                              /v1/propagation
```

The node is **loss-tolerant**: an unshipped observation is re-schedulable, so it
may queue, batch, and shed — with every dropped observation counted and every
drop attributed to a reason. The LB data plane is **lossless**: bytes must not
be dropped, so it refuses new connections instead of queueing them. These two
shapes are deliberately different, which is why there is no shared "pipeline"
abstraction.

### 4.4 Media serving

```
 client ──GET + Range──► http.Server
                            │
                            ├─ sanitize asset name (refuse, never normalize)
                            ├─ index lookup (atomic pointer)
                            ├─ admission (global + per-client semaphores)
                            ├─ open file, then stat the FD (closes TOCTOU)
                            ├─ parse Range → 206 | 416 | 200
                            ├─ If-Range: strong ETag only
                            └─ copy → io.Copy fast path | bounded readahead
```

Memory per stream is one readahead buffer (buffered mode) or socket buffers
only (sendfile mode) — bounded by configuration, independent of file size.

---

## 5. Concurrency Model

**Governing rule:** goroutines are paid for by connections, not pooled, except
where the work unit is not a socket. A worker pool in front of a blocking
`Read` converts one goroutine into two plus a channel handoff, and strictly
loses latency.

| Boundary | Owner | Population | Synchronization |
|---|---|---|---|
| L4 accept | `lb/l4` | 1 per listener | none |
| L4 connection | `lb/l4` | 1 per client connection | socket deadlines only |
| L7 request | `net/http` | 1 per connection | stdlib-owned |
| Health checks | `lb/health` | 1 ticker per pool | atomic backend health |
| UDP sessions | `lb/udp` | 1 read loop, 1 sweeper, 1 reply goroutine per session | `sync.Map` session table |
| Control reload | `lb/control` | serialized by a mutex | `atomic.Pointer` snapshot |
| DNS query | `dnsmon/resolver` | bounded per resolver | per-resolver atomics |
| Node shipping | `dnsmon/node` | 1 prober loop + 1 shipper loop | ring mutex (cold path) |
| Hub ingest | `dnsmon/hub` | 1 per HTTP request | sharded window locks |
| Media stream | `media/server` | 1 per stream | admission semaphores |

**Backpressure is always visible.** There are three mechanisms, and none of them
is a hidden queue:

1. **Admission refusal** — over `max_conns`/`max_streams`, the connection is
   closed or the request answered `429`, with a counter incremented.
2. **Deadlines** — per-operation read/write deadlines cull stalled peers.
3. **Drop with a reason** — the DNS ring and UDP session table drop oldest or
   refuse new, each with a distinct counter (`ring_full`, `table_full`,
   `idle_sweep`). A monitor that hides its own loss is worse than one that
   reports gaps.

---

## 6. Error Model

One taxonomy, in `platform/errs`. `Class` drives exactly three things — log
level, metric label, and retry eligibility — and nothing else.

| Class | Meaning | Retryable | Exit code |
|---|---|---|---|
| `ClassUnknown` | unclassified; must be reported | no | 1 |
| `ClassNetwork` | transport failure | yes | 1 |
| `ClassTimeout` | deadline exceeded (metric distinct from network) | yes | 1 |
| `ClassPeerClosed` | normal EOF/RST; not a fault | no | 1 |
| `ClassConfig` | startup config fault | no | 2 |
| `ClassSecurity` | deny-set hit, oversize, smuggling | never | 1 |
| `ClassResource` | limit reached; shed load | no | 3 (or 4 for drain) |

`ClassOf` walks the wrapped chain and is the only classifier in the tree;
error-string matching is not used anywhere for control flow.

---

## 7. Configuration Model

One YAML document per deployment, validated before any socket binds.

```yaml
observability:  log level/format, admin plane, shutdown budget
lb:             listeners[], pools[], pickers, health, limits, retry, rate limits
dns:            role (node|hub), resolvers[], targets[], ring/ship/spool bounds
tls:            targets[], probe schedule, expiry thresholds, root store
media:          bind, root, limits, io mode, readahead, timeouts
bench:          scenario dir, results dir, timeout
```

Rules, all enforced in code and covered by tests:

- **Unknown fields are a hard error.** A typo must not be silently ignored.
- **Every violation is reported with its field path**, not just the first.
- **Validation is pure** and never touches the network.
- **Secrets are file paths or env references, never inline.** Redaction is
  applied before any schema is echoed (diff, admin API, logs), and a diff is
  computed over redacted copies.
- **Routable binds fail closed.** A routable admin bind requires the explicit
  `allow_remote` opt-in; plaintext hub ingest is loopback-only.

---

## 8. Dependency Direction

```
cmd/rift  ──────────────────────────────►  every subsystem
internal/lb/*        ──►  internal/platform/*
internal/dnsmon/*    ──►  internal/platform/*
internal/tlsmon/*    ──►  internal/platform/*
internal/media/*     ──►  internal/platform/*
internal/bench/*    ──►  internal/platform/*
internal/platform/*  ──►  Go stdlib only   (it imports no subsystem)
```

- `platform` is the dependency sink: it must never import a subsystem.
- Subsystems never import each other.
- The only cross-subsystem imports are `dnsmon/hub` reading `dnsmon/node`'s
  wire type (deliberate: one shared JSON contract) and `dnsmon/probe` reading
  `dnsmon/resolver` and `dnsmon/wire`.

---

## 9. Dependencies

| Module | Why |
|---|---|
| `gopkg.in/yaml.v3` | Configuration must be human-editable with comments; stdlib has no YAML parser. |
| `golang.org/x/sys` | `SO_REUSEPORT`, socket options, `posix_fadvise` on Linux. |
| `github.com/prometheus/client_golang` | Metric exposition correctness and quantile estimation are not things to hand-roll. |

Everything else — the DNS wire codec, the TLS probe, the load generator, the
player model — is stdlib plus first-party code. Notably, RIFT does **not** use
`miekg/dns`: the wire engine is one of the things this project exists to
demonstrate, and it is fuzzed accordingly.

---

## 10. Failure Modes

| Failure | Detection | Behaviour |
|---|---|---|
| All backends down | health checks + `ErrNoUpstream` | `502`; readiness false; process stays live and observable |
| Backend RST mid-stream | read/write error | connection closed, counters decremented, classified `peer_closed` |
| Accept under FD pressure | `max_conns` admission gate | refuse early with a counter, rather than failing mid-copy |
| Config typo on reload | validate-then-swap | swap refused, previous snapshot retained, failure counted |
| Shutdown drain over budget | per-phase deadline | exit code 4 with in-flight counts |
| Slow client | write deadline via `http.ResponseController` | stream culled at the deadline, stall counted |
| Hub unreachable | ship failure | bounded spool; if spool is full, drop with a counter |
| Malformed DNS response | wire parser caps and pointer rules | discarded as `ClassSecurity`, never crashes |
| TLS target broken | probe reports findings | a bad chain is a *successfully monitored* target, not an abort |
| Disk below media target | throughput ratio | admission shed at high-water mark; in-flight streams continue |

---

## See also

- [`SECURITY.md`](SECURITY.md) — threat model, trust boundaries, hardening status
- [`API.md`](API.md) — HTTP contracts
- [`CLI.md`](CLI.md) — command surface, exit codes
- [`OBSERVABILITY.md`](OBSERVABILITY.md) — metrics and logging
- [`TESTING.md`](TESTING.md) — test strategy
- [`PERFORMANCE.md`](PERFORMANCE.md) — benchmark methodology
- [`PRD.md`](PRD.md) — requirements and scope
- [`ROADMAP.md`](ROADMAP.md) — phases and milestones
