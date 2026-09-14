# OBSERVABILITY_SPEC.md — RIFT Observability

Status: **Revision A**. Owning requirements: FR-4, FR-5, FR-39, plus the
metric/label/bucket rules referenced by every component spec. Identifier
authority for metric names: this document. `TECHNICAL_SPEC.md` §10 defines the
code-level rules the `platform/metrics` package enforces.

---

## 1. Principles

1. **Two planes.** Metrics/health/admin on a dedicated listener; data-plane
   listeners never expose `/metrics`. Verified by a port-scan integration test
   (T-69).
2. **The instrument is named.** Every metric's docstring names its instrument
   (server counter vs playersim client model). A number whose instrument is
   ambiguous is not a fact (P3).
3. **Closed label sets.** A label with unbounded value space is a
   memory-exhaustion primitive reachable from client input. Enforced by a unit
   test walking registered collectors (§3.4).
4. **Sampled gauges on hot paths.** Active-connection counts sampled by
   ticker, never incremented per event — an increment-per-event gauge is a
   contention point and a GC burden.
5. **Liveness ≠ readiness.** Zero healthy backends → LB not ready but still
   live: the admin plane must stay readable to explain itself.

## 2. Logging — `platform/logging`

`log/slog`, JSON. Closed field set:

`ts, level, msg, svc, comp, rid, tid, dur_ms` + per-site fields (each
enumerated below; anything else is a spec change):

| Site | Fields |
|---|---|
| LB conn accept | `listener, remote, proto` |
| LB conn close | `listener, reason, up_bytes, down_bytes, dur_ms` |
| LB pick | `pool, backend, algo, attempt` |
| LB retry | `pool, backend, phase, method, outcome` |
| LB health | `pool, backend, healthy, latency_ms` |
| LB reload | `version, diff_paths` (field paths, never values — values could carry secrets) |
| DNS query | `node, resolver, view, qtype, rcode, truncated, transport, latency_ms` (`qname` capped 253) |
| DNS ship | `node, batch, bytes, outcome` |
| DNS alert | `target, code, severity` |
| TLS probe | `target, findings, negotiated, chain_depth` |
| TLS alert | `target, code, severity` |
| Media stream | `asset, range_start, range_len, class, first_byte_ms` |
| Media admission | `scope, remote` |
| Lifecycle | `phase, services, in_flight` |

Rules: level filtering at the handler (debug suppressed in prod profile);
`ClassPeerClosed` events sampled 1:1000 (an RST storm must not bury real
errors in log volume or disk); no raw URLs from user input, no byte dumps, no
secret values ever (config redaction covers reload logs); `rid` per
connection/request (16 hex crypto/rand), `tid` per logical operation, node→hub
propagated via `X-Rift-Tid` header (AD-13). Errors carry `errs.Class` in a
`class` field, set by the structured logger helper, not by hand at each site.

## 3. Metrics

### 3.1 Registry and process metrics

`prometheus/client_golang` registry per process; collectors:
`rift_build_info{version,commit,goversion}` (mandatory — a benchmark without
build labels is not reproducible), `go_goroutines`, `go_memstats_*`,
`process_open_fds`, `process_cpu_seconds_total`, `go_gc_duration_seconds`.

### 3.2 Naming rules

Prefix `rift_`; units in name (`_seconds`, `_bytes`, `_total` for counters);
snake_case; histogram name is the measured quantity, no `_histogram` suffix
(Prometheus convention).

### 3.3 Bucket sets (fixed now; revised only with a measured latency distribution — AR-5)

| Set | Buckets (seconds) | Applied to |
|---|---|---|
| `latency_lan` | .00005, .0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5 | LB request, pick, media first-byte, DNS query |
| `latency_wan` | .05, .1, .25, .5, 1, 2.5, 5, 10 | TLS handshake |
| `bytes_transfer` | 4k, 32k, 256k, 1M, 4M, 16M, 64M, 256M, 1G | media stream bytes |
| `ratio` | .5, .75, .9, .95, .98, .99, 1 | reuse ratio, readahead hit |
| `batch_size` | 1, 8, 32, 128, 512, 2048, 4096 | hub ingest batch |

### 3.4 Cardinality budget

Per-metric label sets are **closed enums** (component specs §metrics). Hard
bounds: `backend` ≤ 1000, `listener` ≤ 32, `resolver` ≤ 32, `target` ≤ 256,
`node` ≤ 64, `asset` **never a label** (unbounded). The enforcement unit test
constructs each registered collector's label names from code constants (never
from runtime data), asserts the name set, and a fuzz pass attempts to register
a collector with an `asset`-like label. `qname` is never a label; logs only,
capped 253.

### 3.5 Gauge sampling

`rift_lb_active_connections`, `rift_media_active_streams`,
`rift_dns_...in_flight` are sampled at 1 Hz by one collector goroutine, reading
atomics — no hot-path increments. `rift_media_readahead_hit_ratio`,
`rift_lb_upstream_conn_reuse_ratio` sampled 1 Hz.

### 3.6 Full inventory

Platform: `rift_build_info`, `rift_phase{svc}` (lifecycle gauge),
`rift_ratelimit_rejected_total{scope,svc}`.

LB: as LOAD_BALANCER_SPEC §9.

DNS/TLS: as DNS_SSL_TRACKER_SPEC §8.

Media: as MEDIA_SERVER_SPEC §8.

Bench harness: `rift_bench_requests_total{scenario,outcome}`,
`rift_bench_request_latency_seconds{scenario}` (loadgen-side; never scraped on
data plane), `rift_player_startup_seconds{scenario}`,
`rift_player_stalls_total{scenario}`, `rift_player_underrun_seconds{scenario}`.

Hub: as DNS_SSL_TRACKER_SPEC §8.

### 3.7 Health endpoints

- `GET /healthz` — liveness: process responsive on admin plane; `200
  {"status":"ok","phase":"serving"}`; phase from `lifecycle.App.Phase()`.
- `GET /readyz` — readiness: AND of component checks (LB: config valid ∧
  listeners bound ∧ ≥1 backend up; media: root present ∧ index built ∧ slots
  free; hub: store writable). Failing → `503` with per-check JSON body — a
  load balancer in front of RIFT LB should not route to it when its own
  pools are empty (that is the point of the distinction, §1.5).
- Both never on the data plane (T-69).

## 4. Dashboards (Grafana JSON in `deploy/grafana/`)

1. **LB Golden Signals** — RPS, p50/p95/p99 from `latency_lan` histograms,
   error rate by `class`, pick latency by algo, backend distribution stacked
   bar (catches a broken picker visually), reuse ratio, admission rejections.
2. **DNS/TLS** — per-resolver latency heatmap, rcode/timeout %,
   propagation-state timeline per target, view-divergence timeline, cert
   days-remaining ascending (the operational view), alerts by severity.
3. **Media** — aggregate + per-class throughput with measured disk/NIC
   ceiling reference lines (from the environment card; the panel that keeps
   claims honest), active streams, first-byte vs send-stall side by side,
   readahead hit ratio.
4. **Host/Capacity** — goroutines, RSS vs `GOMEMLIMIT`, FDs vs rlimit, GC p99
   pause, CPU by core with `GOMAXPROCS` overlay (the panel that catches
   cgroup-vs-GOMAXPROCS mismatch — the classic container perf ghost).

Dashboard JSON is regenerated from a checked-in description and the card
ceilings, not hand-drawn numbers — reference lines must match the environment
that produced them.

## 5. Tracing (deferred, AD-13)

No OTel in v1. `tid` propagation: client-supplied or generated at ingress,
kept in context, logged at each hop, forwarded node→hub via `X-Rift-Tid`.
A distributed trace follows when the node→hub protocol is real and spans
must cross a network hop (the deferral is structural, documented in
ARCHITECTURE AD-13, not an omission).

## 6. Alert Rules (Prometheus expression, shipped in `deploy/prometheus/`)

Representative (the full set with for-clauses and labels ships with the
dashboard pack):

- `histogram_quantile(0.99, rate(rift_lb_request_duration_seconds_bucket[5m])) > 0.25`
- `sum by (pool) (rate(rift_lb_backend_errors_total{class=~"network|timeout"}[2m])) > 1`
- `rift_lb_backend_up == 0` for 2m
- `rift_dns_propagation_state == 3` (Divergent) for 10m
- `time() - rift_tls_cert_not_after_seconds < 86400 * 14`
- `rift_media_send_stall_seconds_count` rate > 0 for 5m (clients starving while
  server CPU is idle = transport problem, not load)
- `process_open_fds / 65536 > 0.7`

## 6a. Failure Modes (observability itself)

| Failure | Detection | Behaviour |
|---|---|---|
| Metrics listener down | scrape failure | Service keeps serving; alert via absent-metric rule (`absent()`) |
| Log disk full | handler error | Rate-limit non-critical levels, never crash the data plane for a log fault |
| Cardinality explosion attempt | closed-label unit test + fuzz | Register rejected; ClassSecurity log |
| Scrape storm | admin rate limit | 429 with Retry-After |
| Gauge sampler stuck | sampler watchdog goroutine | Alert `rift_sampler_stalled` if no tick in 5 s |

## 7. Verification

- Label-set unit test + fuzz (§3.4) green.
- Port-scan test: `/metrics` unreachable on data plane (T-69).
- Each dashboard's queries validated against a running registry (CI spins
  services, scrapes, asserts series exist with expected labels).
- Log schema test: golden files per event type; unknown fields fail.
- `rift metrics` CLI dumps live registry as text (debug aid, admin-plane
  only).
