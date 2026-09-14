# PRD.md — RIFT Product Requirements Document

Status: **Revision A — normative for requirement IDs**. Functional requirements
(`FR-nn`), non-functional requirements (`NFR-nn`), goals (`G-n`), and problems
(`P-n`) defined here are cited by stable ID from every other document. Scope
decisions: multi-resolver node + real node protocol; progressive HTTP media with
byte-range; WSL2+Docker benchmark environment; `rift` binary, module
`github.com/rift/rift`. Oracles: `ARCHITECTURE.md` (boundaries), `TECHNICAL_SPEC.md`
(identifiers). Toolchain: Go 1.25.5.

---

## 1. Executive Summary

RIFT is one Go module producing one binary with three network services sharing
one platform layer: an L4/L7 load balancer, a multi-resolver DNS + TLS
certificate monitoring system (node + hub), and a high-throughput byte-range
media server. The organizing principle is **measurement before claim**: every
optimization is gated behind a benchmark or profile, every performance target is
stated as a measurable quantity with a verification method, and every report
carries an environment card or does not exist.

The three components are chosen because they stress different parts of the
stack: the LB is a syscall-and-allocation problem, DNS/TLS is a
latency-distribution and untrusted-parser problem, media is a
sustained-bandwidth and backpressure problem. The platform layer exists because
three real consumers prevent single-consumer abstractions from calcifying.

Development runs on Windows (Go 1.25.5); all performance work runs in WSL2
Ubuntu. Docker is not yet installed on the host — provisioning it is a Phase 0
task (`ROADMAP.md`), and no container-based claim is made until it exists.

## 2. Problem Statement

- **P1 — Proxy opacity.** Generic proxies (Nginx et al.) do not expose the cost
  of individual routing decisions. "Use least_conn" is advice; the per-pick
  latency and allocation cost of `least_conn` at 1000 backends is knowledge.
  RIFT makes each mechanism measurable and comparable under a controlled A/B.
- **P2 — Propagation conflation.** "DNS propagation" is spoken of as one state.
  It is at least three: authoritative state, recursive-resolver cached state,
  and observed client-facing state. Tools that merge them produce misleading
  dashboards. RIFT records the observation class on every sample and refuses
  to emit a single "propagated worldwide" verdict.
- **P3 — Uninstrumented playback claims.** Media throughput is usually reported
  from the server side, but a rebuffer is a client-side event invisible in
  server logs. RIFT ships a player-model client (`bench/playersim`) with a
  playback clock, so startup latency and stall events are measured where they
  occur, and server-side metrics are never quoted as client experience.

## 3. Goals

| ID | Goal | Verified by |
|---|---|---|
| G1 | Real packet forwarding: L4 TCP/UDP, L7 HTTP/1.1 (+HTTP/2 termination) | Integration tests over real sockets, byte-equivalence checks |
| G2 | Routing-decision cost known and bounded | `testing.B` + `benchstat` in CI; allocs/op asserted |
| G3 | Media throughput limited by disk/NIC, not by RIFT | Syscall counts and CPU profile at target rate; ratio vs measured ceilings |
| G4 | Honest DNS/TLS state model, three observation classes distinguished | Unit tests on classifier; no code path emits a worldwide verdict (T-51) |
| G5 | Zero data-plane goroutine leaks across reload/drain/shutdown | `goleak` in CI; soak with goroutine-count regression |
| G6 | Reproducible benchmarks, one-command regeneration | `rift bench report` refuses to emit without environment card |
| G7 | Race-clean and fuzz-clean | `-race` in CI; fuzz targets on DNS wire parser, Range header, config loader |
| G8 | Hot config reload without dropping established connections | Reload-under-load test asserting zero reload-attributable resets |

## 4. Non-Goals

Each is a named deferral, not an omission:

- QUIC / HTTP-3; TLS 1.0/1.1 support; XDP/DPDK/eBPF/kernel bypass; `io_uring`;
  GSO/GRO; `MSG_ZEROCOPY`; hardware offload (AD-12: none measurable on WSL2;
  revisit requires a real Linux host).
- Transcoding, HLS/DASH packaging, DRM, container parsing (AD-9; milestone).
- Aggregator HA/clustering — single hub, documented SPOF.
- Geographic node fleet in v1. One node querying many real resolvers; real
  node→hub protocol; two-node LAN test. Three-geography deployment: milestone.
- Web UI (Grafana JSON only). RBAC (mTLS or static bearer). gRPC/protobuf for
  node→hub (AD-8). OpenTelemetry adoption (AD-13).

## 5. Target Users

- **U1 — Network engineer** operating L4/L7 in front of services; needs
  trustworthy latency percentiles and per-backend distribution.
- **U2 — SRE / availability owner** needing DNS and certificate state across
  resolvers with expiry alerting.
- **U3 — Systems programmer** (primary) studying the cost of Go networking
  primitives; needs readable source and documented, measured tuning knobs.
- **U4 — Self-hoster** streaming media on a LAN.

## 6. Functional Requirements

Status legend: **v1** = first production-ready version (see `ROADMAP.md` phases);
**M2+** = named milestone. Every FR names its verification.

### Platform

| ID | Requirement | Status | Verification |
|---|---|---|---|
| FR-1 | Single `rift` binary; service by subcommand; `rift init` writes commented starter config | v1 | CLI integration test |
| FR-2 | YAML config, strict unknown-field rejection, programmatic validation with field-path errors; `rift config validate` safe in CI | v1 | Config test matrix incl. typo cases |
| FR-3 | Graceful shutdown on SIGINT/SIGTERM: stop accepting → bounded drain → flush → exit; exit 4 if drain deadline fires | v1 | Lifecycle tests incl. shutdown-under-load |
| FR-4 | `/healthz`, `/readyz`, `/metrics` on a dedicated admin listener, never the data plane | v1 | API tests; port-scan assertion |
| FR-5 | Structured JSON `log/slog`; per-connection `rid`, per-operation `tid`; closed field set; cardinality caps | v1 | Log schema test |
| FR-6 | `rift config diff` prints field-path diff between file and running snapshot | v1 | Diff unit tests |
| FR-7 | Env overrides `RIFT_<SECTION>_<KEY>` for documented scalar keys only | v1 | Override test matrix |

### Load balancer

| ID | Requirement | Status | Verification |
|---|---|---|---|
| FR-10 | L4 TCP proxy: N listeners → backend pools, splice path with pooled-copy fallback | v1 | Byte-equivalence soak test |
| FR-11 | L4 UDP proxy: session table, per-session affinity, idle sweep, hard session cap, drop counters | v1 | UDP session tests incl. flood |
| FR-12 | L7 HTTP load balancing via owned ReverseProxy extension points; HTTP/1.1 + HTTP/2 termination | v1 | Protocol conformance tests |
| FR-13 | Pickers: round-robin, smooth weighted round-robin, least-connections | v1 | Distribution tests (χ²); pick benchmarks |
| FR-14 | Active health checks (TCP, HTTP), rise/fall thresholds, per-check timeout | v1 | Fault-injection tests |
| FR-15 | Limits: per-listener, per-backend conn caps; per-conn/read/write/idle timeouts | v1 | Limit tests |
| FR-16 | Backend keep-alive reuse (L7) with reuse-ratio metric | v1 | Reuse assertion under keep-alive load |
| FR-17 | Retry per closed table (§4.7 TECHNICAL_SPEC); connect-phase vs post-write phases distinguished via httptrace | v1 | Retry safety tests incl. non-idempotent refusal |
| FR-18 | Token-bucket rate limiting per source-IP and per-pool | v1 | Bypass-attempt tests |
| FR-19 | Reload via SIGHUP and admin API through one validate-then-swap path; zero established-conn drops | v1 | Reload-under-load (G8) |
| FR-20 | `X-Forwarded-For` append-only; RFC 7239 `Forwarded` option; trusted-proxy boundary for attribution | v1 | Spoof tests |
| FR-21 | Per-conn, per-read, per-write, idle deadlines; slow-client culling with counters | v1 | Slowloris tests |

### DNS + TLS tracker

| ID | Requirement | Status | Verification |
|---|---|---|---|
| FR-25 | Own wire-level DNS engine: A, AAAA, CNAME, MX, TXT, NS; UDP with TCP fallback on TC | v1 | `dig` byte-equivalence across record types |
| FR-26 | Two views per target: recursive (RD=1, configured resolvers) and authoritative (RD=0 via NS discovery) | v1 | View classification tests |
| FR-27 | Per observation: node ID, location, resolver, timestamp, rcode, answers with TTL, latency, transport, truncation, error class | v1 | Observation schema tests |
| FR-28 | Propagation classifier with states: unknown, insufficient-coverage, unresolvable, divergent, converging, converged | v1 | Classifier unit tests + fixtures |
| FR-29 | TLS probe: chain, expiry, issuer/subject, SANs, negotiated version/cipher, chain problems, hostname validation, handshake failures | v1 | Probe tests against self-signed matrix |
| FR-30 | Node→hub batch ingestion over mTLS; bounded ring; backpressure; offline spool; drop counters | v1 | Two-node integration test |
| FR-31 | Alerts: cert expiry windows, consecutive failures, view divergence | v1 | Alert evaluation tests with fake clock |
| FR-32 | `rift dns query` / `rift tls check` one-shot live tools; no cached/synthetic answers | v1 | CLI tests against real resolvers (network-gated) |
| FR-33 | Hub persistence: bounded memory window + append-only JSONL segments; `rift dns replay` re-materializes | v1 | Replay round-trip test |
| FR-34 | Multi-record-type fingerprint comparison excluding TTL decay | v1 | Fingerprint unit tests |

### Media

| ID | Requirement | Status | Verification |
|---|---|---|---|
| FR-35 | GET/HEAD with single byte-range; 206/416/200 semantics per RFC 7233; strong ETags; If-Range | v1 | Range test matrix incl. malformed |
| FR-36 | Seek without full-file buffering; memory per stream bounded by config, not file size | v1 | Memory assertion at 10 GiB fixture |
| FR-37 | Readahead configurable; sendfile and buffered modes; measured default per E5 | v1 | E5 result + config test |
| FR-38 | Global + per-client stream limits; 429 + Retry-After on exhaustion | v1 | Limit tests |
| FR-39 | Per-stream accounting: bytes, duration, first-byte, send-stall | v1 | Metrics assertion tests |
| FR-40 | Index from filesystem metadata only; rebuild on reload; 100k assets < 2s | v1 | NFR-8 benchmark |
| FR-41 | `HEAD` parity; `Accept-Ranges: bytes` always; suffix ranges | v1 | Protocol tests |

### Bench and CLI

| ID | Requirement | Status | Verification |
|---|---|---|---|
| FR-45 | `rift bench run|compare|report`; scenarios by stable ID; raw NDJSON samples; CI regression gate with tolerance bands | v1 | Self-hosted bench of bench |
| FR-46 | Every reported metric names its method and units; environment card mandatory for reports | v1 | Report refusal test |
| FR-47 | `rift bench env` captures host, kernel, cgroups, NIC, disk ceilings (iperf3/fio), WSL2 flags | v1 | Card schema test |
| FR-48 | Player-model client measuring startup latency, stalls, underrun time | v1 | Playersim tests against synthetic server |

## 7. Non-Functional Requirements

Numbers marked *[E-n]* are set/revised by the named Phase 0 experiment before
the performance phases; empty targets are **deliberately empty** — filling them
without measurement is prohibited (AR-5).

| ID | Property | Requirement | Verification |
|---|---|---|---|
| NFR-1 | L4 accept→first-byte allocations | ≤ 2 allocs | `BenchmarkL4Accept` assert |
| NFR-2 | Pick cost @1000 backends | 0 allocs, p99 < 1 µs [G-sensitive] | `BenchmarkPick` |
| NFR-3 | Reload safety | 0 established conns dropped by reload | Reload-under-load |
| NFR-4 | Memory stability | RSS growth < 2% over 2 h fixed load | Soak + sampler |
| NFR-5 | FD usage | < 70% of rlimit at nominal load | Procfs sampler |
| NFR-6 | Goroutine cleanliness | Post-shutdown count = baseline ±0 | `goleak` TestMain |
| NFR-7 | Media throughput | ≥ 0.8 × min(measured disk, NIC) at 10 concurrent streams [E3] | `media.capacity-ramp` |
| NFR-8 | Index build | 100k assets < 2 s | `BenchmarkIndexBuild` |
| NFR-9 | Node memory | ≤ 256 MiB at any observation rate (ring-bounded) | Flood test + RSS assert |
| NFR-10 | Wire engine | Decode ≤ 3 allocs typical response; encode 1 alloc | Benchmarks |
| NFR-11 | Security posture | No unauthenticated open-proxy path; SSRF deny-set enforced at dial; no error-string matching in retry paths | Security suite + CI greps (T-34) |
| NFR-12 | L7 overhead | ≤ 15% vs direct `http.Server` at plateau [E3] | A/B harness |
| NFR-13 | Toolchain | `-race`, `-vet` clean; fuzz targets run nightly ≥ 10 min | CI gates |

## 8. Requirement Traceability

| Spec | Owning requirement IDs |
|---|---|
| `LOAD_BALANCER_SPEC.md` | FR-10..FR-21, NFR-1..NFR-3, NFR-12 |
| `DNS_SSL_TRACKER_SPEC.md` | FR-25..FR-34, NFR-9, NFR-10 |
| `MEDIA_SERVER_SPEC.md` | FR-35..FR-41, NFR-7, NFR-8 |
| `PERFORMANCE_SPEC.md` | NFR-1..NFR-13, G2, G3, G6 |
| `OBSERVABILITY_SPEC.md` | FR-4, FR-5, FR-39 |
| `SECURITY_SPEC.md` | NFR-11, FR-17..FR-21 posture |
| `TESTING_SPEC.md` | G1..G8 verification mapping |
| `API_SPEC.md` | FR-30, FR-33 query/ingest contracts |
| `CLI_SPEC.md` | FR-1, FR-6, FR-32, FR-45..FR-48 |

## 9. Acceptance (product level)

Per-phase acceptance criteria with owners and gates live in `ROADMAP.md`. The
product is "v1 complete" when: all v1 FRs trace to passing tests; all NFRs have
measured values (not placeholders) committed in `bench/results/`; the benchmark
report for each component is regenerable by one command from a clean clone in
WSL2; and `README.md`'s quickstart reproduces from scratch. No claim of
production readiness is made before those hold, and `README.md` states exactly
which claims are evidenced and which remain open.
