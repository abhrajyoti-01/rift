# RIFT — High-Performance Network Operations Platform

One Go module. One binary. Three real network services sharing one platform
layer, built to be measured, not to look impressive.

> **Status: specification complete; platform security & correctness core
> implemented and green.** The 15 documents are internally consistent and
> normative. Implemented and tested (`-race` clean, hermetic tests):
> `errs` taxonomy, `netx` SSRF Guard (red-line T-50 suite), `pool`
> bounded executor, `ratelimit` sharded limiter with bounded cardinality,
> `circuit` breaker, `retry` policy, `lifecycle` ordered drain engine
> (exit-4 contract), and all three LB pickers (0-alloc asserted, NFR-2).
> Remaining subsystems (LB forwarding, DNS wire, TLS probe, media, config,
> bench) are pinned contracts with honest phase-citing stubs — no fake
> behavior anywhere. Production-grade certification stays gated on ROADMAP
> v1 exit criteria (all FRs green, all NFRs *measured*).

---

## What RIFT Is

| Component | What it really does | Why it exists |
|---|---|---|
| **Load balancer** (`rift lb`) | L4 TCP proxy (splice path + pooled copy), UDP session proxy, L7 HTTP reverse proxy; round-robin / smooth weighted RR / least-connections pickers; active health with rise/fall; per-conn/read/write/idle timeouts; token-bucket rate limits; hot reload with zero dropped connections | Makes the cost of each proxy mechanism individually measurable (per-pick latency, allocs/op, distribution) |
| **DNS + TLS monitor** (`rift dns`, `rift tls`) | Own RFC 1035 wire engine (TTL, rcode, TC, RD=0 — stdlib returns none of these); recursive + authoritative views; bounded-loss node with spool; mTLS hub with JSONL segments; TLS chain/hostname/expiry findings from a closed code set | Honest propagation observation across *observed* resolvers — never a "propagated worldwide" verdict |
| **Media server** (`rift media`) | RFC 7233 single-range serving, strong ETags, If-Range; sendfile + fadvise path with buffered alternative; global/per-client limits; per-stream accounting | High-bandwidth LAN delivery where the bottleneck is disk/NIC, not the server |

Shared platform (one dependency sink, mechanically one-way): `config`
(YAML, validate/redact/diff/reload), `lifecycle` (phases, drain budgets),
`errs` (7-class taxonomy driving log level + metric label + retry
eligibility), `netx` (listeners, SSRF `Guard` at the dial), `pool`,
`ratelimit`, `circuit`, `retry`, `logging`, `metrics`, `health`, `httpx`,
`testsupport` (real-socket fakes, fault injection, goleak).

## Honest-Limits Summary (the parts most projects fake)

- **DNS propagation is observed, not known.** RIFT samples configured
  resolvers from its nodes' vantage points. It distinguishes authoritative
  vs recursive state, records TTL/rcode/latency per observation, and
  **refuses** to emit any "propagated worldwide" state. Client-facing state
  is a documented non-claim.
- **Rebuffer is a client-side event.** Server logs cannot see it. RIFT ships
  `playersim` — a playback-clock client model — and reports server-side
  and client-side numbers separately, never merged.
- **WSL2 distorts absolute numbers.** Throughput/latency claims are ratios
  against same-host measured ceilings (iperf3/fio), with the environment
  card attached. Absolute numbers are re-measured per host.
- **No number is invented.** Every performance target cell is empty until
  the named experiment or scenario fills it from a committed result file.
  "Zero buffering" is a banned phrase; "zero-copy" only when E1's
  `strace -c` shows splice on the reference host.
- **Deferred features are named.** QUIC, HLS, DNSSEC, geographic fleet,
  OTel tracing, kernel bypass — each is a ROADMAP milestone with a
  precondition, not a stub pretending to exist.

## Documentation Map (all 15, normative)

| Document | Authority over |
|---|---|
| `PRD.md` | Requirements: FR/NFR/G/P IDs |
| `ARCHITECTURE.md` | Boundaries, dependency direction, data flow (oracle) |
| `TECHNICAL_SPEC.md` | Identifiers, types, algorithms, concurrency (oracle) |
| `LOAD_BALANCER_SPEC.md` | LB behaviour, retry table, concurrency analysis |
| `DNS_SSL_TRACKER_SPEC.md` | Wire engine, views, classifier, TLS findings |
| `MEDIA_SERVER_SPEC.md` | RFC 7233 matrix, copy paths, admission |
| `PERFORMANCE_SPEC.md` | Experiments E1–E7, KPIs, statistics, Nginx parity |
| `OBSERVABILITY_SPEC.md` | Metric inventory, closed label sets, dashboards |
| `TESTING_SPEC.md` | T-1x..T-99, fuzz, soak, synctest, traceability |
| `SECURITY_SPEC.md` | Trust boundaries, SSRF guard, threat tables |
| `API_SPEC.md` | Hub/admin/media HTTP contracts, problem-details |
| `CLI_SPEC.md` | Command tree, exit codes, one-shot tools |
| `BENCHMARK_REPORT_TEMPLATE.md` | Report form, anti-overclaim audit |
| `ROADMAP.md` | Phases 0–6, milestones M1+, v1 exit criteria |
| `README.md` | This file |

## Requirements vs Claims Ledger (evidence status)

| Claim type | Status |
|---|---|
| Architecture/specification | **Evidenced** — this document set |
| Working software | **Not yet** — Phase 0 starts implementation; nothing runnable exists |
| Performance numbers | **Not yet** — `[E-n]` cells empty by design until experiments run |
| Production readiness | **Not claimed** — gated on ROADMAP v1 criteria |

## Quickstart (will work once Phase 0–1 land; kept honest)

```bash
git clone https://github.com/rift/rift && cd rift
go build ./...        # compiles the full skeleton (pure stdlib, no deps yet)
go test ./...         # errs taxonomy suite green under -race
go run ./cmd/rift version
go run ./cmd/rift lb  # honestly refuses: Phase 2 not implemented (ROADMAP.md)
```

Service subcommands come alive as their ROADMAP phases land: `bench env`
(Phase 0), `init`/`config validate` (Phase 1), `lb` (Phase 2), `dns`/`tls`
(Phase 3), `media` (Phase 4), full `bench` (Phase 5).

## Toolchain & Environment

- Go 1.25.5 (module pins `go 1.25.5`; no toolchain upgrade is silently
  accepted — `x/sys@v0.47.0` requires ≥1.25.0 and is the ceiling test).
- Development: Windows. Performance work: WSL2 Ubuntu (default distro,
  verified present). Target: Linux servers. Windows perf claims: none.
- Admitted dependencies (whole set, with reasons — ARCHITECTURE §8):
  `gopkg.in/yaml.v3`, `golang.org/x/sys`, `prometheus/client_golang`,
  `hdrhistogram-go` (provisional on E6), `go.uber.org/goleak`.
- Docker: not installed on the dev host (verified); WSL2 provisioning is a
  Phase 0 task, and compose topology is marked unexercised until then.

## Repository Layout (target; `experiments/` populates first)

```
cmd/rift/             composition root
internal/platform/    errs config logging lifecycle health metrics httpx netx
                     pool ratelimit circuit retry testsupport
internal/lb/          model picker health l4 udp l7 control
internal/dnsmon/      model wire resolver probe node hub
internal/tlsmon/      model probe
internal/media/       model server
internal/bench/       env loadgen playersim harness
experiments/          E1..E7 Phase-0 toolchain probes (first code)
bench/                scenarios/ baselines/ results/ fixtures/
deploy/               docker/ compose/ grafana/ prometheus/ systemd/ nginx/
docs/                 appendices
```

## Security Posture (summary; SECURITY_SPEC is authoritative)

SSRF enforced once at the dialer (`netx.Guard`) with every-resolved-address
checking and no re-resolution (kills rebinding); LB never proxies to
client-chosen destinations (open-proxy boundary); per-resolver caps and
breakers bound DNS amplification; mTLS on hub ingest; admin plane loopback by
default with refuse-to-start on routable bind without explicit opt-in;
secrets never inline, redaction on every echo path; closed metric-label and
log-field sets as resource-exhaustion defense.

## License / Contribution

Not yet decided (deliberate: license choice belongs to the owner, and a
placeholder license is worse than an honest absence). Contribution model:
docs-first — every PR touches the relevant spec in the same commit as the
code, or it does not merge.
