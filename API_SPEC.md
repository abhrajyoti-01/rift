# API_SPEC.md — RIFT API Contracts

Status: **Revision A**. Owning requirements: FR-30, FR-33, plus admin-plane
endpoints for every service. All APIs are HTTP/1.1 + JSON over TLS unless
noted; data-plane listeners never host these. Versioned `/v1`; breaking
changes bump the version. Errors use RFC 9457 problem-details:

```json
{ "type": "about:blank", "title": "config invalid", "status": 400,
  "detail": "weight must be >= 1", "instance": "/v1/...", "field": "lb.pools[0].backends[2].weight" }
```

`field` carries the config field path when applicable — errors are for
operators, and operators need the location, not a vibe.

---

## 1. Endpoints by Service

### 1.1 LB admin (loopback default; `allow_remote`+token if remote)

| Method/Path | Purpose | Body / Response |
|---|---|---|
| `GET /v1/admin/reload` | current reload state | `{version, last_reload, outcome}` |
| `POST /v1/admin/reload` | trigger reload (same path as SIGHUP) | `202` `{version, diff_paths[]}` / `400` problem |
| `GET /v1/snapshot` | read-only snapshot view (redacted) | `{version, listeners[], pools[]}` |
| `GET /v1/pools` | pool summary | `[{id, picker, backends:[{id,addr,health,conns,weight}]}]` |
| `GET /v1/pools/{id}/backends/{bid}` | backend detail incl. health history window | backend object |
| `GET /healthz` / `GET /readyz` / `GET /metrics` | shared (all services) | OBSERVABILITY_SPEC §3.7 |

Auth: `Authorization: Bearer <token>`, constant-time compare. A reload
triggered while another reload runs → `409 {title:"reload in progress"}` —
reload is serialized; config swaps never interleave (LB §8).

### 1.2 DNS/TLS hub

**Ingest (mTLS, node certs only):**

`POST /v1/ingest` — batch of observations, ≤ 4096/batch:

```json
{ "node_id": "wsl2-home-01", "shipped_at": "2026-09-14T10:00:00Z",
  "observations": [ { "node_id": "...", "view": 2, "resolver": "1.1.1.1:53",
    "qname": "www.example.com.", "qtype": 1, "rcode": 0,
    "answers": [{"name":"www.example.com.","type":1,"ttl":300,"data":"93.184.216.34"}],
    "truncated": false, "transport": "udp", "latency_ms": 12.3,
    "timestamp": "2026-09-14T09:59:59.500Z", "err_class": 0 } ] }
```

`202` always (at-least-once; dedupe key = (node_id, timestamp, qname, qtype,
resolver)). `400` malformed batch (problem+field), `413` > 4096, `429` rate
limit (Retry-After), `503` storage failure (node spools). TLS findings are
shipped on the same endpoint with `"kind": "tls_report"` wrapper:
`{kind, target, findings[], chain[], negotiated, timestamp}`.

**Query (TLS or loopback):**

| Method/Path | Purpose |
|---|---|
| `GET /v1/targets` | monitored targets + current propagation state |
| `GET /v1/observations?target=&view=&from=&to=&limit=&cursor=` | window query, cursor pagination, ≤ 1000 page |
| `GET /v1/propagation?target=&view=` | classifier output: state, reference, divergent resolvers, divergence duration |
| `GET /v1/tls/reports?target=&from=&to=` | TLS reports page |
| `GET /v1/alerts?active=true` | active alerts |
| `GET /healthz` / `/readyz` / `/metrics` | shared |

Pagination cursor = opaque base64 token; `limit` ≤ 1000. `target` values are
validated against the configured closed target set (unbounded query keys are
a DoS vector — closed sets everywhere, SECURITY §3.5).

### 1.3 Media

| Method/Path | Purpose |
|---|---|
| `GET /v1/media/{name}` | the stream (RFC 7233 range semantics) |
| `HEAD /v1/media/{name}` | headers only |
| `GET /v1/media/_index?limit=&cursor=` | index page (JSON) |
| `GET /healthz` / `/readyz` / `/metrics` | shared |

No write endpoints. `429` + `Retry-After` at admission limits. `416` with
`Content-Range: bytes */{size}` on unsatisfiable range.

### 1.4 Bench tooling (local process or admin plane)

| Method/Path | Purpose |
|---|---|
| `rift bench run --scenario <id>` | run, emit samples + card |
| `rift bench compare --against <file>` | tolerance-band compare |
| `rift bench report --samples <dir>` | render report; refuses without card |
| `rift bench env` | emit environment card |

Bench talks to targets over the network like any client; its own API is the
CLI + files, not HTTP (the load generator is a client, not a server).

## 2. Shared Conventions

- Timestamps: RFC 3339 UTC (`Z`), millisecond precision.
- Durations: integer milliseconds in JSON (`latency_ms`), seconds in Prometheus.
- Enums in JSON mirror Go enum values (numeric) with string forms accepted on
  input (`"view": "recursive" | 1`); responses emit numeric + a `*_label`
  companion only in debug contexts.
- IDs: caller-assigned strings, `[a-z0-9-]{1,64}`; invalid → 400 with field
  path.
- All list responses carry `cursor` (opaque) and are capped; no unbounded
  responses anywhere (resource-exhaustion posture).
- `X-Rift-Tid`: echo/propagate on every response (AD-13 correlation).
- Rate limits: admin plane 50 rps default; ingest 100 batches/s per node
  cert; query API 20 rps per source. `429` + `Retry-After`.
- mTLS on ingest: CN=NodeID, cert verified against hub CA; no token path for
  ingest (two auth systems on one endpoint is attack surface, not defense).

## 3. Versioning and Compatibility

`/v1` frozen at v1 ship: additive changes only (new optional fields), removal
or semantic change requires `/v2` with a migration note in this spec. Wire
contracts are versioned; Go identifiers are not (AD-2).

## 4. Errors and Exit Codes

HTTP: problem-details + `field` as above; `5xx` bodies never contain stack
traces or internal paths (leaks). CLI exit codes (CLI_SPEC §5): 0 clean, 1
runtime, 2 config, 3 bind/resource, 4 drain, 124 bench timeout — aligned with
`errs.ExitCode` (TECHNICAL_SPEC §1).

## 5. Contract Tests

`T-99` exercises every endpoint above: happy path, malformed, authz failures
(401/403), rate limits (429), pagination caps, cursor stability, problem-details
shape, `field` presence for config errors, tid echo. Contract tests run
against real listeners in CI — the API spec is executable or it is décor.
