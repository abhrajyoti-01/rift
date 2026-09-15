# RIFT API Reference

RIFT exposes two HTTP planes per service: a **data plane** that carries the
service's actual traffic, and an **admin/query plane** that is never the same
socket as the data plane.

Endpoints marked ✅ are implemented. Endpoints marked 🔜 are documented
contract targets and are not currently served.

All JSON planes return `application/json`. Error bodies are plain text with a
status code, except where noted.

---

## 1. Load Balancer — Admin Plane ✅

Default bind: `127.0.0.1:9000` (`observability.admin_addr`). A routable bind
requires `allow_remote: true`; configuration validation refuses it otherwise.

### `GET /v1/snapshot`

Returns the current immutable configuration snapshot: listeners and pools.

```json
{
  "Version": 3,
  "Listeners": [
    {"ID": "web", "Proto": "http", "Bind": "127.0.0.1:8080", "Pool": "web_pool", "MaxConns": 10000}
  ],
  "Pools": {"web_pool": {"ID": "web_pool", "Picker": "least_connections", "Backends": null}}
}
```

> Note: the snapshot renders pool *identity*; per-backend live state is exposed
> by `/v1/pools` so the two views have distinct costs.

### `GET /v1/pools`

Per-pool backend state, including live health and in-flight connection counts.

```json
{
  "pools": [
    {
      "id": "web_pool",
      "picker": "least_connections",
      "backends": [
        {"id": "b1", "addr": "127.0.0.1:9001", "weight": 1, "health": true, "conns": 3},
        {"id": "b2", "addr": "127.0.0.1:9002", "weight": 1, "health": false, "conns": 0}
      ]
    }
  ]
}
```

### `POST /v1/admin/reload`

Validates the configuration file and, if valid, atomically swaps the snapshot.
Identical to `SIGHUP`. **Established connections are never dropped by a reload.**

Authorization:

| Condition | Accepted caller |
|---|---|
| No `admin_token_file` configured | loopback peers only |
| Token configured | `Authorization: Bearer <token>`, compared in constant time |

| Status | Meaning |
|---|---|
| `200` | `{"outcome":"ok","version":4}` |
| `400` | validation failed; `{"error":"...","outcome":"fail"}`; previous snapshot retained |
| `401` | unauthorized |
| `405` | method other than `POST` |

---

## 2. DNS Hub — Ingest Plane ✅

Default bind: `127.0.0.1:9001` (`dns.hub_server.ingest_addr`).

mTLS is required by default (`server_cert_file`, `server_key_file`,
`client_ca_file`). A plaintext opt-in exists for local development and is
**refused on a routable bind**.

### `POST /v1/ingest`

Accepts newline-delimited JSON observations.

```
Content-Type: application/x-ndjson
```

One record per line:

```json
{"node_id":"node-01","location":"lab","view":1,"resolver":"1.1.1.1:53",
 "qname":"example.com.","qtype":1,"rcode":0,
 "answers":[{"name":"example.com.","type":1,"ttl":136,"data":"104.20.23.154"}],
 "truncated":false,"transport":"udp","latency_ms":31.6,
 "timestamp":"2026-09-15T17:49:25.5273689Z","err_class":0}
```

| Field | Meaning |
|---|---|
| `view` | `1` = recursive view, `2` = authoritative view |
| `qtype` | numeric RR type (1=A, 2=NS, 5=CNAME, 15=MX, 16=TXT, 28=AAAA) |
| `err_class` | `0` on success; otherwise the error classification |
| `timestamp` | RFC 3339 with nanoseconds, UTC |

Validation: `timestamp` must parse, `view` must be 1 or 2, `qname` must be
1–253 characters, and the batch is capped by `max_batch` (default 4096) with the
request body additionally bounded.

| Status | Meaning |
|---|---|
| `202` | `{"accepted":N}` |
| `400` | malformed record, invalid timestamp, invalid view, invalid qname |
| `405` | non-`POST` |
| `413` | batch larger than the cap |
| `429` | rate limited |
| `503` | storage unavailable — the node should spool and retry |

---

## 3. DNS Hub — Query Plane ✅

Default bind: `127.0.0.1:9002` (`dns.hub_server.query_addr`).

### `GET /v1/observations?target=NAME&type=A`

Returns buffered observations for a name. **Capped at the 1000 most recent**;
an unbounded response is a resource-exhaustion primitive.

```json
{
  "count": 1,
  "observations": [
    {
      "NodeID": "test-node",
      "View": 1,
      "Resolver": "1.1.1.1:53",
      "QName": "example.com.",
      "QType": 1,
      "RCode": 0,
      "Answers": [{"Name":"example.com.","Type":1,"TTL":136,"Data":"104.20.23.154"}],
      "Truncated": false,
      "Transport": "udp",
      "Latency": 31656000,
      "Timestamp": "2026-09-15T17:49:25.5273689Z",
      "ErrClass": 0
    }
  ]
}
```

`type` accepts a mnemonic (`A`, `AAAA`, `CNAME`, `MX`, `TXT`, `NS`) or the
numeric code. Omitted defaults to `A`. `target` is required.

### `GET /v1/propagation?target=NAME&type=A`

Returns the classifier's verdict **for the resolvers actually observed**, with
the state named rather than a boolean.

```json
{
  "target": "example.com.",
  "state": "unresolvable",
  "state_code": 2,
  "reference": "",
  "divergent_resolvers": null,
  "responding_resolvers": 0,
  "divergence_seconds": 0
}
```

States and their meaning:

| State | Code | Meaning |
|---|---|---|
| `unknown` | 0 | no observations in the window |
| `insufficient_coverage` | 1 | fewer responding resolvers than `min_responding`; agreement would be meaningless |
| `unresolvable` | 2 | no successful authoritative observation — cannot say what is published |
| `divergent` | 3 | at least one resolver disagrees with the reference, beyond the convergence window |
| `converging` | 4 | partial agreement; the reference changed recently |
| `converged` | 5 | every responding resolver matches the reference |

> **There is no "propagated worldwide" state.** Agreement is reported only
> across observed resolvers. `PropagationState.IsPropagatedWorldwide()` exists
> solely to always return `false`, so a caller looking for that concept finds an
> explicit refusal instead of inventing one.

### `GET /v1/targets`

Lists tracked keys (`view/qname/qtype`).

---

## 4. Media Server — Data Plane ✅

Default bind: `127.0.0.1:8081` (`media.bind`).

### `GET /v1/media/{name}` ✅

| Condition | Response |
|---|---|
| No `Range` | `200` with the full body; `Accept-Ranges: bytes` |
| Valid single range | `206` with `Content-Range: bytes s-e/size` |
| Multipart range | `200` with the full body (RFC-permitted; counted) |
| Malformed or unsatisfiable range | `416` with `Content-Range: bytes */size` |
| `If-Range` with a stale validator | `200` with the full body |
| Non-indexed or traversal name | `404` |
| Method other than `GET`/`HEAD` | `405` with `Allow: GET, HEAD` |
| Admission limit reached | `429` with `Retry-After` |

Response headers on a `206` (verified live):

```
HTTP/1.1 206 Partial Content
Accept-Ranges: bytes
Content-Length: 100
Content-Range: bytes 100-199/1048576
Content-Type: application/octet-stream
Etag: "100000-18d58fb3667e0da0"
Last-Modified: Tue, 15 Sep 2026 17:47:07 GMT
```

`HEAD` returns identical headers with no body.

### `GET /healthz`, `GET /readyz` ✅

`/readyz` returns `503` until the index is built.

---

## 5. Conventions

- **Timestamps:** RFC 3339, UTC. JSON payloads use `Z` suffixes.
- **Caps:** every list endpoint is bounded. There is no endpoint that returns an
  unbounded collection.
- **Closed label sets:** identifiers that reach metrics come from a bounded
  enumeration, never from client input.
- **Peer attribution:** client IP for admission is taken from the connection,
  never from `X-Forwarded-For`.
- **Content type:** `application/json` for JSON planes; `application/x-ndjson`
  for ingest.

---

## 6. Not Yet Served

Documented here so the absence is explicit rather than surprising:

| Endpoint | Purpose |
|---|---|
| `GET /metrics` | Prometheus exposition on the admin plane 🔜 |
| `GET /v1/tls/reports` | Stored TLS probe reports 🔜 |
| `GET /v1/alerts` | Active alerts 🔜 |
| `GET /v1/media/_index` | Media inventory listing 🔜 |
| `GET /v1/dns/replay` | Segment replay 🔜 |
