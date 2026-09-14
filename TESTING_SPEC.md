# TESTING_SPEC.md — RIFT Testing Strategy

Status: **Revision A**. Owning requirement: G1..G8 verification, NFR-13.
Every T-nn ID here is a real test that exists in the tree; specs reference
these IDs and nothing else. Test code follows the same bar as production
code (AR rules apply; a flaky test is a bug in the test or the code — never
"timing-tuned" to pass).

---

## 1. Test Taxonomy and Gates

| Class | What | Gate |
|---|---|---|
| Unit | pure logic: parsers, pickers, classifiers, policies | every commit, `-race` |
| Deterministic-time | retry/breaker/classifier via `testing/synctest` | every commit |
| Integration | real sockets over loopback/netns | every commit (fast subset), nightly (full) |
| Fault injection | backends RST/slow/blackhole/disk-full | nightly |
| Load/stress | scenario runner at target rates | nightly + on demand |
| Soak | 2 h fixed load, RSS/goroutine regression assertions | weekly + release |
| Security red-line | SSRF, open-proxy, traversal, mTLS, redaction | every commit |
| Fuzz | DNS wire, Range, config | nightly 10 min; release: 30 min |
| Benchmark | `testing.B` with alloc asserts | every commit (`-race` off, `-count=10` via CI) |

Platform support lives in `platform/testsupport`: `Echo` (real-socket TCP
echo backend), `Blackhole` (accepts, never responds), `SlowConn` (configurable
delay per op), `Chaos` (scripted fault schedule: RST at N, partial write,
half-close, EMFILE simulation via FD limit), `RstServer` (accepts then RSTs),
goleak `TestMain` helpers, bench fixtures (sparse files for media, corpus
builders).

## 2. Matrix by Component

### LB (T-1x..T-4x)

| ID | Test | Verifies |
|---|---|---|
| T-12 | Byte-equivalence soak: 10 GiB across pickers × copy modes, hashes match | G1 |
| T-14 | Half-close: client FIN → response drains fully | FR-10 semantics |
| T-15 | Deadlock scan under load: no goroutine stuck > deadline × 2 | G5 |
| T-21 | χ² pick distribution (RR), exact interleave (WRR 5,3,1), min-inflight (least-conn) | FR-13 |
| T-22 | Pick benchmark at 4/64/1024 backends × G=1..64: 0 allocs, p99 < 1 µs | NFR-2 |
| T-31 | Health rise/fall hysteresis under scripted flapping | FR-14 |
| T-32 | All-backends-down → 502, readyz=false, admin alive | G4 posture |
| T-33 | Conn counter teardown on every error path (leak scan) | G5 |
| T-34 | No error-string matching in retry paths (CI grep + synctest behavior test) | NFR-11 |
| T-35 | All outbound dials through Guard (depguard + grep) | NFR-11 |
| T-36 | Open-proxy refusal: absolute URI/Host routing never reaches client-chosen host | NFR-11 |
| T-37 | CL+TE request rejected pre-forward | SECURITY §4 |
| T-38 | XFF spoof handling under `trusted_proxies` | FR-20 |
| T-39 | Slowloris culled within deadline × 1.5 | FR-21 |
| T-40 | `max_conns` refusal before EMFILE (FD-margin test) | NFR-5 |
| T-41 | Reload-under-load: zero reload-attributable resets, old config serves until swap | G8, NFR-3 |
| T-42 | UDP session table: affinity, flood cap, sweep, counters | FR-11 |
| T-43 | UDP re-pick on backend-unhealthy mid-session | FR-11 |
| T-44 | Retry safety: POST never retried post-write (scripted fault: RST after first byte) | FR-17 |
| T-45 | Rate-limit bypass: spoofed sources, bursts, pipelining | FR-18 |
| T-46 | Oversized body/header early refusal | FR-18/21 |
| T-47 | TLS floor enforcement + weak-cipher refusal | SECURITY §3.1 |
| T-48 | Unknown method → 501 | FR-17 posture |
| T-49 | RFC 7239 grammar on emit | FR-20 |

### DNS/TLS monitor (T-5x, T-7x)

| ID | Test | Verifies |
|---|---|---|
| T-50 | SSRF suite: Guard deny-set on all monitored-target dials (dnsmon NS resolution + tlsmon targets), every deny rule with positive and negative cases | NFR-11, SECURITY §2 |
| T-51 | No "propagated worldwide" verdict anywhere (enum + alert vocab grep) | G4 |
| T-52 | Wire: encode/decode round-trip all types, TTL preserved | FR-25 |
| T-53 | Fuzz: malformed, pointer-loop, oversized, truncated (corpus committed) | NFR-13 |
| T-54 | Transaction matching: stray/late/spoofed responses ignored | SECURITY §3.2 |
| T-55 | Hub mTLS enforcement: unsigned/foreign-cert batch rejected | FR-30 |
| T-56 | Ingest flood → 429 + ring-bounded memory | NFR-9 |
| T-57 | TLS finding matrix: expired, wrong-host, self-signed, incomplete chain, weak key/sig, old-TLS — each yields its exact Finding code | FR-29 |
| T-57b | `verify: strict` mode fails (does not report-and-continue) | FR-29 |
| T-58 | Classifier fixtures: all six states + convergence-window boundary | FR-28 |
| T-59 | NS churn: old+new NS queried, freshest reference wins | FR-26 |
| T-61 | Two-node integration over real netns: ingest, dedupe, replay | FR-30/33 |
| T-62 | Spool round-trip during hub outage; drop-oldest counter | FR-30 |
| T-63 | Alert thresholds fire exactly at boundary (synctest) | FR-31 |
| T-64 | `dig` equivalence: answers + TTLs across ≥3 resolvers (network-gated) | FR-25 |
| T-65 | TLS handshake-failure classification matrix | FR-29 |
| T-71 | Alert flapping hysteresis: divergence oscillation inside convergence window does not flap alerts | SECURITY §3.5 |
| T-72 | Hub query API abuse: pagination caps enforced, closed target set, unbounded-query refusal | SECURITY §3.5 |
| T-73 | DNS abuse: non-configured targets refused; per-resolver concurrency cap under flood; query budget enforced | SECURITY §3.2 |
| T-74 | QName sanitization: 253-char cap; hostile qname cannot inject fields/values into logs | SECURITY §3.2 |
| T-75 | Replay: duplicate batch deduped on idempotency key; at-least-once invariant | SECURITY §3.2 |

### Admin plane & cross-cutting (T-6x)

| ID | Test | Verifies |
|---|---|---|
| T-66 | Log-field injection/cardinality abuse: hostile values cannot introduce fields or unbounded values into logs (closed field set + caps) | SECURITY §3.4 |
| T-67 | Unauthorized remote reload: no token / wrong token → 401/403; `allow_remote` unset + routable bind → refuse to start | SECURITY §3.4 |
| T-69 | Admin exposure: port-scan data-plane listeners — `/metrics`,`/healthz`,`/readyz` absent; admin loopback default; remote requires opt-in | FR-4, SECURITY §3.4 |
| T-70 | Refuse-to-start: routable admin bind without `allow_remote: true` exits 3 with explicit message | SECURITY §3.4 |

(Secret-redaction golden tests live at T-93; log-field sanitization for DNS
qnames at T-74.)

### Media (T-8x)

| ID | Test | Verifies |
|---|---|---|
| T-81 | RFC 7233 matrix: every row of MEDIA_SERVER_SPEC §2 | FR-35 |
| T-82 | Path traversal suite: `..`, absolute, backslash, NUL, symlink escape | NFR-11 |
| T-82b | Multi-range ignored → 200, counted | FR-35 |
| T-83 | 10-GiB sparse fixture: RSS delta ≤ readahead + socket buffers | FR-36 |
| T-84 | Seek storm: rapid random ranges under per-client limit | FR-38 |
| T-85 | Write-timeout culling of stalled client; counter | FR-38 |
| T-86 | Admissions 429 + Retry-After at limits | FR-38 |
| T-87 | Index build 100k < 2 s; atomic swap mid-request | NFR-8, FR-40 |
| T-88 | Delete-mid-stream: clean close, no corruption signal | MEDIA §9 |
| T-88b | ETag strong-match If-Range | FR-35 |
| T-89 | Existence oracle: indexed vs non-indexed names produce uniform error surface; no size/timing leak of non-indexed assets | SECURITY §3.3 |
| T-89b | Oversized/oversized-count headers → 431/400 immediate; no resource growth | SECURITY §3.3 |

### Platform, API, CLI, bench (T-9x)

| ID | Test | Verifies |
|---|---|---|
| T-91 | Lifecycle: every phase transition observable; shutdown budgets; exit codes 0/1/4 | FR-3 |
| T-92 | Config: unknown-field rejection, field-path errors, env overrides, typo matrix | FR-2 |
| T-93 | Redaction golden tests (diff, snapshot, errors) | NFR-11 |
| T-94 | goleak TestMain across all services: shutdown to baseline ±0 | G5, NFR-6 |
| T-95 | CLI: subcommand matrix, exit codes (0/1/2/3/4/124), `rift init` round-trip | FR-1 |
| T-96 | Playersim model vs scripted server: startup/stall/underrun computed correctly (analytic cases) | FR-48 |
| T-97 | Bench harness self-test: identical config twice → within tolerance bands | FR-45 |
| T-98 | Report refusal without environment card | FR-46 |
| T-99 | API conformance: all API_SPEC endpoints, status codes, pagination caps | API_SPEC |

## 3. Benchmark-Assertions in Tests

Allocation/latency budgets (TECHNICAL_SPEC §11) are asserted in the same test
binary: `testing.AllocsPerRun` for pick/encode/decode/ingest; benchmark
functions additionally guard with `b.ReportAllocs()` + `b.Run` param sweeps.
CI runs `go test -run '^$' -bench . -benchmem -count=10` + `benchstat` gate
against committed baselines with tolerance bands (PERFORMANCE_SPEC §7) — a
regression beyond band fails the build, an improvement beyond band prompts
re-baselining (not silent drift).

## 3a. Soak and Stress Protocol

Soak (weekly): 2 h at `lb.http.small.1k` + `media.4k.x10` simultaneously on the
reference host; assertions: RSS growth < 2% (NFR-4), goroutines = baseline ±5%
at end, FDs < 70% rlimit, zero panics, p99 drift < 20% hour-over-hour. Stress
(nightly): capacity-ramp to cliff, `lb.fault.backend-rst` (50% backends dying
on schedule), `dns.flood.resolver-timeout` — assertions are on *classification
correctness and resource bounds*, never on throughput numbers (throughput
claims come only from the benchmark methodology, not stress runs).

## 4. Deterministic Time: `testing/synctest`

Retry backoff, breaker transitions, health rise/fall, alert thresholds,
convergence windows, idle sweeps, TTL decay in the classifier — all driven
under synctest bubbles with zero real sleeps (AR-8). Rule: any component
whose timing is test-relevant takes an injected clock/ticker; synctest tests
policy, integration tests cover sockets (bubbles cannot contain real I/O —
that boundary is what keeps these two classes honest).

## 5. Integration Topology

Nightly integration runs in WSL2 with two network namespaces (`ip netns add`)
joined by a veth pair: node-A ↔ hub in ns0, node-B in ns1 — the two-node LAN
test (T-61) runs over a real wire, not a pipe. Where Windows development
blocks a Linux-only feature, the test carries a `//go:build linux` tag and CI
runs it in WSL2; `SO_REUSEPORT`, `posix_fadvise`, netns, cgroup v2 sampling.

## 6. Fuzzing

Targets + seed corpora committed: `FuzzParseMessage` (dnsmon/wire — corpus:
real digests of real responses + crafted malformed), `FuzzParseRange`
(media), `FuzzLoadConfig` (platform/config, secrets redaction invariant),
`FuzzParseDuration`-style helpers. Success criterion: 10 min nightly × target
no crash, no unbounded memory; every discovered failure adds its input to the
regression corpus.

## 7. Race Detection

`-race` on all unit/integration/security tests every commit (CI). Soak runs
`-race` weekly (race overhead invalidates its throughput numbers — those
runs assert *correctness* only, never quoted as performance). Load/stress
never run with `-race` (they feed the benchmark methodology, which forbids it).

## 7a. Failure Testing as a Discipline

Every fault class is scripted in `platform/testsupport.Chaos`: backend RST
mid-body, partial write then stall, half-close then die, accept-then-silence,
EMFILE simulation, disk-full on segment write, hub cert expiry. A fault test
asserts three things: correct classification (`errs.Class`), correct resource
teardown (no goroutine/FD leak), correct observable behavior (metric +
log line). Fault tests that only assert "no panic" are rejected in review.

## 8. Test-to-Requirement Traceability

CI emits a traceability report (`rift bench trace` internal mode): FR/NFR →
T-ID → last run status. A v1 FR with no green T-ID blocks v1 completion
(PRD §9). The report is a first-class artifact in `bench/results/`, not
documentation.

## 9. Acceptance

- All T-IDs green in nightly; red-lines green every commit.
- Fuzz corpora committed and growing; no open fuzz crashes.
- Traceability report shows every v1 FR/NFR → ≥1 green test.
- Zero flaky tests tolerated: `go test -count=5` clean on changed packages.
