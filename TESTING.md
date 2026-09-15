# RIFT Testing Strategy

Every claim in this repository is meant to be backed by a test that can fail.
This document describes how the tests are organized, what they cover, and —
importantly — what they deliberately do **not** cover.

---

## 1. Principles

1. **Tests never dial the public internet.** Integration tests bind loopback
   listeners. A test that needs the network is not a unit test, and a test suite
   that depends on `example.com` being up is a flaky suite.
2. **Tests never sleep to wait for a condition.** Timing-dependent logic takes an
   injected clock, or the test polls with a deadline.
3. **A test asserts behaviour, not existence.** "No panic occurred" is not a
   test result.
4. **Every bug found gets a regression test.** Where a test caught a real defect
   during development, that is noted below.
5. **Race detection is on.** `go test -race` is the default invocation.

---

## 2. Test Layers

| Layer | Scope | Example |
|---|---|---|
| Unit | pure logic: parsers, classifiers, hysteresis, codecs | `TestParseRangeMatrix`, `TestClassifyDivergentBeyondWindow` |
| Contract | closed tables and enumerations that must not drift | `TestRetryTable`, `TestIsPropagatedWorldwideAlwaysFalse` |
| Integration (loopback) | real sockets, real HTTP, real TLS handshakes | `TestL4ByteEquivalence`, `TestMediaRangeRequests` |
| Security red-line | the controls in `SECURITY.md` §9 | `TestGuardDeniesDefaultSet`, `TestL7NeverRoutesByHostHeader` |
| Memory bound | hostile cardinality and capacity guarantees | `TestRingMemoryIsBounded`, `TestWindowBoundsMemory` |
| Deterministic timing | breaker and budget transitions | `TestBreakerHalfOpenProbeBudgetReachesThreshold` |
| Measurement | benchmark harness correctness | `TestReportRefusesWithoutCard`, `TestCompareFlagsRegression` |

---

## 3. Coverage by Component

### 3.1 Platform

| Area | Tests |
|---|---|
| Error taxonomy | class label set is closed; wrapping preserves the cause; `ClassOf` walks chains; exit-code mapping; drain matching |
| SSRF guard | 14 deny ranges; multi-answer refusal; no re-resolution; allow-list override via a real local dial; fail-closed config; metadata address |
| Config | 15 tests: valid load, unknown-field rejection, all-violations reporting, routable-admin refusal, literal-IP resolver rule, media validation, byte-size parsing, redaction, redacted diff, env overrides, skip-env, duration parsing |
| Pool | processes all work; never blocks on full; closed rejection; drain on close; budget exceeded names leftovers; concurrent submit/close |
| Rate limit | burst and refill; atomic `AllowN`; unconfigured key fails closed; 10k-key flood stays bounded with evictions counted; shard spread |
| Circuit breaker | opens on consecutive failures; half-open probe budget reaches the threshold; probe failure reopens; success resets the counter; defaults |
| Retry | attempt budget; disabling; the closed retryable class set; exponential backoff with cap; jitter bounds; nil-error handling |

### 3.2 Load balancer

| Area | Tests |
|---|---|
| L4 TCP | byte equivalence in **both** copy modes; half-close drain of a large response; admission refusal; failover to a healthy backend; no-backend close; context-cancel shutdown; connection accounting returns to zero |
| L7 HTTP | request/response forwarding; distribution across backends; **open-proxy refusal**; CL+TE rejection; oversized body; XFF append-only and trusted-proxy behaviour; RFC 7239; dead upstream `502`; no-backend `502`; Host preservation |
| Retry table | 11 cases covering every method and phase, including unknown verbs |
| UDP | echo through a session; **per-session backend affinity**; table cap; idle sweep with attributed drops; no-route drops; accounting |
| Pickers | χ² uniformity; unhealthy skipping; all-unhealthy `ErrNoUpstream`; exact 5:3:1 smooth interleave with no 3-in-a-row burst; redistribution when a heavy backend leaves; least-connections minimum; tie spreading; load-shift response; **zero allocations**; concurrent safety |
| Health | live and dead TCP checks; HTTP status-set matching; default `200`; timeout is unhealthy; checker selection by kind; **rise/fall hysteresis**; failure resets the pass counter; defaults |
| Control | snapshot build; invalid config refuses to publish; reload increments version and swaps; **invalid reload retains the previous snapshot**; removed backends marked draining; **admin auth from a remote peer**; token auth; method rejection |

### 3.3 DNS

| Area | Tests |
|---|---|
| Classifier | empty window → unknown; no authoritative → unresolvable; too few resolvers → insufficient coverage; convergence; divergence beyond the window with the divergent resolver named; converging within the window; failed observations excluded; freshest authoritative is the reference; **no worldwide verdict is reachable**; closed state labels; fingerprint excludes TTL and is order-independent |
| Ring | drops oldest and counts; take respects count; **100k puts into a 64-slot ring stay bounded** with exact drop count |
| Node | ships to a hub; **shutdown flush does not lose data**; spools on hub failure; spool drains on recovery; requires a hub URL; wire round-trip preserves answers and location |
| Hub | ingest and query; malformed rejection; invalid view rejection; oversized batch; method rejection; segment persistence on disk; propagation endpoint converged and divergent; stale reference → unresolvable; **pagination cap at 1000**; query requires a target; lifecycle; **two-node integration** |

### 3.4 TLS

| Area | Tests |
|---|---|
| Probe | valid self-signed is reported not aborted; expired cert flagged critical; 1024-bit RSA flagged weak; **hostname mismatch detected independently of chain trust**; connection refused classified; **SSRF guard refuses an internal target**; strict mode fails on findings; negotiated version and cipher recorded; target splitting; days-until-expiry |

### 3.5 Media

| Area | Tests |
|---|---|
| Range parsing | 23-case matrix; empty representation; integer overflow |
| HTTP semantics | full body; 9-case range matrix; **content maps to correct file offsets**; HEAD parity; `If-Range` stale vs matching; empty file `416`; method rejection |
| Security | **10 traversal attacks** both non-200 and non-leaking; name sanitizer accept/reject lists; peer IP ignores `X-Forwarded-For` |
| Limits | per-client admission with `Retry-After`; release restores the slot |
| Index | nested paths, exclusion patterns, atomic swap |
| Integration | real listener serving a real `206` |

### 3.6 Benchmark harness

| Area | Tests |
|---|---|
| Report | **refuses without a card**; renders with one |
| Compare | flags a regression; within-band passes; `HigherIsBetter` direction |
| Loadgen | closed-loop records samples; **open-loop records intended start**; 5xx counts as completed; unreachable target scores errors; nearest-rank percentile; scenario defaults |

---

## 4. Defects Found by These Tests

Recorded because they demonstrate the tests are load-bearing rather than
decorative:

| Defect | Found by | Fix |
|---|---|---|
| Pool `Close` deadlocked (workers only exited on ctx-cancel) | `TestCloseDrainsQueuedWork` | Close now closes worker queues |
| Circuit breaker could never close with `SuccessThreshold > 1` (one probe admitted) | `TestBreakerHalfOpenProbeBudgetReachesThreshold` | Half-open probe budget equals the threshold |
| Lifecycle called `Stop` twice and ignored the configured budget | `TestDrainDeadlineExitsFour` | Fixed ordering; budget made injectable |
| **Absolute path `/etc/passwd` was rewritten into a relative lookup** | `TestMediaPathTraversal` | Refusal on the raw string, no normalizing |
| Untrusted-root finding **masked** a hostname mismatch | `TestProbeHostnameMismatch` | Hostname verified independently |
| Hub never closed its segment file (descriptor leak) | Hub tests on Windows | Added `Close` |
| Node shutdown flush used a canceled context (silent data loss) | `TestNodeFlushesOnShutdown` | Detached bounded flush context |
| Bench runner counted its own shutdown as server errors | `TestClosedLoopRunnerRecordsSamples` | Stop signal separated from request context |
| Media write deadline never applied (interface assertion never matched) | code review | `http.ResponseController` |
| Config required `media.root` even when media was unused | config tests | Requirement made conditional |
| Resolver validator rejected valid `1.1.1.1:53` | `rift config validate` | Literal-IP check split host/port |
| Spool filenames collided within one clock tick, so two batches overwrote each other and byte accounting disagreed with disk | `TestNodeDrainSpool` (intermittent) | Monotonic sequence in the filename |
| Node spool directory was built by appending to the hub **URL**, producing a path containing `://` — the offline buffer silently never worked | code review of the CLI wiring | Dedicated `dns.spool_dir` field, validated to reject URLs (`TestSpoolDirMustBeAPath`) |
| Hub required mTLS certificate fields in configuration but **listened in plaintext regardless** — a documented control that did not exist | code review of the CLI/hub wiring | Real `tls.Listen` with `RequireAndVerifyClientCert`; unauthenticated and foreign-CA clients now fail at handshake |

---

## 5. What Is Not Tested

Stated so the gaps are visible:

| Gap | Why | Consequence |
|---|---|---|
| Public-internet DNS behaviour | tests must not depend on the network | resolver behaviour against real authorities is unverified in CI |
| Sustained throughput | needs a dedicated Linux host | no performance claim is made |
| 2-hour soak | time cost | leak detection is per-test, not project-wide |
| Fuzz targets | not yet written | the DNS wire parser and range parser would benefit most |
| Goroutine-leak assertions project-wide | partial | a few tests assert accounting; `goleak` is not wired into every package |
| mTLS identity binding | feature not implemented | see `SECURITY.md` §4 |
| Multi-geography nodes | only loopback multi-node is tested | cross-network behaviour is unverified |

---

## 6. Running the Tests

```bash
# Everything, with race detection
go test -race ./...

# One component
go test -race ./internal/lb/...

# The full local gate
go build ./... && go vet ./... && go test -race ./...

# One-shot tools against real services (these DO use the network)
go run ./cmd/rift dns query example.com --type A --resolver 1.1.1.1:53
go run ./cmd/rift tls check example.com
```

One-shot CLI tools are the deliberate exception to the no-network rule: their
purpose is to query the real world, and their output is not part of any
automated gate.

---

## 7. Writing a New Test

1. Prefer a real socket over a mock. A fake transport tests the fake.
2. If it needs timing, inject the clock.
3. If it asserts a refusal, assert the **classification** as well as the refusal
   — "it failed" and "it failed as a security denial" are different claims.
4. If it found a bug, leave a comment naming the bug. The comment is what stops
   the regression from returning.