# RIFT — Product Requirements

## Executive Summary

RIFT is a Go network operations platform: one module, one binary, three network
services sharing one platform layer. It exists to demonstrate — with
measurements rather than assertions — how high-performance network software is
actually built: where the syscalls go, what a routing decision costs, how a
monitor degrades honestly, and what bounds media throughput.

The guiding constraint is **measurement before claim**. Every performance
target in this repository is either backed by a committed benchmark or marked as
not yet measured. There are no invented numbers, and no feature is described as
working when it is not.

## Problem Statement

Three concrete gaps motivate the component selection:

- **P1 — Proxy opacity.** Generic proxies do not expose the cost of individual
  routing decisions. "Use least-connections" is advice; the per-pick latency
  and allocation count of least-connections at a thousand backends is
  knowledge. RIFT instruments the decision itself.
- **P2 — Propagation conflation.** DNS "propagation" is spoken of as one state
  when it is at least three: authoritative state, recursive-resolver state, and
  observed client-facing state. Tools that merge them produce misleading
  dashboards. RIFT records the observation class on every sample and provides
  no mechanism to emit a single "propagated worldwide" verdict.
- **P3 — Uninstrumented playback.** Media throughput is usually reported from
  the server's point of view, but a rebuffer is a client-side event invisible
  in server logs. RIFT ships a player-model client so stalls are measured where
  they happen, and server-side and client-side numbers are reported separately.

## Goals

| # | Goal | How it is verified |
|---|---|---|
| G1 | Real packet forwarding: TCP, UDP, HTTP | Integration tests over real sockets with byte-equivalence checks |
| G2 | Routing-decision cost is known and bounded | Allocation assertions in tests; `BenchmarkPick` |
| G3 | Media throughput limited by disk/NIC, not by RIFT | Throughput measured against same-host ceilings |
| G4 | Honest DNS/TLS state model | Classifier returns `unresolvable`/`insufficient_coverage` rather than a global verdict; no API can claim global propagation |
| G5 | No goroutine or descriptor leaks | Leak assertions; connection accounting tests |
| G6 | Reproducible benchmarks | Every result carries an environment card; reports regenerate from samples |
| G7 | Security controls are tested | Red-line tests listed in `SECURITY.md` §9 |

## Non-Goals

Explicitly out of scope, and each is a named milestone rather than a silent
omission:

- No QUIC/HTTP-3, no kernel bypass (XDP/DPDK), no `io_uring`, no GSO/GRO.
- No transcoding, no HLS/DASH packaging, no DRM.
- No aggregator HA or clustering in v1.
- No geographic node fleet in v1; the node protocol is real and multi-node is
  tested over loopback.
- No web UI; no OpenTelemetry adoption yet.
- No media authentication in v1 (LAN-only by design — see `SECURITY.md` §6).
- No DNSSEC validation.

## Target Users

- **Network engineer** operating L4/L7 in front of services, who needs
  trustworthy latency percentiles and per-backend distribution.
- **SRE / availability owner** who needs DNS and certificate state across
  resolvers, with alerting before expiry rather than after outage.
- **Systems programmer** studying the cost of Go networking primitives, who
  needs readable source and documented, measured tuning knobs.
- **Self-hoster** streaming media on a LAN.

## Functional Requirements

Status: **✅ implemented and tested** · **◐ partial, with the gap stated** ·
**🔜 planned milestone**. This table is the single source of truth for scope.

### Platform

| # | Requirement | Status | Verification |
|---|---|---|---|
| F-1 | Single binary; service selection by subcommand | ✅ | CLI smoke tests |
| F-2 | YAML config, strict unknown-field rejection, field-path errors, env overrides | ✅ | `config_test.go` (14 tests) |
| F-3 | Config redaction; diffs computed over redacted copies | ✅ | `TestRedactionHidesSecrets`, `TestRedactedDiffNeverLeaksSecrets` |
| F-4 | Seven-class error taxonomy driving log level, metric label, retry | ✅ | `errs_test.go` |
| F-5 | Structured logging with request/trace ID plumbing | ◐ | Package present; not yet wired through every call site |
| F-6 | Prometheus metrics registry with closed label sets | ◐ | Counters exist per component; registry wiring on the admin plane is incomplete |
| F-7 | Liveness/readiness endpoints distinct in meaning | ◐ | `/healthz` and `/readyz` served by media and hub; the LB admin plane exposes topology but not `/healthz` |
| F-8 | Ordered shutdown with drain budgets; exit 4 on exceeded drain | ◐ | The `lifecycle` package implements and tests this. **The `rift lb` command does not use it**: it waits on the signal context and closes listeners, so in-flight connections are dropped rather than drained, and exit code 4 is never produced. Wiring the services through `lifecycle` remains. |

### Load balancer

| # | Requirement | Status | Verification |
|---|---|---|---|
| F-10 | L4 TCP proxy with admission limits and timeouts | ✅ | `TestL4ByteEquivalence` (both copy modes), `TestL4AdmissionRefusesOverMax` |
| F-11 | Half-close propagation with response drain | ✅ | `TestL4HalfCloseDrain` |
| F-12 | Connect-phase failover to a healthy backend | ✅ | `TestL4FailoverToHealthyBackend` |
| F-13 | UDP proxy with per-session backend affinity | ✅ | `TestUDPBackendAffinityPerSession` |
| F-14 | UDP session table cap and idle sweep, each attributed | ✅ | `TestUDPSessionTableCapDrops`, `TestUDPSweeperReapsIdleSessions` |
| F-15 | HTTP reverse proxy with owned transport and buffer pool | ✅ | `TestL7ForwardsRequestAndResponse` |
| F-16 | Round-robin, smooth weighted RR, least-connections | ✅ | χ² distribution test, exact 5:3:1 interleave test, min-inflight tests |
| F-17 | Zero-allocation pick path | ✅ | `testing.AllocsPerRun` assertion (0 allocs at 64 backends) |
| F-18 | Active TCP and HTTP health checks with rise/fall hysteresis | ✅ | `TestTrackerHysteresis`, checker tests against real listeners |
| F-19 | Closed retry table; unknown verbs never retry | ◐ | `Retryable` implements the table and `TestRetryTable` covers 11 cases, but **the proxy never calls it**: `MaxAttempts`, `IdempotentPutDelete`, and `Retries` are parsed and never consulted. No request is retried today, so the table is a tested contract with no production call site. |
| F-20 | Hot reload without dropping established connections | ✅ | `TestReloadIncrementsVersionAndSwaps`, `TestReloadRejectsInvalidConfigRetainsPrevious` |
| F-21 | Rate limiting per source and per pool | ◐ | Sharded limiter implemented and tested; LB wiring uses the admission gate, per-source wiring pending |
| F-22 | TLS termination on listeners | 🔜 | Config parsed and validated; no cert loading yet |
| F-23 | Backend connection reuse accounting | ◐ | Transport pools connections; reuse-ratio metric pending |

### DNS monitoring

| # | Requirement | Status | Verification |
|---|---|---|---|
| F-30 | Own RFC 1035 wire engine with TTLs, rcode, TC bit | ✅ | Implemented; codec present with size caps and pointer rules |
| F-31 | Per-resolver engine with UDP → TCP fallback on truncation | ✅ | Implemented with transaction and question validation |
| F-32 | Bounded per-resolver concurrency and circuit breaking | ✅ | Engine construction and stats present |
| F-33 | Recursive-view probing with canonicalized answers | ✅ | `Fingerprint` order-independence and TTL-exclusion tests |
| F-34 | Propagation classifier with six honest states | ✅ | `classify_test.go` (12 tests) including failed-observation handling |
| F-35 | No code path may claim worldwide propagation | ✅ | `TestIsPropagatedWorldwideAlwaysFalse` |
| F-36 | Node with bounded ring, counted drops | ✅ | `TestRingMemoryIsBounded`, `TestRingDropsOldestAndCounts` |
| F-37 | Batch shipping with size/age triggers | ✅ | `TestNodeShipsObservationsToHub` |
| F-38 | Shutdown flush that does not lose data | ✅ | `TestNodeFlushesOnShutdown` |
| F-39 | Bounded offline spool on hub outage | ✅ | `TestNodeSpoolsOnHubFailure`, `TestNodeDrainSpool` |
| F-40 | Hub ingest with validation and batch caps | ✅ | `TestHubIngestAndQuery`, `TestHubRejectsMalformedObservation`, `TestHubRejectsOversizedBatch` |
| F-41 | Bounded hub window | ✅ | `TestWindowBoundsMemory` |
| F-42 | Append-only JSONL segment persistence | ✅ | `TestHubWritesSegments` |
| F-43 | Query API: observations, propagation, targets | ✅ | `TestHubPropagationEndpoint`, pagination cap test |
| F-44 | Multi-node aggregation | ✅ | `TestTwoNodeIntegration` |
| F-45 | Authoritative-view (RD=0) NS discovery | 🔜 | View type and classifier support it; NS discovery not implemented |
| F-46 | mTLS identity binding for node ingest | 🔜 | mTLS material validated; cert→node-ID binding not enforced (`SECURITY.md` §4) |
| F-47 | Alert evaluation and delivery | 🔜 | Thresholds configurable and validated; evaluator not implemented |

### TLS monitoring

| # | Requirement | Status | Verification |
|---|---|---|---|
| F-50 | Handshake capture without verification, manual chain build | ✅ | Implemented per `SECURITY.md` §5.1 |
| F-51 | Findings from a closed code set, never an abort | ✅ | `TestProbeValidSelfSignedReportsSelfSigned` |
| F-52 | Expiry, weak key, weak signature detection | ✅ | `TestProbeExpiredCert`, `TestProbeWeakRSAKey` |
| F-53 | Hostname verified independently of chain trust | ✅ | `TestProbeHostnameMismatch` (caught a masking bug) |
| F-54 | Strict mode for compliance checks | ✅ | `TestProbeStrictModeFailsOnFindings` |
| F-55 | SSRF-guarded targets | ✅ | `TestProbeSSRFGuardRefusesInternal` |
| F-56 | One-shot `tls check` CLI | ✅ | Verified live against a public host |

### Media

| # | Requirement | Status | Verification |
|---|---|---|---|
| F-60 | RFC 7233 range semantics: 206 / 416 / 200 | ✅ | `TestMediaRangeRequests` (9 cases), `TestParseRangeMatrix` (23 cases) |
| F-61 | Malformed ranges produce 416, never a silent 200 | ✅ | Included in the matrix above |
| F-62 | Strong ETags and `If-Range` | ✅ | `TestMediaIfRangeMismatchServesFull`, `TestStrongETagStabilityAndChange` |
| F-63 | Bounded memory independent of file size | ✅ | `TestMediaEmptyFileAnyRangeIs416`; readahead bounded by config |
| F-64 | Global and per-client stream limits | ✅ | `TestMediaAdmissionLimits` |
| F-65 | Path traversal refusal | ✅ | `TestMediaPathTraversal` (10 attack strings) |
| F-66 | Peer IP for admission, `X-Forwarded-For` ignored | ✅ | `TestClientIPIgnoresForwardedHeader` |
| F-67 | Filesystem-metadata index with pattern exclusion | ✅ | `TestMediaIndexBuild` |
| F-68 | Sendfile and buffered copy modes | ✅ | Both implemented; live `206` verified |
| F-69 | SIGHUP index rebuild | ✅ | Wired in `cmd/rift` |

### Benchmark harness

| # | Requirement | Status | Verification |
|---|---|---|---|
| F-70 | Environment card | ✅ | `rift bench env` |
| F-71 | Open-loop scheduling that records intended start | ✅ | `TestOpenLoopRecordsIntendedStart` |
| F-72 | Raw NDJSON samples | ✅ | Written per run |
| F-73 | Percentile summary | ✅ | `TestPercentileNearestRank` |
| F-74 | Tolerance-band comparison | ✅ | `TestCompareFlagsRegression`, `TestCompareHigherIsBetterDirection` |
| F-75 | Report refuses without an environment card | ✅ | `TestReportRefusesWithoutCard` |
| F-76 | Player model measuring startup and stalls | ◐ | Implementation present; no committed measurement yet |
| F-77 | Scenario registry | ✅ | Three scenarios under `bench/scenarios/` |

## Non-Functional Requirements

Targets marked **[unmeasured]** are deliberately empty: this repository does not
publish a performance number it has not measured on a recorded environment.

| # | Requirement | Target | Status |
|---|---|---|---|
| N-1 | Pick path allocation count | 0 allocs/op | ✅ asserted |
| N-2 | Retry safety | closed table, unknown verbs never retry | ✅ asserted |
| N-3 | Reload safety | previous snapshot retained on invalid reload | ✅ asserted |
| N-4 | Bounded memory under hostile key cardinality | capacity-bounded | ✅ asserted |
| N-5 | Media memory per stream | independent of file size | ✅ asserted |
| N-6 | UDP session count | bounded by configuration | ✅ asserted |
| N-7 | Sustained media throughput vs disk/NIC | ≥ 0.8 × measured ceiling | **[unmeasured]** — needs the Linux benchmark host |
| N-8 | L4 throughput and latency percentiles | — | **[unmeasured]** |
| N-9 | L7 overhead vs direct server | — | **[unmeasured]** |
| N-10 | 2-hour soak stability | RSS growth < 2% | **[unmeasured]** |
| N-11 | Goroutine leak freedom | post-shutdown baseline | ◐ asserted in several tests; project-wide `goleak` wiring pending |

## Success Criteria

The project is "v1 complete" when: every ✅ requirement above has a passing
test; every **[unmeasured]** target has a committed measurement with its
environment card; and the benchmark reports regenerate from stored samples.

Until then, no performance claim is made, and this document says so.

## See Also

[`ARCHITECTURE.md`](ARCHITECTURE.md) · [`SECURITY.md`](SECURITY.md) ·
[`PERFORMANCE.md`](PERFORMANCE.md) · [`ROADMAP.md`](ROADMAP.md)
