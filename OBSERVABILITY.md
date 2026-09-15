# RIFT Observability

RIFT follows one rule that shapes everything here: **a number must name its
instrument.** Server-side counters and client-side measurements are never
merged, because they measure different things.

---

## 1. Instrument Separation

| Instrument | Measures | Never used to claim |
|---|---|---|
| Server counters (bytes sent, active streams) | what the server emitted | what the client experienced |
| `playersim` (playback clock) | startup latency, stalls, underrun | server throughput |
| `loadgen` (client-side) | request latency, throughput, errors | server CPU cost |
| Environment card | host limits and ceilings | application behaviour |

A "rebuffer rate" computed from server-side send stalls is not a rebuffer rate;
the server cannot see the client's playout buffer. RIFT reports send-stall
seconds (a server observation) and stall count (a client observation) as
separate numbers with separate names.

---

## 2. Logging

Structured logging via `log/slog`. The intended field set is closed:

| Field | Meaning |
|---|---|
| `ts` | timestamp |
| `level` | level |
| `msg` | message |
| `svc` | service (`lb`, `dns`, `tls`, `media`) |
| `comp` | component within the service |
| `rid` | per-connection/request ID |
| `tid` | per-operation trace ID, propagated node→hub |
| `dur_ms` | operation duration |
| `err` | error text |
| `class` | the `errs.Class` of a failure |

Cardinality discipline: **no unbounded value in a log field.** DNS names are
capped at 253 characters, raw client URLs are not logged verbatim, and no byte
dumps are emitted. A log field that accepts attacker-controlled text without a
bound is a disk-exhaustion primitive.

> **Implementation status:** the `logging` package defines this contract. Full
> wiring across every call site is incomplete; several components currently
> report through return values and counters rather than through this logger.
> This is recorded here rather than implied to be finished.

---

## 3. Metrics

Metric naming follows a strict convention: `rift_` prefix, snake_case, unit in
the name (`_seconds`, `_bytes`, `_total`, `_bytes_total`).

### 3.1 Counters currently maintained in code

These are real, atomically-maintained counters on the data paths.

**Load balancer**

| Counter | Type | Meaning |
|---|---|---|
| `accepted` | counter | connections accepted |
| `rejected` | counter | connections refused by the admission gate |
| `closed` | counter | connections torn down |
| `upstream_bytes` / `downstream_bytes` | counter | bytes in each direction |
| `dial_failures` | counter | connect failures to backends |
| `active_conns` | gauge | in-flight connections |
| `requests` | counter | HTTP requests handled |
| `responses_2xx` / `_4xx` / `_5xx` | counter | response class |
| `retries` / `retry_refused` | counter | retry outcomes |
| `upstream_errors` | counter | proxy errors |

**DNS node**

| Counter | Meaning |
|---|---|
| `observations` | observations produced |
| `shipped` | observations delivered to the hub |
| `ship_failures` | failed delivery attempts |
| `dropped_ring_full` | observations evicted from the bounded ring |
| `dropped_spool` | observations dropped because the spool was full |
| `spooled` / `spool_bytes` | spooled observations and current spool size |

**DNS hub**

| Counter | Meaning |
|---|---|
| `batches_accepted` / `batches_rejected` | ingest outcomes |
| `observations` | observations stored |
| `window_evictions` | per-key window evictions |
| `segments` / `segment_bytes` | segment files and bytes written |

**Media server**

| Counter | Meaning |
|---|---|
| `bytes_sent` | bytes served |
| `active_streams` | in-flight streams |
| `range_requests` | by parse outcome (`none`, `ok`, `multi`, `invalid`) |
| `admission_refused` | streams refused at the limit |
| `client_stalls` | copy errors after headers were sent |
| `first_byte_nanos` / `first_byte_count` | first-byte latency accumulation |
| `deadline_unsupported` | streams where a write deadline could not be applied |

### 3.2 Drop attribution

Every place that discards data attributes the discard:

| Reason | Component | Meaning |
|---|---|---|
| `ring_full` | DNS node | bounded ring evicted the oldest observation |
| `table_full` | UDP LB | session table at capacity; new client dropped |
| `idle_sweep` | UDP LB | session expired |
| `dial_fail` | UDP LB | upstream unreachable |
| `no_route` | UDP LB | no healthy backend |
| `spool_full` | DNS node | offline spool at capacity |

A monitor that reports "some data was lost" is useful. One that reports nothing
is not.

### 3.3 Prometheus exposition

> **Status:** 🔜 the admin plane does not yet serve `/metrics`. The counters
> above are maintained and testable; the registry and exposition layer is the
> remaining work. `github.com/prometheus/client_golang` is already a dependency
> for exactly this step.

When wired, the label cardinality rules are:

| Label | Bound |
|---|---|
| `listener` | ≤ 32 |
| `pool` | ≤ 256 |
| `backend` | ≤ 1000 |
| `resolver` | ≤ 32 |
| `target` | ≤ 256 |
| `reason`, `class`, `outcome` | closed enumerations |
| `qname` | **never a label** — logs only, capped at 253 characters |

An unbounded label is a memory-exhaustion primitive reachable from client input,
so this is a correctness rule, not a style preference.

---

## 4. Health Endpoints

| Endpoint | Meaning | Current behaviour |
|---|---|---|
| `/healthz` | liveness: is the process responsive? | Served by media and hub; returns `{"status":"ok"}` |
| `/readyz` | readiness: can it serve? | Media returns `503` until the index is built; hub returns ready |

The distinction matters: a load balancer with zero healthy upstreams should stay
**live** (so an operator can read its state) but report **not ready** (so it
leaves the rotation).

---

## 5. What a Useful Dashboard Shows

Charts worth building, and the question each answers:

1. **Latency percentiles over time** (p50/p95/p99) — is the tail moving before
   the median does? The tail is where users notice.
2. **Error rate split by class** — a `timeout` spike and a `peer_closed` spike
   are different events; merging them into one "errors" line hides both.
3. **Backend distribution as a stacked bar** — this is the chart that catches a
   broken picker visually. If the split drifts from the configured weights, the
   picker has a bug.
4. **Admission refusals over time** — the honest saturation signal. Rising
   refusals mean the system is shedding load deliberately rather than
   accumulating latency.
5. **Throughput against measured ceilings** — plot media bytes/second against
   the disk and NIC ceilings from the environment card. A number without its
   ceiling is not interpretable.
6. **First-byte latency beside send-stall seconds** — server-side contributions,
   side by side, with the client-side stall count on a separate panel.
7. **Goroutine count and open descriptors** — slow growth here is a leak; it is
   the cheapest early warning there is.
8. **DNS propagation state per target** — with divergent resolvers named, and
   `insufficient_coverage` clearly distinct from `converged`.

---

## 6. Reproducibility

Every benchmark artifact carries an environment card. Reports regenerate from
stored raw samples. The card explicitly declares when a host is unsuitable:

```
Environment card (generated 2026-09-15T17:41:42Z)
  os/arch        windows/amd64
  cpus           12 (GOMAXPROCS 12)
  wsl2           false
  note           Windows host: no performance claims may be derived from this card
```

No performance number in this repository should be read without its card.
