# BENCHMARK_REPORT_TEMPLATE.md — RIFT Benchmark Report

Status: **Revision A**. This is the *form* every report takes; `rift bench
report` fills it from samples + environment card, and **refuses to render
without a card** (FR-46, T-98). A human may fill cells marked `MANUAL` only;
cells marked `[E-n]` are filled from committed experiment files. Numbers
never invented: an unmeasured cell stays `not measured — <reason>` and is
tracked in ROADMAP until measured.

---

# RIFT Benchmark Report: <scenario-id>

## 0. Card (machine-generated, mandatory)

| Field | Value |
|---|---|
| Generated (UTC) | |
| rift build | `rift_build_info` (version, commit, goversion) |
| OS / kernel | |
| WSL2 (yes/no) + Windows build | |
| CPU model / sockets / cores | |
| Governor min–max | |
| GOMAXPROCS / cgroup cpu.max | |
| memory.max / GOMEMLIMIT | |
| NIC / driver / MTU | |
| Disk + fs + mount opts | |
| ulimit -n | |
| THP / mitigations | |
| Measured NIC ceiling (iperf3) | |
| Measured disk ceiling (fio) | |
| Scenario definition file | `bench/scenarios/<id>.json` commit |
| Client tool(s) | rift-loadgen (v…), wrk2/hey (v…) |
| Duration / warm-up / runs | |

*Card source: `rift bench env` output, commit `<hash>`.*

## 1. Executive Summary

Three sentences max: what was measured, the headline finding, the confidence
statement. If CIs overlap, the summary *says so* instead of implying a win.

## 2. Methodology Conformance

Checklist (all mandatory; unchecked ⇒ report is invalid):

- [ ] Warm-up ≥ 15% duration or 30 s, discarded and reported separately
- [ ] Steady-state ≥ 180 s per point, ≥ 3 runs per point
- [ ] Open-loop (absolute-time) scheduling for latency scenarios
- [ ] A/B arms alternated (ABAB) where comparing
- [ ] Percentiles from HDR histograms with bootstrap 95% CIs
- [ ] Same fixtures, client, host, run order for all arms
- [ ] No `-race`, no debug build, `GODEBUG` defaults recorded
- [ ] Raw samples (NDJSON) committed under `bench/results/<run>/`
- [ ] Forbidden-claims audit (PERFORMANCE_SPEC §5a) passed

## 3. Results

### 3.1 Throughput / Rate

| Metric | Run 1 | Run 2 | Run 3 | p50 | CI95 | Unit |
|---|---|---|---|---|---|---|
| requests/sec | | | | | | rps |
| connections/sec | | | | | | conn/s |
| bytes/sec | | | | | | MB/s |

### 3.2 Latency (open-loop)

| Percentile | Value | CI95 | Unit |
|---|---|---|---|
| p50 | | | ms |
| p95 | | | ms |
| p99 | | | ms |
| p99.9 | | | ms |
| max | | | ms |

### 3.3 Resources

| Metric | Value | Unit | Instrument |
|---|---|---|---|
| CPU (user+sys) at plateau | | % | mpstat |
| RSS steady-state | | MiB | card sampler |
| Allocations per op | | allocs/op | testing.B |
| GC p99 pause | | µs | go_gc_duration_seconds |
| FDs at plateau | | count | process_open_fds |
| Goroutines at plateau | | count | go_goroutines |

### 3.4 Component-Specific

LB: backend distribution χ² p-value; pick p50/p99 by algo; reuse ratio;
admission rejections; error split by class.
DNS: per-resolver p50/p95; rcode split; timeout %; truncated→TCP fallback
count; aggregation (batch→window) latency p95.
Media: throughput vs card ceilings (ratio — the headline form); playersim
startup p50/p95; stalls per client-hour; send-stall seconds; readahead hit
ratio. **Server-side and client-side rows are separate and labeled** — never
merged (P3).

## 4. Comparison (if applicable)

| Arm | Metric | Value | CI95 | Δ vs baseline | CI overlap? |
|---|---|---|---|---|---|
| nginx <config> | p99 | | | — | |
| rift lb | p99 | | | | |

Rules: Δ without CI overlap is stated as "no significant difference"; Nginx
parity table (PERFORMANCE_SPEC §6) attached as appendix; run order shown.

## 5. Observations

Profile evidence (top-5 frames before/after for any optimization claim);
anomalies; deviations from scenario definition with reasons; anything the
numbers *don't* support (the anti-overclaim section — mandatory to fill).

## 5a. Anti-Overclaim Audit (mandatory)

Explicit statements: (a) every claim in §1 traces to a table row; (b) which
claims are ratio-only due to WSL2 (AD-11); (c) instrument named for every
number (server counter vs playersim); (d) "zero-copy" only if E1 splice
verified on this host; "zero buffering" never appears; (e) any environmental
caveat (thermal, background load).

## 6. Reproduction

Exact commands, committed artifacts (samples dir, card, scenario file
commit), `rift bench report --samples <dir>` regenerates this file
byte-identical from inputs (G6). One human paragraph: what a skeptical reader
should run to check the weakest claim.

## 7. Verdict

- Regression gate: PASS/FAIL vs `bench/baselines/perf-baseline.json` bands
- Optimization register rows opened/closed (OC IDs)
- Spec cells filled by this report: `[E-n] → value` list
- Unmeasured items remaining (with ROADMAP owner)

## Appendix

Raw percentile tables; environment card JSON; scenario JSON; loadgen client
build info; Nginx config used (if baseline arm); `strace -c` summary if E1-gated
claim present.
