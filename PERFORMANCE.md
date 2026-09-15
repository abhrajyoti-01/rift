# RIFT Performance Engineering

> **Read this first.** This document contains **no performance numbers**, because
> none have been measured on an appropriate host yet. Every target below is
> marked `[unmeasured]`. That is deliberate: publishing an unmeasured number is
> the failure mode this project exists to avoid.

---

## 1. The Rule

No optimization enters the codebase without a measurement, and no number is
published without its environment. Concretely:

- Every benchmark result carries an **environment card** (`rift bench env`).
- A report **cannot be generated** without one.
- Where a target is unmeasured, this document says `[unmeasured]` rather than
  leaving a plausible-looking blank filled in.

---

## 2. Why There Are No Numbers Yet

The development host is unsuitable for performance claims, and the tooling says
so out loud:

```
$ rift bench env
Environment card (generated 2026-09-15T17:41:42Z)
  os/arch        windows/amd64
  go             go1.25.5 (gc)
  cpus           12 (GOMAXPROCS 12)
  wsl2           false
  note           Windows host: no performance claims may be derived from this card
```

Three reasons a number measured here would mislead:

1. **Different syscall paths.** The zero-copy paths that matter on the target
   (Unix `splice`, `sendfile`) do not exist on Windows. A throughput measured
   here would not predict a Linux server.
2. **Windows TCP stack behaviour** differs from Linux in ways that dominate
   connection-heavy workloads.
3. **Timer granularity and scheduling** differ enough to distort tail latency,
   which is exactly the range this project cares about.

The graph below shows the intended measurement topology, not a measured result:

```
        ┌──────────────────────────┐
        │   Linux host (barrier)   │
        │  ┌────────┐ ┌─────────┐  │
        │  │ rift   │ │ backends│  │
        │  │ lb     │─│ echo/   │  │
        │  └────────┘ │ http    │  │
        │  ┌────────┐ └─────────┘  │
        │  │ rift   │              │
        │  │ media  │◄── loadgen ──┼─── same host, loopback + veth
        │  └────────┘              │
        └──────────────────────────┘
```

---

## 3. What Has Been Verified (Functionally)

These are behaviour guarantees that are asserted in tests today. They are not
throughput claims.

| Property | Status | Test |
|---|---|---|
| Pick path performs **zero heap allocations** | ✅ asserted at 64 backends | `TestPickerAllocFree` |
| Pick latency is sub-microsecond on this host | ◐ observed, not a publishable number | `BenchmarkPick1000` |
| Media memory per stream is independent of file size | ✅ | bounded readahead; `TestMediaEmptyFileAnyRangeIs416` |
| DNS ring memory is bounded regardless of observation rate | ✅ | `TestRingMemoryIsBounded` |
| Hub window is bounded per key | ✅ | `TestWindowBoundsMemory` |
| Rate limiter cardinality is bounded under a 10k-key flood | ✅ | `TestBoundedCardinalityUnderFlood` |
| UDP session table is bounded | ✅ | `TestUDPSessionTableCapDrops` |
| Admission overflow is refused, never queued | ✅ | `TestL4AdmissionRefusesOverMax`, `TestMediaAdmissionLimits` |

These matter more than a headline number: a bounded system under hostile input
is the precondition for any throughput number being meaningful.

---

## 4. Benchmark Methodology

Defined now so that the first measurement is comparable to the hundredth.

### 4.1 Environment requirements

| Item | Requirement |
|---|---|
| OS | Linux (bare metal preferred; WSL2 acceptable only for relative comparisons) |
| CPU | ≥ 8 physical cores, governor set to `performance` |
| NIC | ≥ 1 GbE loopback or better for media work |
| Disk | SSD/NVMe for media throughput |
| Isolation | no other load; `GOMAXPROCS` matched to allotted cores |

The environment card records OS, kernel, CPU count, `GOMAXPROCS`, cgroup limits,
WSL2 status, and build identity. A measurement is not comparable without them.

### 4.2 Statistical practice

| Concern | Practice |
|---|---|
| Warm-up | ≥ 15% of duration or 30 s, discarded and reported separately |
| Duration | ≥ 180 s per steady-state point |
| Repetition | ≥ 3 runs per point |
| Percentiles | nearest-rank over raw samples; never interpolate an unobserved value |
| Coordinated omission | **open-loop scenarios only** for latency; intended start recorded per request |
| Reporting | p50/p95/p99 plus max; means are reported but never as the headline |
| Comparison | both arms on the same host, same fixture, same client, alternating order |

### 4.3 Why open-loop matters

In a closed-loop benchmark, a request is never issued while another is
outstanding. When the server stalls, the generator simply waits — and the
resulting latency percentiles look excellent precisely when the system is
failing. RIFT's open-loop mode schedules each request at its **intended**
deadline regardless of completion, and records that intended time alongside the
actual one in every raw sample, so the distortion is measurable after the fact.

```json
{"intended_start":"...T10:00:00.000Z","actual_start":"...T10:00:00.041Z",
 "latency_ns":2100000,"status":200,"bytes":1234,"conn_id":7}
```

### 4.4 Ceilings, not isolated numbers

Media throughput is only interpretable relative to the host's own ceilings, so
each environment measures them first:

| Ceiling | Tool | Establishes |
|---|---|---|
| Network | `iperf3` | maximum achievable transfer rate |
| Disk | `fio` (sequential read) | maximum achievable read rate |

A result is then reported as a **ratio against `min(disk, NIC)`**, e.g.
"0.83 × measured ceiling", which remains meaningful when the host changes. An
absolute "1.9 GB/s" does not survive a host change and invites false comparison.

---

## 5. Scenarios

Versioned JSON under `bench/scenarios/`. The current set:

| Scenario | Shape | What it measures |
|---|---|---|
| `lb.http.small.1k` | open loop, 2000 req/s, 64 conns | L7 request path at fixed arrival rate |
| `lb.http.closed.200` | closed loop, 200 concurrent | L7 maximum throughput and connection reuse |
| `media.range.4k` | open loop, 10 streams | media streaming with range requests |

Planned additions:

| Scenario | Purpose |
|---|---|
| `lb.l4.bulk` | raw TCP forwarding throughput |
| `lb.fault.backend-rst` | failover behaviour under backend death |
| `lb.fault.reload` | reload under load; assert zero dropped connections |
| `dns.throughput` | resolver engine query rate |
| `dns.flood.resolver-timeout` | behaviour when resolvers stop answering |
| `media.capacity-ramp` | find the concurrent-stream cliff |

---

## 6. Targets (all unmeasured)

| # | Component | Metric | Target | Status |
|---|---|---|---|---|
| P-1 | LB L4 | throughput | ≥ 0.9 × measured NIC ceiling | `[unmeasured]` |
| P-2 | LB L4 | p99 added latency | ≤ 100 µs | `[unmeasured]` |
| P-3 | LB L7 | overhead vs direct backend | ≤ 20% at p99 | `[unmeasured]` |
| P-4 | LB L7 | pick cost | 0 allocs, < 1 µs p99 | ✅ allocs asserted; latency unmeasured |
| P-5 | LB | reload under load | 0 dropped connections | ◐ asserted functionally; not under load |
| P-6 | Media | sustained throughput | ≥ 0.8 × min(disk, NIC) at 10 streams | `[unmeasured]` |
| P-7 | Media | memory per stream | ≤ readahead + socket buffers | ✅ structurally guaranteed |
| P-8 | Media | startup latency | p50 ≤ 500 ms on LAN | `[unmeasured]` |
| P-9 | DNS node | memory ceiling | ≤ 256 MiB at any observation rate | ✅ ring-bounded by construction |
| P-10 | DNS | hub ingest | ≥ 10k observations/s | `[unmeasured]` |
| P-11 | All | 2-hour soak | RSS growth < 2% | `[unmeasured]` |
| P-12 | All | descriptor usage | < 70% of limit at nominal load | `[unmeasured]` |

**Until a row loses the `[unmeasured]` marker, it is not a claim.**

---

## 7. Measuring, Eventually

The sequence, in order, once a suitable host exists:

1. `rift bench env` — capture and commit the card.
2. Measure ceilings with `iperf3` and `fio`; record them in the card.
3. Run each scenario ≥ 3 times; store raw samples under `bench/results/`.
4. `rift bench report --samples <dir>` — regenerate the report from stored
   samples (this must work without re-running, or the samples were not the
   record).
5. Establish a baseline for tolerance-band comparison.
6. Only then, update §6 with measured values and the card hash they came from.

Step 6 is the only step that produces a claim.

---

## 8. Profiling Protocol

When a number is worse than expected, profile before changing code:

| Question | Tool |
|---|---|
| Where is CPU going? | `go test -bench ... -cpuprofile` |
| Where are allocations coming from? | `-memprofile`, `-benchmem` |
| Are we blocked on locks? | `-blockprofile`, `-mutexprofile` |
| Where are syscalls going? | `strace -c` (Linux) |
| Is the scheduler the problem? | `go tool trace` |

Change one thing at a time, with a benchmark before and after in the commit
message. An optimization without a before-number is a guess.