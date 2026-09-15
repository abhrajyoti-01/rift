# RIFT CLI Reference

The binary is `rift`. Configuration is delivered by file; flags select behaviour.

```
Usage: rift <command> [flags]
```

Global conventions:

- `--config PATH` selects the configuration file. Resolution order is
  `--config`, then `$RIFT_CONFIG`, then `./rift.yaml`.
- Consistent exit codes across every command (§5).
- Errors print the offending **field path** for configuration problems, and list
  **every** violation rather than only the first.

---

## 1. Command Surface

| Command | Purpose | Status |
|---|---|---|
| `rift version` | Print build information | ✅ |
| `rift init` | Write a starter `rift.yaml` | ✅ |
| `rift config validate` | Validate a config file | ✅ |
| `rift config diff` | Field-path diff between two config files | ✅ |
| `rift lb` | Run the load balancer (TCP/UDP/HTTP) | ✅ |
| `rift dns query` | One-shot live DNS query | ✅ |
| `rift dns node` | Run a DNS monitoring node | ✅ |
| `rift dns hub` | Run the aggregation hub | ✅ |
| `rift tls check` | One-shot live TLS probe | ✅ |
| `rift media` | Run the media server | ✅ |
| `rift bench env` | Emit the environment card | ✅ |
| `rift bench scenarios` | List available scenarios | ✅ |
| `rift bench run` | Run a scenario, write samples | ✅ |
| `rift bench compare` | Tolerance-band comparison | ✅ |
| `rift bench report` | Render a report from samples | ✅ |
| `rift metrics` | Dump live metrics | 🔜 |
| `rift health` | Probe a running service | 🔜 |

---

## 2. Commands

### `rift version`

```
$ rift version
rift 0.0.0-dev (commit unknown, go1.25.5)
```

### `rift init`

Writes a commented starter configuration. The generated file is guaranteed to
validate — a starter that teaches an invalid example is worse than none.

```
rift init [--config rift.yaml] [--force]
```

Refuses to overwrite an existing file without `--force`.

### `rift config validate`

Runs exactly the validation that service startup runs, so CI checks the file an
operator would actually deploy.

```
$ rift config validate --config rift.example.yaml
config valid: rift.example.yaml
```

On failure it lists every violation with its path and exits `2`:

```
$ rift config validate --config broken.yaml
rift: config.validate: 3 configuration violation(s):
  - lb.listeners[0].proto: must be tcp|udp|http, got "spud"
  - lb.listeners[0].pool: references unknown pool "web"
  - lb.pools[0].backends[0].addr: must be host:port (got "127.0.0.1")
exit status 2
```

### `rift config diff`

Compares two configuration files by field path. Both sides are redacted first,
so a diff can never leak a secret.

```
rift config diff --config old.yaml --against new.yaml
```

### `rift lb`

Runs every configured listener. TCP, UDP, and HTTP listeners are started
concurrently; health checking runs per pool; `SIGHUP` reloads.

```
$ rift lb --config rift.example.yaml
lb: http 127.0.0.1:8080 → pool web_pool
lb: tcp  127.0.0.1:9022 → pool web_pool
lb: admin 127.0.0.1:9000 (loopback only)
```

Admin endpoints (loopback unless `allow_remote` is set):

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/snapshot` | Current configuration snapshot |
| `GET` | `/v1/pools` | Pools with per-backend health and connection counts |
| `POST` | `/v1/admin/reload` | Trigger reload (same path as `SIGHUP`) |

Reload requires authorization: loopback peers when no token is configured, or
a bearer token once remote access is enabled.

### `rift dns query`

Performs a real wire query against a real resolver. No caching, no synthetic
answers.

```
$ rift dns query example.com --type A --resolver 1.1.1.1:53
;; example.com A @1.1.1.1:53 (30.7 ms)
example.com.                        9 IN A      172.66.147.243
example.com.                        9 IN A      104.20.23.154
;; rcode=NOERROR aa=false tc=false
```

The `NAME@RESOLVER` shorthand is accepted: `rift dns query example.com@8.8.8.8`.

Supported types: `A`, `AAAA`, `CNAME`, `MX`, `TXT`, `NS`.

### `rift dns node`

Runs the monitoring node: schedules probes against configured resolvers, buffers
observations in a bounded ring, and ships batches to the hub.

```
$ rift dns node --config rift.example.yaml
dns node: test-node (test-lab) → http://127.0.0.1:19001
```

### `rift dns hub`

Runs the aggregation hub: mTLS ingest plane and query plane.

```
$ rift dns hub --config rift.hub.yaml
dns hub: ingest 127.0.0.1:19001 query 127.0.0.1:19002 data ./bench/results/hubdata
```

Query endpoints:

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/ingest` | Node observation batches (JSON lines) |
| `GET` | `/v1/observations?target=X&type=A` | Observations, capped at 1000 |
| `GET` | `/v1/propagation?target=X&type=A` | Classifier output with its state |
| `GET` | `/v1/targets` | Tracked keys |
| `GET` | `/healthz`, `/readyz` | Liveness, readiness |

### `rift tls check`

Performs a real TLS handshake and reports findings.

```
$ rift tls check example.com --floor 1.2
target example.com:443
protocol TLS 1.3  cipher TLS_AES_128_GCM_SHA256
subject  CN=example.com
issuer   CN=Cloudflare TLS Issuing ECC CA 3,O=SSL Corporation,C=US
expires  2026-10-27T22:17:21Z (42 days)
san      example.com
san      *.example.com
findings none
```

Flags: `--floor 1.2|1.3`, `--strict` (exit non-zero on any finding),
`--json` (machine-readable report).

### `rift media`

Serves the media root with RFC 7233 range support.

```
$ rift media --config rift.example.yaml
media: 127.0.0.1:18081 serving ./bench/fixtures (1 assets)
```

`SIGHUP` rebuilds the index and atomically swaps it.

### `rift bench`

```
$ rift bench env
Environment card (generated 2026-09-15T17:41:42Z)
  os/arch        windows/amd64
  go             go1.25.5 (gc)
  cpus           12 (GOMAXPROCS 12)
  wsl2           false
  note           Windows host: no performance claims may be derived from this card
```

The card **declares when a host is unsuitable for performance claims**. Reports
cannot be produced without one.

```
$ rift bench scenarios
lb.http.closed.200
lb.http.small.1k
media.range.4k

$ rift bench run lb.http.closed.200
scenario lb.http.closed.200 (rift, closed loop)
  target       http://127.0.0.1:8080/
  completed    142033
  errors       0
  throughput   4734.4 req/s
  p50/p95/p99  169µs / 812µs / 2.1ms

$ rift bench compare summary.json baseline.json
p99_ns               baseline=1000000 actual=2100000 delta=+110.0% band=±10% regression

$ rift bench report bench/results/lb.http.closed.200-20260915T174005
wrote bench/results/lb.http.closed.200-20260915T174005/report.md
```

---

## 3. Scenario Definition

Scenarios are JSON files under `bench/scenarios/`:

```json
{
  "name": "lb.http.small.1k",
  "tool": "rift",
  "target": "http://127.0.0.1:8080/",
  "duration_ms": 30000,
  "warmup_ms": 5000,
  "loop": "open",
  "conns": 64,
  "rate_per_sec": 2000,
  "method": "GET",
  "keep_alive_fraction": 1.0
}
```

`loop: "open"` schedules each request at its intended deadline regardless of
completion, so a stalled response does not silently hide queueing delay. Both
intended and actual start times are recorded in the raw samples.

---

## 4. Configuration Delivery

Environment overrides are a **closed set**. An undocumented `RIFT_*` variable
has no effect.

| Variable | Field |
|---|---|
| `RIFT_OBSERVABILITY_LOG_LEVEL` | `observability.log_level` |
| `RIFT_OBSERVABILITY_ADMIN_ADDR` | `observability.admin_addr` |
| `RIFT_OBSERVABILITY_ALLOW_REMOTE` | `observability.allow_remote` |
| `RIFT_DNS_NODE_ID` | `dns.node_id` |
| `RIFT_DNS_LOCATION` | `dns.location` |
| `RIFT_DNS_HUB_URL` | `dns.hub.url` |
| `RIFT_MEDIA_ROOT` | `media.root` |
| `RIFT_MEDIA_BIND` | `media.bind` |
| `RIFT_BENCH_RESULTS_DIR` | `bench.results_dir` |
| `RIFT_CONFIG` | config file path (not a field) |

---

## 5. Exit Codes

| Code | Meaning | Supervisor action |
|---|---|---|
| 0 | success | — |
| 1 | runtime fault | restart |
| 2 | configuration invalid | **fix the file, do not restart** |
| 3 | bind / resource failure | restart; check ports and limits |
| 4 | shutdown drain deadline exceeded | restart; inspect what was in flight |

The distinction between `2` and `3` is deliberate: a supervisor that restarts a
process with a bad config will loop forever.