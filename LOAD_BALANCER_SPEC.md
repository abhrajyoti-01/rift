# LOAD_BALANCER_SPEC.md — RIFT Load Balancer

Status: **Revision A**. Requirement IDs from `PRD.md` (FR-10..21, NFR-1..3,
NFR-12). Identifier names from `TECHNICAL_SPEC.md` §4 are authoritative; this
document explains behaviour, algorithm derivations, concurrency analysis, and
the v1/milestone split. Oracle for data flow: `ARCHITECTURE.md` §5.1.

---

## 1. Scope and Posture

v1 ships: L4 TCP proxy, L4 UDP session proxy, L7 HTTP reverse proxy, three
pickers, active health, connection/timeouts/limits, retry-per-closed-table,
token-bucket rate limiting, hot reload with zero-dropped-connections, metrics
and structured logs on a separate admin plane.

Milestones (not in v1, no stubs — ARCHITECTURE AR-6): HTTP/2-to-backend
(cleartext h2c stays disabled by default, AD-3), consistent-hash picker with
ring, per-backend passive health from data-plane error rates, TLS backend
re-encryption (v1 terminates only), connection draining on backend removal
beyond idle-timeout grace, Prometheus SD metadata endpoint, OCSP stapling
automation.

## 2. Configuration

```yaml
lb:
  listeners:
    - id: web
      proto: http           # http | tcp | udp
      bind: "0.0.0.0:8080"
      pool: web_pool
      max_conns: 10000
      tls: null             # or {cert_file, key_file, min_version: "1.2"}
    - id: raw
      proto: tcp
      bind: "0.0.0.0:9022"
      pool: raw_pool
      max_conns: 5000
  pools:
    - id: web_pool
      picker: least_connections   # round_robin | weighted_round_robin | least_connections
      backends:
        - {id: b1, addr: "127.0.0.1:9001", weight: 5}
        - {id: b2, addr: "127.0.0.1:9002", weight: 3}
        - {id: b3, addr: "127.0.0.1:9003", weight: 1}
        # NOTE: a deliberately-invalid entry (e.g. addr: "127.0.0.1 9003",
        # missing colon) is rejected at validate with a field path — shown in
        # the validation matrix below, not in this otherwise-valid example.
      health:
        kind: http          # tcp | http
        interval: 5s
        timeout: 2s
        rise: 2
        fall: 3
        http: {method: GET, path: /healthz, expect: [200, 204]}
      timeouts:
        dial: 3s
        idle: 60s
        copy: 30s
      retry:
        max_attempts: 2     # per request, connect-phase for all methods; post-write per table
      rate_limit:
        per_source: {rate: 200/s, burst: 400}
        per_pool:  {rate: 5000/s, burst: 10000}
  reload:
    mode: signal_and_api    # SIGHUP and POST /v1/admin/reload
    validate_before_swap: true
  shutdown_timeout: 15s
```

Validation matrix (all rejected at startup *and* reload, with field paths):

- unknown field anywhere → `config invalid: lb.pools[0].backends[2].addr`
- `max_conns ≤ 0`, `weight < 1`, `rise|fall < 1`, `interval < 1s`,
  `timeout > interval`, malformed `addr` (host:port, IP or resolvable host,
  no userinfo, no path), duplicate pool/listener IDs, listener referencing
  missing pool, `proto: udp` with `tls` set, negative rates, `burst < rate`
  for token-bucket steady-state sanity, `min_version < 1.2`, missing
  `cert_file`/`key_file` when `tls` non-null, port < 1024 without
  `allow_privileged: true` (Linux; hints at capabilities rather than silently
  binding or crashing).
- `rift config validate` executes the identical code path (FR-2); exit 2 on any
  failure with every violation listed, not just the first.

## 3. L4 TCP — Behaviour and Rationale

### 3.1 Connection lifecycle

Accept → admission check (`maxConns`; over → immediate close +
`rift_lb_admissions_rejected_total{reason="max_conns"}` — **never a queue**;
queuing admissions converts overload into latency instead of a visible refusal)
→ `SetNoDelay(true)` + keepalive (`Idle: 30s, Interval: 15s`) → pick → dial
under `dial` timeout → forward → teardown. One goroutine per connection is the
correct model here: the work unit *is* the socket (ARCHITECTURE §6 rule).
A worker pool in front of a blocking `Read` would add a channel handoff per
event and strictly lose latency; measured, not assumed, in E7's control arm.

### 3.2 Copy loop and the splice question

Primary path: `dst.ReadFrom(src)` on `*net.TCPConn` — verified present in the
pinned toolchain; on Linux this takes the runtime `splice()` fast path for
socket→socket. Fallback path (always compiled, chosen by config, and the
*default* until E1 confirms): 8 KiB `sync.Pool` chunked copy with per-op
deadlines. E1's `strace -c` result decides which is default; the decision and
its numbers are committed in `bench/results/E1-splice.txt` before the splice
path ships as default (UD-2). **Neither path allocates per KiB** (NFR-1) — the
pooled-copy path reuses its buffer across the connection's lifetime.

### 3.3 Half-close handling

Client EOF → `CloseWrite` to upstream and **continue draining the response**
until upstream EOF or idle deadline. Without this, a client that half-closes
after sending a request gets its response truncated at whatever was buffered —
the classic toy-proxy bug RIFT exists to not have. `CloseRead`-drain on
teardown so an in-flight backend answer does not become an RST.

### 3.4 Deadlines

Per-op deadlines, not blanket: `idle` (no data in a direction for 60s default),
`copy` (30s per Read/Write call, reset each op — distinguishes "slow but
flowing" from "stuck"). A stalled client holds only its own goroutine and one
`maxConns` slot and is culled at deadline: bounded resources is a correctness
property (FR-21).

### 3.5 Backend failure on dial

Dial failure (connect-phase only) → repick from the same pool, up to
`max_attempts` (default 2), each attempt a *different* backend. Attempts
exhausted → `rift_lb_backend_errors_total{class}` + close. This retry is
safe for all methods because zero bytes reached any backend (the
`httptrace`-anchored phase definition in §6.2 applies to L4 identically).

## 4. L4 UDP — Sessions

- One read loop on the shared listener; `SessionID` =
  `hex(src)|hex(dst)` (normalized 5-tuple). One upstream `*net.UDPConn` per
  session (connected), one reply goroutine per session.
- Affinity is per-session (AD-14): mid-flow re-picking breaks application flow
  state (QUIC-in-UDP, DTLS, games). Re-pick only if the session's backend goes
  unhealthy AND the client sends again.
- `max_sessions` (default 4096) is a hard gate; overflow → drop +
  `rift_lb_udp_dropped_total{reason="table_full"}`. An unbounded session table
  is a memory bomb reachable by one `sendto` loop — the cap is security, not
  tuning.
- Idle sweep 30s; TTL 120s (config). `Up/DownPkts/Bytes` atomics per session
  feed `rift_lb_udp_sessions` and `rift_lb_bytes_total{proto="udp"}`.
- **No congestion signal exists in UDP.** Drop-with-counter is the only
  backpressure. The spec states this rather than pretending flow control
  exists; per-source token buckets (§7) bound abuse.

## 5. L7 HTTP — Reverse Proxy with Owned Seams

Framing is stdlib-owned (AD-3): CL+TE ambiguity, `Expect: 100-continue`,
trailers, 1xx relay — the smuggling surface — stays in `net/http` +
`httputil.ReverseProxy`. RIFT owns the three seams:

1. **Transport**: ours. `MaxIdleConnsPerHost` 32/backend default,
   `IdleConnTimeout` from pool config, `ForceAttemptHTTP2: false`,
   `ResponseHeaderTimeout` = `dial + response budget`, httptrace hooks →
   reuse ratio and connect-vs-wait metrics, retry-phase anchoring.
2. **BufferPool**: `sync.Pool[[]byte]` (16 KiB); reuse rate is a metric.
3. **ModifyResponse/ErrorHandler**: status classification, retry-eligibility
   recording, per-attempt accounting.

### 5.1 Request classification (before forwarding)

- Method → idempotency class per §6 table; unknown methods rejected with `501`
  (`rift_lb_requests_total{outcome="method_rejected"}`) — an unknown method is
  not something a proxy should forward on someone's behalf (FR-17 posture).
- Absolute-form request-target (proxy-form `GET http://host/`) → host header
  rewritten to upstream authority *only from the configured pool's backends*;
  authority never selects the backend (open-proxy boundary, SECURITY_SPEC §3).
- `Content-Length` > `max_body_bytes` (default 64 MiB, config) → `413` early,
  counted. Smells like tuning, is actually security (resource exhaustion).
- CL+TE both present → reject before forwarding (stdlib parses; we re-check
  because the proxy boundary is where smuggling lives, SECURITY_SPEC §4).

### 5.2 Retry phases and the closed table

Phase = `connect` (no byte written to backend socket; anchored by httptrace
`WroteRequest`/`GotConn`) or `post-write`. `max_attempts` counts *retries*, not
attempts (a "retry" budget of 2 = at most 3 total attempts).

| Method | connect-phase | post-write |
|------|---|---|
| GET, HEAD, OPTIONS, TRACE | retry ≤ max | retry ≤ max |
| POST | retry ≤ max | **never** |
| PUT, DELETE | retry ≤ max | once iff `idempotent_put_delete: true` |
| PATCH | retry ≤ max | **never** |
| other | **never** | **never** |

Retrying a POST whose bytes reached the backend is a data-corruption bug, not
an availability feature (AD-15). Every refusal increments
`rift_lb_retries_total{outcome="refused_nonidempotent"}` so the operator sees
the availability being traded away for correctness.

### 5.3 Headers

`X-Forwarded-For`: append-only. Inbound `X-Forwarded-For` is *attributed*
(trusted) only when the peer IP ∈ `trusted_proxies` (default empty). RFC 7239
`Forwarded` emitted when configured. `X-Forwarded-Proto` set. Server-side
headers that leak topology (`Server`) replaced with `rift`.

### 5.4 Distribution guarantees

Picker distribution is tested, not hoped: χ² test on pick counts for RR (uniform
p > 0.01 with df = backends−1), smooth-WRR exact interleave pattern assertion
(weights 5,3,1 over 9 picks = deterministic sequence), least-conn picks minimum
inflight with tie-rotation. See TESTING_SPEC T-22.

## 6. Health

- Scheduler: one goroutine per pool on an injected ticker; checks on
  `pool.Executor` (workers = min(backends, 8)); rise/fall thresholds applied
  before `Backend.Health` atomic store; all transitions logged with latency.
- HTTP check: expected-status set; non-2xx/3xx within set → fail; timeout →
  fail. Body not read beyond 1 KiB (bounded work per check).
- On reload: new backends start `unknown` → treated as unhealthy until first
  check passes (`rise` default 2) — a fresh backend must earn traffic; removed
  backends keep established conns until idle grace, stop being picked.
- Interaction with picker: unhealthy backends are skipped by all three
  pickers without allocation. All-unhealthy → `ErrNoUpstream` → L7 `502`
  + `readyz=false` (process stays live and observable — ARCHITECTURE §13).

## 7. Rate Limiting

Per-source and per-pool token buckets (`platform/ratelimit.Sharded`): FNV-1a
key hash, power-of-two shards (≥8), bounded key capacity (default 10k keys)
with LRU eviction + `rift_ratelimit_keys_evicted_total` — unbounded key maps
are spoof-source memory bombs. Per-connection bucket (in addition to per-IP)
defeats spoofed-source bypass: a flood from many fake IPs still consumes
per-pool and per-conn budgets. Rejected → `429` (L7) with `Retry-After` /
immediate close (L4) + counter. Bypass attempts (header forging, rotating
spoofed sources, pipelined bursts) are a named test class (T-45).

## 8. Reload

Path: read → validate → build Snapshot → `atomic.Pointer.Store` → log
`config.Diff` (field paths) → drain removed backends. Established connections
**are never dropped by a reload** (NFR-3, G8): listeners keep accepting (bind
unchanged), pools swap atomically; a removed backend's established conns live
out their idle grace. SIGHUP and `POST /v1/admin/reload` are one code path;
invalid configs reach the data plane through neither. Reload-under-load test
asserts zero reload-attributable resets (T-41).

## 9. Metrics (LB-specific)

Full inventory + label sets: `OBSERVABILITY_SPEC.md` §3. LB surface:

`rift_lb_connections_total{listener,proto,state}` ·
`rift_lb_active_connections{listener}` (sampled gauge) ·
`rift_lb_bytes_total{listener,proto,direction}` ·
`rift_lb_request_duration_seconds{listener,pool}` ·
`rift_lb_pick_duration_seconds{pool,algo}` ·
`rift_lb_backend_picks_total{pool,backend}` ·
`rift_lb_backend_errors_total{pool,backend,class}` ·
`rift_lb_retries_total{pool,backend,outcome}` ·
`rift_lb_upstream_conn_reuse_ratio{pool}` ·
`rift_lb_backend_up{pool,backend}` ·
`rift_lb_ratelimit_rejected_total{scope}` ·
`rift_lb_admissions_rejected_total{reason}` ·
`rift_lb_udp_sessions{listener}` · `rift_lb_udp_dropped_total{reason}` ·
`rift_lb_config_reload_total{outcome}` ·
`rift_lb_config_last_reload_timestamp_seconds` ·
`rift_lb_health_check_duration_seconds{pool}`.

Label cardinalities: `backend` bounded by config (≤1000), `listener` ≤ 32,
`reason`/`outcome`/`class` from closed enums. The label-set unit test
(OBSERVABILITY_SPEC §3.4) fails the build on any open-ended label.

## 10. Concurrency Analysis (the questions, answered)

- **Goroutine model**: 1 accept loop + 1 goroutine/conn (L4); stdlib
  per-conn goroutines (L7); 1 scheduler + bounded check pool (health); 1 read
  loop + 1 sweeper + 1 reply goroutine/session (UDP); 1 control goroutine.
  No shared worker pool spans data and control planes.
- **Connection lifecycle**: `conn.Opened → Active → (Idle|Draining) → Closed`
  with ConnState-equivalent counters; every state transition is atomic and
  metric-visible; teardown always decrements even on error paths (leak test
  T-33).
- **Worker architecture**: pools only for timer-driven and outbound-budget
  work (health checks, DNS), sized by work; never in front of blocking
  socket reads — a channel handoff per event strictly loses latency, and E7's
  control arm measures the cost for the record.
- **Synchronization**: data plane reads `atomic.Pointer[Snapshot]` (zero
  locks); backend `Conns`/`Health` atomics; picker RR/WRR state under one
  small mutex (measured, §PERFORMANCE_SPEC); rate-limiter sharded maps;
  control plane serialize reloads (single goroutine) so two reloads cannot
  interleave.
- **Lock contention risks**: WRR mutex at G≥16 is the known candidate;
  benchmark at G=1..64 × backends 4/64/1024 decides sharded variant —
  no mutex redesign without a superlinear scaling result (AR-5).
- **Channel usage**: channels appear only at *cold* boundaries (reload
  trigger, shutdown signal, check dispatch); the hot path is function calls
  and atomics only. No channel sits between a socket read and a socket write.
- **Memory allocation risks**: accept path (2-alloc budget, NFR-1), picker
  (0-alloc, NFR-2), response-header maps in L7 (bounded by stdlib's pooling —
  verified by benchmark not assumed), `httptrace` closures (cold: one per
  request; acceptable at L7 request cost, would be a finding at L4 rates).
- **Buffer management**: L4 pooled 8 KiB (fallback mode), L7 16 KiB
  `BufferPool`, UDP: no buffering — read packet, table lookup, write
  upstream; the socket buffer is the only queue.
- **Backpressure**: admission refusal (never queue) at `maxConns`; per-op
  deadlines cull stalled writers; token buckets shed abusive load; UDP drops
  with counter. Overload produces visible refusals, not hidden latency.
- **Network read/write behaviour**: TCP_NODELAY on; splice path when E1
  confirms; per-op deadline resets; half-close propagation; `CloseRead`-drain.
- **Graceful shutdown**: fixed ordering (TECHNICAL_SPEC §13); drain budget
  from `shutdown_timeout`; in-flight conns served to completion or deadline;
  exit 4 with in-flight counts when the budget fires (visible, not hidden).

## 10a. Failure Modes (LB)

| Failure | Detection | Behaviour | Rationale |
|---|---|---|---|
| All backends down | health + `ErrNoUpstream` | 502, `readyz=false`, process live | Admin plane must stay readable |
| Backend RST mid-stream | read/write error | Close client, decrement, `ClassPeerClosed` excluded from error SLO | Normal churn, not a fault |
| Accept EMFILE | `maxConns` < rlimit−margin | Admission refused early + counter | Earlier refusal beats mid-copy EMFILE |
| Reload during drain | validate-then-swap | Previous snapshot keeps serving | A typo cannot take down the data plane |
| Health-check flap | rise/fall thresholds | Hysteresis via 2/3 default | Prevents pick oscillation |
| UDP table flood | `max_sessions` gate | Drop + distinct counter | Unbounded table = memory bomb |
| Slowloris | per-read deadline + `IdleTimeout` | Cull at deadline + counter | Bounded goroutines/FDs is correctness |
| Client sends garbage bytes (L4) | No parse exists | Byte-tunnel — cannot fail parse | L4 has no parser to poison; framing errors are the client's problem |

## 11. Acceptance (LB-specific)

- Byte-equivalence soak: 10 GiB across 3 pickers × splice/pooled modes, all
  bytes match, zero leaks (T-12).
- Distribution: χ² passes for RR; WRR exact interleave; least-conn min-inflight
  (T-22).
- Reload-under-load: zero reload-attributable drops (T-41, G8/NFR-3).
- Fault suite: backend RST / slow / half-close / EMFILE simulation — all
  classified correctly, no goroutine leaks (T-3x).
- Benchmarks: NFR-1, NFR-2, NFR-12 measured and recorded in `bench/results/`;
  Nginx parity table completed with both-arms runs (`PERFORMANCE_SPEC` §6).
