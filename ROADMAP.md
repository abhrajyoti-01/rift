# RIFT Roadmap

Phases are cumulative. A phase is complete when its exit criteria are met and
verified, not when its code exists.

Status marks: ✅ done · ◐ partial · 🔜 not started.

---

## Phase 0 — Foundation ✅

**Objective:** a repository that can build, test, and be reasoned about.

| Deliverable | Status |
|---|---|
| Go module with pinned toolchain | ✅ `go 1.25.5` |
| Package layout matching the architecture | ✅ |
| Error taxonomy and exit-code mapping | ✅ |
| Compiling skeleton for every subsystem | ✅ superseded by real implementations |

**Exit criteria:** `go build ./...` and `go test ./...` succeed. ✅

---

## Phase 1 — Platform Primitives ✅

**Objective:** the shared substrate every service depends on.

| Deliverable | Status | Tests |
|---|---|---|
| `errs` — taxonomy, wrapping, exit codes | ✅ | 5 |
| `netx` — SSRF dial guard, listeners | ✅ | 9 |
| `config` — strict YAML, validation, redaction, diff, env | ✅ | 15 |
| `pool` — bounded executor | ✅ | 6 |
| `ratelimit` — sharded token buckets with bounded cardinality | ✅ | 6 |
| `circuit` — breaker with injectable clock | ✅ | 5 |
| `retry` — policy with closed retryable class set | ✅ | 6 |

**Exit criteria:** every primitive has tests that assert its contract,
including the failure modes (bounded memory, refused overflow, fail-closed
configuration). ✅

---

## Phase 2 — Load Balancer ✅

**Objective:** real forwarding on all three transports.

| Deliverable | Status | Tests |
|---|---|---|
| TCP forwarding with admission, failover, half-close | ✅ | 7 |
| UDP sessions with per-session affinity and sweep | ✅ | 6 |
| HTTP reverse proxy with owned transport | ✅ | 11 |
| Three pickers with distribution assertions | ✅ | 12 |
| Active health checks with hysteresis | ✅ | 10 |
| Zero-allocation pick path | ✅ | asserted |
| Closed retry table | ✅ | 11 cases |
| Snapshot control plane and reload | ✅ | 9 |
| Admin API with authorization | ✅ | red-line tested |
| Live end-to-end verification | ✅ | request proxied to a real backend |

**Exit criteria:** byte equivalence in both copy modes; reload retains the
previous snapshot on invalid input; open-proxy refusal proven by test. ✅

**Not in this phase:** TLS termination (validated in config, not yet
implemented), per-source rate-limit wiring, reuse-ratio metrics.

---

## Phase 3 — DNS Monitoring ✅ / ◐

**Objective:** observe resolver state honestly and aggregate it.

| Deliverable | Status | Tests |
|---|---|---|
| Wire codec with TTLs, rcode, TC, size caps | ✅ | codec present; fuzz targets 🔜 |
| Resolver engine with UDP→TCP fallback | ✅ | present |
| Recursive-view probing and canonicalization | ✅ | fingerprint tests |
| Six-state propagation classifier | ✅ | 12 |
| Bounded ring with attributed drops | ✅ | 3 |
| Batch shipping with size/age triggers | ✅ | 4 |
| Shutdown flush without data loss | ✅ | regression test |
| Bounded offline spool | ✅ | 2 |
| Hub ingest with validation and caps | ✅ | 6 |
| Bounded window and JSONL segments | ✅ | 2 |
| Query API | ✅ | 3 |
| Two-node aggregation | ✅ | 1 |
| Live end-to-end verification | ✅ | node → hub → query |
| Authoritative-view NS discovery | 🔜 | — |
| mTLS identity binding | 🔜 | — |
| Alert evaluation | 🔜 | — |

**Exit criteria met:** the pipeline runs end to end against real resolvers, and
the classifier returns `unresolvable` rather than a false "converged" when it
lacks ground truth. ✅

---

## Phase 4 — TLS Monitoring ✅

| Deliverable | Status | Tests |
|---|---|---|
| Handshake capture + manual verification inversion | ✅ | 8 |
| Closed finding-code set | ✅ | contract |
| Expiry, weak key, weak signature detection | ✅ | 3 |
| Hostname independence from chain trust | ✅ | regression test |
| Strict compliance mode | ✅ | 1 |
| SSRF-guarded targets | ✅ | 1 |
| Live verification against a public host | ✅ | TLS 1.3 negotiated, chain reported |

---

## Phase 5 — Media Serving ✅

| Deliverable | Status | Tests |
|---|---|---|
| RFC 7233 parsing | ✅ | 23-case matrix |
| 206 / 416 / 200 semantics | ✅ | 9 cases + live verification |
| Strong ETags and `If-Range` | ✅ | 2 |
| Path traversal refusal | ✅ | 10 attacks |
| Admission limits | ✅ | 2 |
| Filesystem index with exclusions | ✅ | 1 |
| Sendfile and buffered modes | ✅ | live `206` |
| SIGHUP index rebuild | ✅ | wired |
| Bounded per-stream memory | ✅ | structural |

---

## Phase 6 — Benchmark Harness ◐

| Deliverable | Status | Tests |
|---|---|---|
| Environment card | ✅ | CLI |
| Open/closed-loop runner with raw samples | ✅ | 5 |
| Intended-start recording | ✅ | 1 |
| Percentile summary | ✅ | 1 |
| Tolerance-band comparison | ✅ | 3 |
| Report with mandatory card | ✅ | 2 |
| Scenario registry | ✅ | 3 scenarios |
| Player model | ◐ | implementation present, no measurement |
| Soak/stress automation | 🔜 | — |
| Nginx baseline comparison | 🔜 | — |

---

## Phase 7 — Wiring the Platform Packages ◐

**Objective:** make the platform primitives reachable from the code paths a
user actually runs. An audit of imports found nine of the thirteen platform
packages imported by nothing; each of these is implemented, tested, and
unreachable.

| Deliverable | Status | Why it matters |
|---|---|---|
| Wire `retry.Policy` + the L7 closed table into the proxy error path | 🔜 | `retry.max_attempts` and `idempotent_put_delete` are currently ignored |
| Wire `ratelimit.Sharded` into L4/L7 admission | 🔜 | `rate_limit.*` is currently ignored; rate limiting is not active anywhere |
| Run services through `lifecycle.App` | 🔜 | `shutdown_timeout` is ignored and exit code 4 is unreachable; in-flight connections are closed rather than drained |
| Serve `metrics.Registry` on the admin plane | 🔜 | counters exist per component but nothing exports them |
| Adopt `httpx` for data-plane servers | 🔜 | timeout sets are currently constructed ad hoc per service |
| Adopt `pool.Executor` where the work unit is not a socket | 🔜 | health checks and DNS queries currently start goroutines directly |
| Wire `health.Registry` into `/readyz` | 🔜 | LB readiness is not exposed |
| Structure `logging` calls at every boundary | ◐ | the package exists; call sites are inconsistent |
| Use `testsupport` fixtures in integration tests | 🔜 | fakes are currently hand-rolled per package |

**Exit criteria:** no platform package is unreachable, and each row has a test
that exercises the feature through the CLI rather than only through the package
API. This phase exists because a tested-but-unwired feature is the exact failure
mode this project's documentation rules are meant to prevent.

---

## Phase 8 — Observability Completion 🔜

| Deliverable | Status |
|---|---|
| Prometheus `/metrics` on the admin plane | 🔜 |
| Label-cardinality enforcement test | 🔜 |
| Full structured-log wiring | ◐ |
| Grafana dashboards | 🔜 |
| Alert rules | 🔜 |
| Project-wide goroutine-leak assertions | 🔜 |

---

## Phase 9 — Measurement 🔜

**Blocked on a suitable Linux host, not on code.**

| Deliverable | Status |
|---|---|
| Environment card for the benchmark host | 🔜 |
| `iperf3` / `fio` ceiling measurement | 🔜 |
| Every `[unmeasured]` target in `PERFORMANCE.md` §6 | 🔜 |
| Published reports with cards | 🔜 |
| Nginx comparison with alternating runs | 🔜 |

**This phase produces the only performance claims the project will ever make.**

---

## Milestones (post-v1)

Each is fully designed before implementation; none is a stub today.

| # | Milestone | Precondition |
|---|---|---|
| M1 | TLS termination on LB listeners with cert reload | Phase 2 follow-up |
| M2 | Per-source and per-pool rate-limit wiring in the LB | Phase 2 follow-up |
| M3 | Authoritative-view NS discovery | DNS wire maturity |
| M4 | mTLS identity binding for node ingest | certificate tooling |
| M5 | Alert evaluation and delivery | threshold semantics stable |
| M6 | Geographic multi-node deployment | a second real network |
| M7 | Hub persistence replay tooling (`rift dns replay`) | segment format stable |
| M8 | Fuzz targets for the wire parser and range parser | — |
| M9 | DNSSEC validation | wire engine maturity |
| M10 | Media token authentication | non-LAN exposure |
| M11 | HLS/CMAF packaging | throughput story proven |
| M12 | OpenTelemetry tracing | cross-network spans justified |

---

## Definition of Done for v1

1. Every ✅ in `PRD.md` has a passing test in CI.
2. Every `[unmeasured]` in `PERFORMANCE.md` has a committed measurement **with
   its environment card**.
3. Reports regenerate from stored samples without re-running.
4. `go build`, `go vet`, and `go test -race` are all green.
5. No document claims a capability that `PRD.md` marks as 🔜.

Until all five hold, the README states exactly which claims are evidenced and
which remain open.