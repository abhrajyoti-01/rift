# ROADMAP.md — RIFT Implementation Roadmap

Status: **Revision A**. Phases are cumulative; each ends with "functional
early" increments (nothing postponed to the end, per the brief). Acceptance
per phase references T-IDs (TESTING_SPEC) and NFR/FR IDs (PRD). "Owner" is
the phase's accountable implementer; all phases assume the 15 docs are
committed and internally consistent before Phase 0 code starts.

---

## Phase 0 — Toolchain, Experiments, CI Rails (1 week equivalent)

**Objective:** make every later decision measurable; no service code.

- Repo rails: Makefile, `.golangci.yml` (depguard enforcing AR-1, `gofmt`/
  `goimports`, `revive`), `gosec` in CI, CI workflow (test + race + bench
  gate), `.gitignore`, issue/PR templates (docs-only repo polish).
- Experiments **E1–E7** (PERFORMANCE_SPEC §3) in `experiments/`: standalone
  `main.go` programs + committed result files in `bench/results/`. Each is a
  real program: E1 transfers 1 GiB through `TCPConn.ReadFrom` and
  `strace -c`-counts syscalls; E3 runs iperf3/latency across loopback,
  host↔WSL2, and a netns pair.
- Docker inside WSL2 provisioned (currently absent on host — verified); compose
  reference topology file written but *not yet exercised* (honestly marked).
- **Fills**: UD-1 (E3), UD-2 (E1), UD-3 (E2), UD-4 (E6) → specs amended in
  the same commits as results.
- **Tests**: experiment programs self-verify (byte hashes, syscall-count
  assertions).
- **Acceptance**: E1–E7 result files committed; spec `[E-n]` cells filled
  from them only; CI green on empty-tree test path; `bench/env` card renders.
- **Risks**: WSL2 kernel gaps (E1/E2 may show no splice/reuseport benefit →
  fallback paths ship, documented); Docker Desktop install friction.
- **Dependencies**: none beyond toolchain.

## Phase 1 — Platform Layer (2 weeks)

**Objective:** the shared substrate all three services build on; compiling,
tested, no placeholders.

- Packages (`internal/platform/`): `errs`, `config` (YAML load/validate/
  redact/diff/watch), `logging`, `lifecycle` (phases, errgroup, signals,
  drain budgets), `health`, `metrics` (registry, bucket sets, label-set
  enforcement test), `httpx` (server factory, middleware, TLS profiles),
  `netx` (listeners, keepalive, `Guard`), `pool`, `ratelimit`, `circuit`,
  `retry`, `testsupport` (Echo, Blackhole, SlowConn, Chaos, goleak main).
- `cmd/rift`: subcommand skeleton, `init`, `config validate`, `config diff`,
  `version` (real: reads build info).
- **Tests**: unit per package; synctest suites for retry/breaker/lifecycle
  budgets; label-set fuzz; redaction golden; goleak TestMain wired.
  Benchmarks: `BenchmarkPick` stubs' substrates (pool, ratelimit shards).
- **Acceptance**: `go vet ./... && go test -race ./...` green; `rift init |
  rift config validate` round-trips (T-92/95); goleak clean; `platform` has
  zero subsystem imports (depguard green).
- **Risks**: over-engineering (guard: platform packages ship only what a
  service will use within one phase — no speculative `buffer`/`dns`/`tls`
  packages, AR-4).
- **Dependencies**: Phase 0 CI rails.

## Phase 2 — Load Balancer (3 weeks)

**Objective:** functional LB: L4 TCP + L7 HTTP, three pickers, health, limits,
reload, metrics.

- Packages: `lb/model`, `lb/picker`, `lb/health`, `lb/l4`, `lb/l7`, `lb/udp`,
  `lb/control`.
- Features in order: pickers → health → L4 (copy modes both, splice default
  per E1) → L7 (ReverseProxy seams, retry table, XFF) → limits/rate-limit →
  admin API + SIGHUP reload → UDP sessions (late in phase, after L4 soak).
- **Tests**: T-12..T-49 progressively; fuzz on Range n/a here; χ² and
  interleave distribution tests; reload-under-load (G8).
- **Benchmarks**: NFR-1/2/12 asserted; scenario runner v0 (closed loop) for
  `lb.http.small`, `lb.l4.bulk`; first Nginx parity run.
- **Acceptance**: all LB v1 FRs green (T-trace report); NFR-1/2/12 recorded;
  reload-under-load zero drops; goleak clean across all fault tests.
- **Risks**: WRR mutex contention (measured via G-sweep, sharded variant only
  if superlinear); Windows dev gaps on socket options (Linux CI compensates).
- **Dependencies**: Phase 1 platform.

## Phase 3 — DNS + TLS Monitor (3 weeks)

**Objective:** wire engine, resolver engines, two views, node + hub, TLS
prober, alerts — all real.

- Packages: `dnsmon/wire` (+ fuzz corpus), `dnsmon/resolver`, `dnsmon/probe`,
  `dnsmon/model`, `dnsmon/node`, `dnsmon/hub`, `tlsmon/model`, `tlsmon/probe`.
- Features in order: wire (encode/decode/fuzz first — parser hardening before
  any socket touches it) → resolver engine → recursive view → authoritative
  view → node ring/shipper → hub ingest/window/segments → classifier →
  alerts → TLS prober → one-shot CLI tools.
- **Tests**: T-51..T-65 progressively; `dig` equivalence (network-gated);
  two-node netns integration (T-61); spool round-trip; alert boundaries via
  synctest.
- **Benchmarks**: NFR-10 asserted; `dns.throughput`, `dns.flood` scenarios.
- **Acceptance**: all DNS/TLS v1 FRs green; fuzz corpus committed and
  nightly-clean; NFR-9 flood test green; hub replay round-trips (T-62).
- **Risks**: public-resolver variability in CI (network-gated tests opt-in
  with local unbound-in-WSL2 fallback resolver for determinism); NS churn
  test realism.
- **Dependencies**: Phase 1 platform; Phase 2's `netx.Guard` hardening
  (shared).

## Phase 4 — Media Server (2 weeks)

**Objective:** range server at measured bandwidth; playersim instrument.

- Packages: `media/model` (Range parse + fuzz, Asset/Index types),
  `media/server` (handler, admission, index builder); `bench/playersim`.
- Features in order: range parser (fuzzed first) → GET/HEAD + 206/416 →
  sendfile path + fadvise → buffered mode (E5 decides default) → limits/
  admission → index → metrics → playersim.
- **Tests**: T-81..T-88b; traversal suite; 10-GiB memory assertion; seek
  storm; delete-mid-stream.
- **Benchmarks**: `media.4k.x10`, `media.lossless`, `media.capacity-ramp`;
  NFR-7/8 recorded; playersim startup/stall numbers separate from server
  metrics.
- **Acceptance**: all media v1 FRs green; NFR-7 ratio vs measured ceilings
  recorded; E5 result committed with shipped default justified.
- **Risks**: WSL2 disk path distortion (fio ceiling + card honesty, AD-11);
  sparse-file fixtures vs real fragmentation (both used: sparse for memory
  tests, real files for throughput).
- **Dependencies**: Phase 1 platform; Phase 2's loadgen scaffolding.

## Phase 5 — Bench, Baselines, Reports, Docs (2 weeks)

**Objective:** the measurement machine complete; v1 claims evidenced.

- Packages: `bench/env`, `bench/loadgen` (open-loop scheduling + HDR, E6
  decided), `bench/harness` (run/compare/report/trace), dashboards
  (`deploy/grafana/`), Prometheus rules, nginx baseline config set.
- Scenario definitions: all named scenarios from PERFORMANCE_SPEC §4
  versioned in `bench/scenarios/`.
- Soak/stress/parity: 2 h soak assertions; Nginx parity with alternated runs;
  wrk2 cross-check; report regeneration byte-identical (G6).
- **Acceptance**: every NFR has a measured value committed in
  `bench/results/`; report regenerates from samples; traceability report
  shows every v1 FR/NFR → green T-ID; README quickstart reproduces from
  clean clone; dashboards validate against live registries.
- **Risks**: tolerance-band calibration (first baselines need care to avoid
  either always-green or always-red bands); wrk2 availability in WSL2.
- **Dependencies**: Phases 2–4.

## Phase 6+ — Milestones (post-v1, no placeholders, each fully designed before start)

| Milestone | Content | Precondition |
|---|---|---|
| M1 | Geographic nodes (real second-site node) | Two-network deployment available |
| M2 | DNSSEC validation; CAA/SRV/PTR; DoH/DoT | Wire engine soak |
| M3 | Consistent-hash picker; passive health | LB distributions green |
| M4 | HLS/CMAF packaging (external FFmpeg, real segments) | Media throughput story proven |
| M5 | Webhook alert delivery; Grafana OnCall | Alert conditions stable |
| M6 | TLS 1.0/1.1 opt-in scanning | Compliance use case |
| M7 | `madvise`/readahead tuning, O_DIRECT (E5 follow-up) | Profile justifies |
| M7b | `io_uring` exploration | Real Linux host (AD-12) |
| M8 | Media token auth | Non-LAN exposure |
| M9 | Hub query API v2 (aggregations) | Real analytical need |
| M10 | OTel tracing (AD-13 reversal condition) | Cross-network spans justified |

## Definition of v1 Complete (PRD §9 restated operationally)

1. All v1 FRs trace to green T-IDs in the traceability report.
2. All NFRs have measured values (committed result files), no `[E-n]` cells
   empty.
3. `bench/baselines/perf-baseline.json` bands calibrated; CI gate live.
4. One command regenerates each component report from committed samples.
5. README quickstart reproduces from a clean clone in WSL2.
6. The forbidden-claims grep (PERFORMANCE_SPEC §5a) passes over all docs.

Until all six hold, README states exactly which claims are evidenced and
which remain open — no "production ready" claim before the evidence.
