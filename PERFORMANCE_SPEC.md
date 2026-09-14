# PERFORMANCE_SPEC.md — RIFT Performance Engineering

Status: **Revision A**. Owning requirements: NFR-1..NFR-13, G2, G3, G6. The
rule this document enforces project-wide: **no optimization without a
measurement** (AR-5). Where a target is `[E-n]`-marked, the experiment sets
it; **all number cells are deliberately empty** — filling them without the
named methodology is a spec violation, not a TODO.

---

## 1. Governance

- Every perf-relevant commit message carries a before/after `benchstat`
  table; result files land in `bench/results/<scenario>.txt`.
- Optimization ladder, in order: measure → profile → fix the top frame →
  re-measure. Skipping steps is how cargo cults start.
- A/B claims require non-overlapping bootstrap 95% CIs across ≥3 runs, or an
  explicit "CIs overlap — no claim" statement (§5).
- Any perf work must state its expected percentile target before measuring
  (declare-then-measure, not fish-for-a-win).
- WSL2 is a first-class documented constraint (AD-11): every absolute number
  carries the environment card; portability comes from ratios vs measured
  ceilings.

## 2. Environments

| Role | Environment | Notes |
|---|---|---|
| Development | Windows 11 + Go 1.25.5 | correctness work only; no perf claims from Windows |
| Reference benchmark | WSL2 Ubuntu (default distro, verified) | E3 quantifies fidelity vs bare Linux; ratios quoted |
| Baseline | Nginx (same host, same fixtures, same client) | §6 parity table |
| Target deployment | Linux server, 1–2 sockets, ≥8 cores, NVMe | ratio targets carry over; absolutes re-measured per host |

## 3. Phase 0 Experiments (gating, run before perf phases)

| ID | Question | Method | Gates |
|---|---|---|---|
| E1 | Does `TCPConn.ReadFrom` reach `splice()` on WSL2? | 1 GiB L4 transfer, `strace -c -e splice,sendfile,read,write`, bytes/syscall | L4/media copy path default (UD-2) |
| E2 | accept-loop scaling: single listener vs SO_REUSEPORT shards | 8k conn/s, shards 1..8, `accept4` counts + setup p99 | whether reuseport ships (UD-3) |
| E3 | WSL2 network fidelity | iperf3/latency/p99 across loopback, host↔WSL2, netns pair vs published bare-Linux expectations | absolute vs ratio-only targets (UD-1) |
| E4 | DNS wire engine cost+correctness | bench encode/decode allocs; `dig +ttldata` equivalence | NFR-10, FR-25 |
| E5 | media IO mode | io.Copy+SectionReader vs explicit readahead at 1/4/16 streams | shipped default mode |
| E6 | histogram dependency overhead | hdrhistogram-go vs fixed-bucket in-house at loadgen rates | UD-4, keeps or drops the dependency |
| E7 | worker-pool-in-front-of-Read cost | one-goroutine-per-conn vs pool+channel at conn rates | documents the pool anti-pattern cost |

Exit criterion: each E gets a committed `bench/results/E<n>-*.txt` with
numbers, and specs' `[E-n]` cells are filled **from those files only**.
Contradictions between plan and measurement resolve in favour of the
measurement, with a spec amendment commit.

## 3a. Optimization Candidate Register (measured in Phase 1+)

| ID | Candidate | Expected mechanism | Decide by |
|---|---|---|---|
| OC-1 | `sync.Pool` buffer reuse in L4 copy path | GC pressure ↓ | E1-resulting profile + alloc bench |
| OC-2 | header-map pre-sizing in L7 | alloc ↓ | L7 A/B plateau |
| OC-2b | `httptrace` closure elimination on hot path | alloc/req ↓ | L7 alloc bench |
| OC-3 | WRR sharded counters | lock contention ↓ | G=1..64 sweep |
| OC-3b | per-backend response-buffer pooling | alloc ↓ | L7 A/B |
| OC-4 | `posix_fadvise` sequential on media fds | readahead thrash ↓ on HDD | fio+scenario on mixed media |
| OC-5 | readahead size sweep (64k/256k/1M/4M) | syscall/byte ↓ | E5 extension |
| OC-5b | `SO_REUSEPORT` shards | accept queue contention ↓ | E2 |
| OC-6 | `GOMEMLIMIT` + soft cap | GC p99 ↓ near ceiling | soak at capacity |
| OC-6b | pointer-density reduction in Observation slices | scan cost ↓ | GC p99 in hub flood |
| OC-7 | heap-oblivious encoder (write DNS names without fmt) | alloc ↓ | wire bench |
| OC-7b | decode into caller-provided structures | alloc ↓ | wire bench |
| OC-8 | interval-based (amortized) health re-eval | churn ↓ | health flap test |

None of these ships without its measurement; the register is public in
`bench/results/optimizations.md` with commit-hash linkage.

## 4. KPIs and Instruments (per component)

### Load balancer

| KPI | Instrument | Target |
|---|---|---|
| requests/s (L7) | scenario `lb.http.*`, both own loadgen + wrk2 cross-check | [E3] |
| connections/s (L4) | scenario `lb.l4.churn` | [E3] |
| concurrent connections | `rift_lb_active_connections` + harness | 10k sustained, NFR-5 margins |
| throughput (L4 bulk) | `lb.l4.bulk` byte rate | [E3] |
| latency p50/p95/p99 | HDR histograms, open-loop | [E3] |
| CPU per 1k rps | mpstat in scenario | [E3] |
| memory RSS | card + sampler | NFR-4 |
| failed connections | `rift_lb_connections_total{state="failed"}` | < 0.1% steady state |
| backend distribution | `rift_lb_backend_picks_total` χ² | p > 0.01 (T-21) |

### DNS monitor

| KPI | Instrument | Target |
|---|---|---|
| queries/s per node | scenario `dns.throughput` | [E3] |
| resolver concurrency | `rift_dns_...in_flight` | ≤ configured cap, hard |
| DNS latency (p50/p95) | `rift_dns_query_duration_seconds` | resolver-dependent, reported not targeted |
| successful query % | `rift_dns_queries_total` rcode split | ≥ 95% over 24 h real resolvers (network-gated) |
| timeout % | `rift_dns_timeouts_total` | resolver-dependent, reported |
| aggregation latency | `rift_hub_ingest_batch_size` + window age | p95 < 5 s (batch age trigger) |
| memory | NFR-9 flood test | ≤ 256 MiB hard |

### Media server

| KPI | Instrument | Target |
|---|---|---|
| MB/s aggregate | `media.4k.x10`, `media.capacity-ramp` vs card ceilings | ≥ 0.8 × min(disk, NIC) [E3] |
| startup latency | `playersim` (client-side) + server first-byte (separately) | p50 ≤ 500 ms LAN [E3] |
| concurrent streams at rating | capacity-ramp cliff detection | 10×4K + 50×lossless [E3] |
| CPU / memory | mpstat + sampler | §MEDIA_SPEC targets |
| rebuffer events | `playersim` stalls | p95 ≤ 1/h at rating [E3] |
| disk/network utilization | card ceilings + `ss -ti` | ≥ 80% at capacity |

**Instrument separation rule**: server-side metrics never quoted as client
experience; playersim results never merged with server-side numbers in the
same table row (P3, FR-46).

## 5. Statistical Methodology

- Warm-up ≥ 15% duration or 30 s (discarded, reported separately).
- Steady-state window ≥ 180 s per point, ≥ 3 runs per point, alternated arms
  (A/B/A/B) to defeat thermal and drift.
- Percentiles from HDR log-bucketed histograms (E6-decided implementation);
  open-loop (absolute-time) scheduling for latency scenarios — the
  coordinated-omission fix.
- p99 reporting: bootstrap 95% CI over runs; no CI ⇒ no percentile claim.
- Distributions per-quantile; means are reported only alongside p50 and are
  never the headline. Heavy-tail honesty.
- Benchmark comparisons use `benchstat` on ≥10 count; a "win" outside CI is
  not a win.
- Raw samples NDJSON committed per scenario run; the report is regenerable
  from samples without re-running (G6).

## 5a. Forbidden Claims

- Any number without its environment card.
- "Zero-copy" unless E1 shows splice reached on the reference host.
- "Zero buffering" — banned phrase project-wide (PRD stance).
- "Scales infinitely"/"production-ready" before PRD §9 holds.
- Means without percentiles; latency without CI.
- Rebuffer claims from server-side metrics.
- Any [E-n] cell filled by prediction.

## 5b. Unmeasured ⇒ Unclaimed

An empty target cell is a *placeholder-free statement of scope*: it says "not
measured yet", which is a fact, not a lack. Filling it without E-n evidence
is prohibited. Roadmap phases carry the acceptance item "fill the E-n cells
from committed result files".

## 6. Baseline Comparison — Nginx Parity

Baseline config versioned at `bench/baselines/nginx/`. Parity knobs (both
arms identical except the proxy):

| Nginx | RIFT |
|---|---|
| `worker_processes` | `GOMAXPROCS` |
| `worker_connections` | `max_conns` |
| `keepalive_requests` | `MaxIdleConnsPerHost` reuse policy |
| `upstream least_conn` | `least_connections` picker |
| `proxy_buffering on/off` | run both arms both ways — buffering parity matters |
| `listen reuseport` | `SO_REUSEPORT` flag, both arms |
| TLS certs/ciphers | identical material |
| `access_log off` in both arms | log overhead excluded from the proxy comparison |

Run order alternates; same fixtures, same client (our loadgen), same host;
wrk2 cross-check on the plateau. Parity gaps (nginx-only knobs) are rows in
the report with "no RIFT equivalent" — not silently dropped.

## 7. CI Regression Gate

`rift bench compare` against `bench/baselines/perf-baseline.json` with
per-metric tolerance bands (e.g. allocs/op ±0, p99 ±15%, rps ±10%):
out-of-band → build fails; improvement > band → baseline refresh PR (human
review) — prevents both silent regressions and silent drift of baselines.

## 8. Profiling Protocol

`make profile-lb`: CPU 30 s, heap, goroutine, block, mutex (contention rate
10–100× nominal — low-rate contention is invisible), `go tool trace` on
goroutine anomalies. Profiles land in `bench/results/profiles/<commit>/`. A
frame-by-frame account accompanies every optimization commit (AR-5): the
top-5 profile frames before and after, in the commit message.

## 8a. Environment Card (full definition in `bench/env`)

`rift bench env` output committed per report: OS/kernel/WSL2 build, CPU
model, cores, governor, `GOMAXPROCS`, cgroup v2 limits, NIC + MTU, fs + mount
options, `ulimit -n`, THP, mitigations, measured iperf3/fio ceilings, git
commit, `rift_build_info`. Report generation refuses to run without a fresh
card (T-98). The card is the unit of reproducibility (G6) — a report is
regenerated from samples + card, never from memory.

## 9. Optimization Register Discipline

`bench/results/optimizations.md` logs every OC entry: candidate, hypothesis,
profile evidence, benchstat before/after, decision (shipped/rejected/
deferred), commit. Rejected candidates stay in the register — negative
results are results (they prevent re-litigating the same idea later, which is
the whole point of writing them down).

## 10. Acceptance

- All E1–E7 committed with numbers; spec cells filled from them only.
- NFR-1/2/10/12 asserted green in CI (alloc budgets, pick budget, wire
  budgets, L7 overhead).
- Nginx parity report exists with alternated runs and CI overlap statements.
- Soak assertions (NFR-4/5/6) green weekly.
- No Forbidden Claims (§5a) anywhere in the tree (CI grep on docs).
- Optimization register current: every OC row has evidence links.
