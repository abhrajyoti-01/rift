# ARCHITECTURE.md — RIFT High-Performance Network Operations Platform

Status: **Revision A — normative**. Toolchain: Go 1.25.5 (`go.mod` pins `go 1.25.5`).
This document is one of two oracles. `TECHNICAL_SPEC.md` is the other and is
authoritative for type and identifier names; this document is authoritative for
package boundaries, dependency direction, and data flow. If code and these two
files disagree, the file is the defect or the code is — resolve in favour of the
file, then amend the file deliberately in its own commit.

---

## 1. Purpose and Scope

RIFT is one Go module producing one binary (`rift`) that runs three network
services sharing one platform layer:

| Component | Networking problem it exercises | Why it is in this platform |
|---|---|---|
| **Load balancer** (`lb`) | syscall count per byte, allocation per routing decision, accept-path scaling | Makes the cost of each proxy mechanism individually measurable |
| **DNS + TLS tracker** (`dnsmon`, `tlsmon`) | latency distributions, untrusted-parser hardening, many-concurrent-outbound control | Demonstrates a real wire-protocol implementation under resource caps |
| **Media server** (`media`) | sustained bandwidth, backpressure, bounded memory at arbitrary file size | Demonstrates that throughput is bounded by disk/NIC, not by the application |

These are not three unrelated products bolted together. They were selected
because each stresses a different failure mode of Go network I/O, and because a
shared platform layer with **three real consumers** is far less likely to calcify
into a wrong abstraction than one with a single consumer.

**Primary engineering goal:** understand and demonstrate high-performance
networking. Not: build a web application, not: accumulate features.

---

## 2. Architectural Rules

These are mechanically enforced (CI + `.golangci.yml` `depguard`), not stylistic
preferences. A violation fails the build.

- **AR-1 — One-way dependency.** `platform` imports no subsystem. Subsystems
  never import each other. `cmd/rift` is the only composition root. Nothing else
  may wire components together.
- **AR-2 — Everything internal.** All code lives under `internal/`. RIFT is an
  application; publishing unstable Go APIs creates compatibility obligations that
  conflict with benchmark-driven restructuring. The external contracts are the
  **wire protocols** (DNS, TLS, HTTP, node→hub ingestion), versioned in JSON as
  `/v1`, not Go signatures.
- **AR-3 — Stdlib first, dependency by evidence.** A dependency is admitted only
  with a written justification naming the stdlib gap. Current admitted set is
  §8; anything else requires a spec amendment.
- **AR-4 — No speculative abstraction.** A package exists because it has ≥2
  consumers or one consumer with a genuinely hard algorithm. §7 lists the
  abstractions deliberately **not** built.
- **AR-5 — Optimization requires a measurement.** No tuning code enters the tree
  without a before/after `benchstat` table in its commit message and a result file
  in `bench/results/`. See `PERFORMANCE_SPEC.md` §1.
- **AR-6 — Deferred features are named as deferred.** A future milestone is a
  milestone, never a stub that returns plausible data. Grep-for-`TODO` is a CI
  check on `internal/**`.
- **AR-7 — Boundary validation only.** Network input, config files, and
  operator-supplied targets are validated. Internal calls do not re-validate.
- **AR-8 — Deterministic under test.** Every time-dependent component takes a
  clock/ticker by injection. No package may read wall time implicitly on a path
  that a test exercises.

---

## 3. System Layout

```
                        ┌──────────────────────────────────────┐
                        │             cmd/rift                 │
                        │   flags → config → build → run      │
                        │   (composition root; no logic)       │
                        └───┬─────────┬─────────┬─────────┬────┘
                            │         │         │         │
                   ┌────────▼───┐ ┌───▼──────┐ ┌▼────────┐ ┌▼─────────┐
                   │ internal/lb│ │internal/ │ │internal/│ │internal/ │
                   │ l4 · udp   │ │ dnsmon   │ │ media   │ │  bench   │
                   │ l7 · picker│ │ +tlsmon  │ │ server  │ │ loadgen  │
                   │ health     │ │ node+hub │ │ index   │ │ playersim│
                   │ control    │ │ wire     │ │         │ │ env      │
                   └────────┬───┘ └────┬─────┘ └───┬─────┘ └────┬─────┘
                            │          │           │            │
   ═════════════════════════▼══════════▼═══════════▼════════════▼═════════
                       internal/platform   (dependency sink)
   errs · config · logging · lifecycle · health · metrics · httpx · netx
   pool · ratelimit · circuit · retry · testsupport
   ═══════════════════════════════════════════════════════════════════════
                            │
                            ▼
              Go stdlib + the four admitted dependencies (§8)
```

Subsystems are **peers**. The DNS node does not call the TLS prober through Go;
both are separate services sharing `platform`. Cross-component correlation happens
through the hub's storage and the admin API, never through an import edge. This is
what keeps `dnsmon` and `tlsmon` independently deployable and independently
benchmarkable.

---

## 4. Package Map (normative)

Directory layout is fixed. `TECHNICAL_SPEC.md` fixes the identifiers inside.

```
cmd/rift/                     composition root: subcommand dispatch, wiring only

internal/platform/
  errs/          error Class taxonomy, wrapping, exit-code mapping
  config/        YAML load, schema validation, redaction, diff, file watch
  logging/       slog setup, level parsing, request/trace ID context plumbing
  lifecycle/     ordered phases, errgroup supervision, signals, drain, deadlines
  health/        liveness/readiness state machine, check fan-in
  metrics/       registry, bucket sets, process collectors, build info, Handler
  httpx/         server factory with timeout sets, middleware, TLS profiles
  netx/          listeners (SO_REUSEPORT), keepalive, deadline helpers,
                 Guard (SSRF dial authorization), buffered-conn helpers
  pool/          bounded worker pool sized by work, not goroutine count
  ratelimit/     token bucket, sharded per-key map with bounded cardinality
  circuit/       closed / open / half-open breaker
  retry/         attempt policy, jittered backoff, idempotency classification
  testsupport/   real-socket fakes, fault injectors, goleak TestMain, fixtures

internal/lb/
  model/         Backend, Pool, ListenerSpec, Snapshot, Status
  picker/        Picker interface; round-robin, smooth weighted, least-connections
  health/        Checker interface; TCP-connect and HTTP-get active checks
  l4/            TCP forwarding proxy (splice path + pooled-copy fallback)
  udp/           UDP session table, affinity, idle sweep, accounting
  l7/            HTTP reverse proxy, keep-alive reuse, XFF/RFC 7239
  control/       admin API, config reload, atomic snapshot swap

internal/dnsmon/
  model/         Target, View, Observation, PropagationState, classifier
  wire/          RFC 1035 message encode/decode, compression, TC, size caps
  resolver/      per-resolver query engine: concurrency cap, timeouts, stats
  probe/         recursive-view and authoritative-view (RD=0) probing
  node/          scheduler, bounded ring, batch shipper, offline spool
  hub/           ingest, memory window, JSONL segments, query API, alert eval

internal/tlsmon/
  model/         Report, Finding, Severity, ProbeConfig
  probe/         handshake capture, manual chain build, hostname verification

internal/media/
  model/         Asset, Index, RangeSpec, Validators
  server/        range handler, limits, per-stream accounting, index builder

internal/bench/
  env/           host probe → environment card (JSON + rendered text)
  loadgen/       scenario runner, closed+open loop, HDR summary, raw samples
  playersim/     playback-clock client model → startup + underrun measurement
  harness/       run / compare / report subcommands, CI regression gate

experiments/     E1..E7 — standalone real programs, Phase 0 toolchain probes
docs/            images and appendices only; the 15 specs live at repo root
deploy/          docker/, compose/, grafana/, systemd/, nginx/
bench/           scenarios/, baselines/, results/, fixtures/
```

**Package count rationale:** 33 non-test packages for ~3 services plus a platform
layer. Each subsystem package exists because it has a distinct concurrency
boundary or a distinct test surface, not as a file-organization device.
`picker` is separate from `l4`/`l7` because both consume it and its benchmarks
have their own loop. Merging `probe` into `resolver` would mix scheduling policy
with transport and double the test matrix of each.

---

## 5. Component Boundaries and Data Flow

### 5.1 Load balancer

```
 client ──TCP/TLS──► netx.Listener ──accept──► conn goroutine (l4 | l7)
                          │                        │
                          │                        ├─ admission: maxConns, ratelimit
                          │                        ├─ l4: dst.ReadFrom(src)  [splice]
                          │                        ├─ l7: ReverseProxy → Transport
                          │                        │        └─ picker.Pick() → *Backend
                          │                        │             (atomic.Pointer snapshot)
                          │                        └─ metrics + trace log
                          │
      ┌───────────────────┴───────────────────┐
      │ health.Checker (own goroutines)       │  writes Backend.Health atomically
      │ control.reload → validate → swap      │  atomic.Pointer[Snapshot] store
      └───────────────────────────────────────┘
```

Data plane reads the snapshot; control plane writes a new one. **No lock is
shared between them.** Consequences: reload cannot stall a request, and a
removed backend keeps serving its established connections until drain completes.

### 5.2 DNS + TLS monitoring

```
 operator config (targets, resolvers, node identity)
        │
        ▼
 dnsmon/node ──schedule──► resolver.Engine (bounded workers, per-resolver cap)
        │                        │  wire.Query: UDP → (TC) → TCP, ctx deadline
        │                        ▼
        │                   Observation (view-classified, TTL, latency, rcode)
        │                        ▼
        ├── ring buffer (fixed capacity; drop-oldest counted, never silent)
        │        ▼
        │   batch shipper ──HTTPS+mTLS──► dnsmon/hub /v1/ingest
        │                                      │
        │                        ┌─────────────┼──────────────┐
        │                        ▼             ▼              ▼
        │                  memory window  JSONL segments  alert evaluator
        │                  (bounded N)   (append-only)   (expiry/divergence)
        │                        │
        ▼                        ▼
 tlsmon/probe ──Report──► hub ──► query API + /metrics
```

The node is **loss-tolerant and throughput-bound**: an unshipped observation is
re-schedulable, so it may queue, batch, and shed with a visible counter. The LB
data plane is **lossless and latency-bound**, so it must not queue. These two
shapes are why there is no shared "pipeline" abstraction (§7).

### 5.3 Media

```
 client ──HTTP GET + Range──► http.Server (per-conn goroutine)
                                  │
                                  ├─ parse/validate RangeSpec → 206 | 416 | 400
                                  ├─ admit: maxStreams (global) + per-client
                                  ├─ index.Lookup → Asset (path, size, mtime, etag)
                                  ├─ Seek(offset) → Copy(readahead buffer,
                                  │     io.Copy → TCPConn.ReadFrom → splice)
                                  └─ per-stream accounting (bytes, stalls, duration)
```

Memory per stream = one readahead buffer + socket buffers. Bounded by config,
independent of file size. No whole-file reads, no mmap in v1.

---

## 6. Concurrency Boundaries

Each row is one goroutine *population* with one owner and one lifecycle. The full
table, lock inventory, and shutdown ordering live in `TECHNICAL_SPEC.md` §6–§8.

| Boundary | Owner | Population | Synchronization |
|---|---|---|---|
| L4 accept | `lb/l4` | 1 per listener | none |
| L4 conn | `lb/l4` | 1 per client conn | socket deadlines only |
| L7 request | `net/http` | 1 per conn | stdlib owns it |
| Active health | `lb/health` | 1 ticker + bounded workers | atomic backend health |
| UDP sessions | `lb/udp` | 1 read loop + 1 sweeper | sharded session map |
| Control reload | `lb/control` | 1 | atomic snapshot pointer |
| DNS query | `dnsmon/resolver` | bounded worker pool | per-resolver stats atomic |
| Node ship | `dnsmon/node` | 1 | ring mutex (cold path) |
| Hub ingest | `dnsmon/hub` | 1 per request | sharded window map |
| Media stream | `media/server` | 1 per stream | admission semaphore |
| Bench loadgen | `bench/loadgen` | bounded per scenario | HDR histograms per worker |

**Governing rule:** goroutines are paid for by connections, not pooled, except
where the work unit is *not* a socket. A worker pool in front of a blocking
`Read` converts one goroutine into two plus a channel handoff, and strictly loses
latency. Bounded pools are used only for timer-driven and outbound-budget-driven
work (health checks, DNS queries).

---

## 7. Abstractions Deliberately Not Built

Recorded because the absence of each is a decision, and a future reader will
otherwise "fix" it.

| Not built | Why | Revisit when |
|---|---|---|
| `platform/buffer` | Buffer policy is a property of the copy loop. `lb` pools `[]byte` chunks; `media` holds one readahead per stream; `dnsmon/wire` owns one message buffer. A shared pool type would be genericity with no consumer. | A profile shows GC pressure common to ≥2 paths. |
| `platform/dns` | `dnsmon` is the only consumer. Promoting it implies a reuse story that does not exist. | A second component needs wire DNS. |
| `platform/tls` | **Security-critical.** `lb` terminates TLS and must verify; `tlsmon` must *not* verify so it can report chain failures. A shared config helper is the exact mechanism by which `InsecureSkipVerify` leaks into a data plane. | Never. Split by design. |
| `platform/pipeline` | §5.2: the LB path must not queue; the DNS path must queue. One abstraction cannot serve both without lying. | Not planned. |
| Generic `Repository`/`Store` interfaces | Single implementation each. Interfaces at exactly one implementer hide the call and invite the wrong seams. The seams RIFT *does* define (`Picker`, `Checker`, `Resolver`) exist because they have ≥2 real implementations. | 2nd implementation lands. |
| Plugin/registry mechanism | Three subsystems is not a plugin ecosystem. | Not planned. |
| ORM / migration framework | Storage is a bounded memory window plus append-only JSONL segments. There is no schema to migrate. | A SQL backend becomes a real milestone. |

---

## 8. Dependencies

Admitted set, each with the stdlib gap it fills. **Admission is phase-gated
(AR-3)**: a dependency enters `go.mod` in the same commit as the first code
that imports it. The Phase-0 skeleton is pure stdlib, and `go.mod` always
reflects exactly what the tree imports — version pins below are the
pre-agreed admission versions, applied when their phase lands.

| Module | Version | Justification | Rejected alternative |
|---|---|---|---|
| `gopkg.in/yaml.v3` | v3.0.1 | Config must be human-editable with comments for a 40-backend pool; stdlib has no YAML. | JSON (uneditable at that size); TOML (also needs a dep). |
| `golang.org/x/sys` | v0.47.0 | `unix.SetsockoptInt`, `SO_REUSEPORT`, `splice` verification, `posix_fadvise`, cgroup v2 limits. Needs Go ≥1.25.0 — verified compatible with the pinned 1.25.5. | `syscall` package (platform-specific, no `unix` helpers). |
| `github.com/prometheus/client_golang` | v1.23.0 | Hand-rolled histogram quantile estimation and exposition-format correctness must not be wrong; every consumer already speaks it. | Own text exposition (real correctness risk). |
| `github.com/HdrHistogram/hdrhistogram-go` | v1.1.2 | Coordinated-omission-correct percentiles in the load generator. **Provisional** — experiment E6 compares it against a fixed-bucket in-house histogram; if its own overhead is material we replace it (AR-5). | In-house bucketed histogram (E6 may choose this). |
| `go.uber.org/goleak` | v1.3.0 | Goroutine-leak assertions in `TestMain`. NFR-6 is not satisfiable by inspection at this concurrency. | Manual `runtime.NumGoroutine` deltas (flaky). |

**Explicitly rejected:** OpenTelemetry SDK (traces deferred by decision; its
exporter overhead would have to be subtracted from every number this project
reports); `miekg/dns` (own wire engine is the point of the component and is
fuzzed — see `TECHNICAL_SPEC.md` §4.3); `modernc.org/sqlite` (transpiled-from-C,
dwarfs first-party code); `pgx` (forces a DB into every integration test);
`gRPC`/protobuf for node→hub (§9.4); `cgo` anywhere (breaks cross-compilation and
pure-Go `net` resolver behaviour); `automaxprocs` (30 lines against `x/sys` +
procfs, and we want the logic visible in a profiling project).

---

## 9. Architectural Decisions Register

Each entry is a decision, its alternative, and what would reverse it. Trade-offs
are stated because a decision without a cost is not a decision.

| ID | Decision | Alternative rejected | Trade-off accepted | Reversal condition |
|---|---|---|---|---|
| **AD-1** | Single binary, subcommand-selected services. | One binary per service. | Binary size and a slightly larger attack surface than per-service minimal builds. | If per-service image size becomes a real deployment constraint, split `cmd/` while keeping one module. |
| **AD-2** | All code under `internal/`. | Exported public Go API. | Third parties cannot import subsystems. | Never for v1; revisit only if external consumers appear. |
| **AD-3** | `httputil.ReverseProxy` + owned `Transport`/`BufferPool`/`ModifyResponse`, not a hand-rolled L7. | Custom HTTP/1.1 forwarding. | Less "complete control" over the request path. | Smuggling-class risk in HTTP framing is not worth the control; measured overhead (E7) is the only reason to revisit. |
| **AD-4** | Own DNS wire engine in `dnsmon/wire`. | `net.Resolver` or `miekg/dns`. | Must fuzz and validate our own parser. | `net.Resolver` cannot return TTL (`net.NS` is `{Host string}`, `net.MX` is `{Host,Prefer}` — verified in the pinned toolchain), cannot do `RD=0`, and collapses rcode. Non-negotiable. |
| **AD-5** | `tlsmon` uses `InsecureSkipVerify` then manual `x509.Verify` + `VerifyHostname`. | Rely on `VerifyPeerCertificate` or default verification. | Two code paths to keep correct. | Default verification collapses chain problems into one error, hiding the exact failures being monitored. |
| **AD-6** | Immutable `Snapshot` behind `atomic.Pointer`; zero data-plane locks on reload. | `RWMutex`-guarded config. | Reload cannot invalidate an in-flight decision (by design). | Never. |
| **AD-7** | Bounded memory window + append-only JSONL segments for hub storage. | Embedded SQLite / Postgres. | No ad-hoc SQL over history. | A real analytical need appears, or segment size defeats the replay tool. |
| **AD-8** | Node→hub = HTTP/1.1 + JSON lines over mTLS. | gRPC + protobuf. | No streaming multiplexing, larger payload than binary. | Binary framing beats JSON by >5% end-to-end at the target batch rate. |
| **AD-9** | v1 media = progressive HTTP + single byte-range only. | HLS/DASH packaging, transcode. | Browsers/players needing adaptive bitrate are out of scope. | A milestone; needs FFmpeg as an external process, which we deliberately keep out of the hot path. |
| **AD-10** | SSRF guard enforced in `platform/netx` at the dialer, not per call site. | Validate URLs at each component. | One more indirection on every outbound dial. | Never — per-site validation is the pattern that produces TOCTOU rebinding bugs. |
| **AD-11** | Performance claims are environment-carded; WSL2 is a first-class documented constraint. | Publish bare numbers. | Absolute numbers are less flattering and less portable. | A bare-metal Linux host replaces the reference environment; ratios stay valid. |
| **AD-12** | Kernel-bypass/`io_uring`/XDP/GSO/`MSG_ZEROCOPY` excluded from v1. | Adopt early for headline numbers. | Leaves theoretical throughput on the table. | Requires a real Linux host first; WSL2's kernel would make the measurement non-generalizable. |
| **AD-13** | Traces deferred; `rid`/`tid` propagated through logs, metric labels only from a closed set. | OpenTelemetry now. | No cross-service span waterfall yet. | Node→hub protocol ships and spans must cross a real network hop. |
| **AD-14** | UDP load balancing is session-affine, per-session socket, drop-with-counter as sole backpressure. | Per-packet pick. | Cannot balance mid-flow; no congestion signal. | Never for v1 — this is what UDP actually permits. |
| **AD-15** | Retry table is closed and method-keyed (§4.6); unknown methods never retry; `retryablehttp`-style blanket retry rejected. | Retry on any connection error. | Fewer retries, so slightly lower availability for transient faults. | Never. Retrying a POST whose bytes reached the backend is a data-corruption bug. |

---

## 10. Configuration Model

One root document per deployment, service sections inside it. Format: YAML
(`gopkg.in/yaml.v3`), validated programmatically before the data plane binds a
single socket; `rift config validate` runs the identical path so CI checks the
file an operator wrote.

```
rift.yaml
  shared/observability   logging level/format, admin bind, metrics, TLS profile
  lb:                    listeners[], pools[], picker, health checks, limits,
                         reload policy, ratelimits, timeouts
  dns:                   role: node|hub, node identity+location, resolvers[],
                         targets[], schedule, ring caps, hub endpoint + client cert
  tls:                   targets[], probe schedule, alert thresholds, root store
  media:                 root, limits, readahead, timeouts, index policy
  env override           RIFT_<SECTION>_<KEY> for scalars only, documented per key
```

Rules: unknown fields are a **hard error** (typo safety beats forward-compat for a
single-operator config); secrets are file paths or env references, never inline
values; `config.Diff` produces a stable, field-path-addressed diff that is both
logged on reload and printed by `rift config diff` before a swap; validation is
pure and never touches the network.

---

## 11. Error and Exit-Code Model

Single taxonomy (`platform/errs`), fully specified in `TECHNICAL_SPEC.md` §3.
`Class` drives exactly three things — log level, metric label, retry
eligibility — and nothing else, so it stays cheap on the hot path and
label-cardinality-safe.

Exit codes: `0` clean · `1` runtime fault · `2` config invalid (startup only) ·
`3` bind/resource failure · `4` drain deadline exceeded · `124` bench timeout.
Chosen so a supervisor can distinguish "fix the file" (`2`) from "restart me"
(`1`) without parsing logs.

---

## 12. Deployment Architecture

Reference target is a Linux server; development host is Windows with WSL2 Ubuntu.

```
   ┌───────────────────────── one Linux host / container ─────────────────────────┐
   │  rift lb        :80, :443 (data plane)      admin 127.0.0.1:9000            │
   │  rift dns node  outbound UDP/TCP 53         ──┐                             │
   │  rift tls probe outbound TCP 443            ──┤ mTLS                        │
   │  rift media     :8080 (LAN)                 admin :9100                      │
   │  rift dns hub   :9001 ingest + :9002 query  ─┘  /metrics, /healthz, /readyz │
   │  prometheus scrape → rift metrics           grafana → deploy/grafana/*.json │
   └──────────────────────────────────────────────────────────────────────────────┘
        backends: deploy/compose/nginx.yml + echo servers (real, not mocked)
```

- Data-plane and admin-plane listeners are **always separate sockets**. The admin
  listener defaults to loopback and refuses to bind a routable address without an
  explicit `allow_remote: true` plus a token — refusing to start is a better
  failure mode than an accidentally open `/config/reload`.
- Containerization is `deploy/docker/*.dockerfile` (multi-stage, distroless or
  `alpine`, static build, non-root UID) plus `compose` for the reference
  topology with Nginx as a real baseline. Docker is **not installed on the current
  development host** (verified); provisioning inside WSL2 is a Phase 0 task, and
  until it exists the compose topology is untested — recorded in `ROADMAP.md`
  rather than claimed.
- No kernel features are assumed. `SO_REUSEPORT`, TCP buffer sizing, and
  `posix_fadvise` are optional, feature-detected, each with a working fallback.
- Single-writer-per-port: two `rift lb` instances on one host would fight over the
  listener, which is what `SO_REUSEPORT` exists for.

---

## 13. Failure Modes (system level)

Full matrix with detection and blast radius in `RUNBOOK`-style detail inside
`SECURITY_SPEC.md` §7 and `TESTING_SPEC.md` §8. Summary by principle:

| Failure | Detection | Behaviour | Why this choice |
|---|---|---|---|
| All backends down | health + `ErrNoUpstream` | `502` + `readyz` false, process stays live and observable | Keeping the admin plane up is how an operator reads the cause |
| Hub unreachable from node | ship failures | Ring keeps buffering, drop-oldest counter rises, offline spool grows | A monitor must degrade by reporting gaps, not by lying |
| Resolver timing out | per-query ctx | Timeout counter, distinct from network error, circuit-break that resolver | Conflating the two hides amplification attacks |
| Disk below media target | throughput ratio vs measured ceiling | `429`/shed new admits at high water mark, existing streams preferred | Protects in-flight playback over admitting more |
| Config invalid on reload | validation before swap | Swap refused, previous snapshot retained, `reload_total{outcome="fail"}` | A typo must not take down a data plane |
| Drain deadline exceeded | lifecycle step budgets | Exit `4`, "forcibly terminated" log, in-flight conns RST | Visible failure beats a hung process forever |
| Slow client | write deadline + send-stall metric | Connection culled at deadline, counter incremented | Bounded goroutines/FDs is a correctness property |
| FD exhaustion | `maxConns` < rlimit with margin, sampler | Admission refused + counter, not `accept` failure | Refusing earlier is strictly better than EMFILE mid-copy |
| Session-table flood (UDP) | `max_sessions` gate | Drop + counter with distinct reason | Unbounded session table = one-`sendto`-loop memory bomb |

---

## 14. Observability Integration

Defined in `OBSERVABILITY_SPEC.md`. Architecturally load-bearing points:

- Metrics are exported **only** from the admin listener; subsystems never expose a
  data-plane `/metrics`.
- Gauges are sampled where an increment-every-event gauge on the hot path would be
  a contention point (active-connection counts).
- Metric labels come from a **closed set** enumerated in `OBSERVABILITY_SPEC.md`
  §3. An unbounded label is a memory-exhaustion primitive reachable from client
  input, so this is an architectural rule enforced by a unit test that asserts the
  registry's label sets, not a style opinion.
- Liveness and readiness are different endpoints with different meanings:
  zero upstream backends makes the LB **not ready** but still **live**.

---

## 15. Unresolved Decisions

Each is owned by a named experiment or milestone; none is silently assumed.

| ID | Question | Owner | Blocking? |
|---|---|---|---|
| **UD-1** | Does WSL2 resolve the performance effects RIFT cares about, or must v1 publish ratios only? | Experiment **E3** | Gates `PERFORMANCE_SPEC.md` targets' wording |
| UD-2 | Is the `splice()` path actually reached by `TCPConn.ReadFrom` on WSL2? | **E1** | Gates L4 + media copy strategy |
| UD-3 | Is `SO_REUSEPORT` sharding necessary at the target accept rate? | **E2** | Gates whether it ships, and default shard count |
| UD-4 | Is `hdrhistogram-go` overhead material vs a fixed-bucket in-house histogram? | **E6** | Gates the only provisional dependency |
| UD-5 | Authoritative-view coverage: query all NS servers, or a capped subset? | Phase 4 design | Non-blocking; capped by default for FD safety |
| UD-6 | Per-node auth: mTLS only, or mTLS + signed-batch? | Phase 4 | Non-blocking; mTLS ships first |
| UD-7 | Should `media` serve with `O_DIRECT`? | **E5** follow-up | Deferred; needs alignment work, evidence thin |

---

## 16. Reading Order

1. This file (boundaries, direction, data flow)
2. `TECHNICAL_SPEC.md` — identifiers, interfaces, algorithms, concurrency (**oracle**)
3. `LOAD_BALANCER_SPEC.md` · `DNS_SSL_TRACKER_SPEC.md` · `MEDIA_SERVER_SPEC.md`
4. `PERFORMANCE_SPEC.md` · `OBSERVABILITY_SPEC.md` · `SECURITY_SPEC.md`
5. `TESTING_SPEC.md` · `API_SPEC.md` · `CLI_SPEC.md` · `BENCHMARK_REPORT_TEMPLATE.md`
6. `PRD.md` (requirements) · `ROADMAP.md` (phases) · `README.md` (usage)

Cross-references are by stable ID (`AD-n`, `FR-n`, `NFR-n`, `UD-n`, `T-nn`, `E-n`)
so document renames do not sever them.
