<div align="center">

# RIFT

**A high-performance network operations platform in Go**

A load balancer, a DNS and TLS monitoring system, and a media server —
built around one shared platform layer, and measured rather than asserted.

[![Go](https://img.shields.io/badge/Go-1.25.5-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Tests](https://img.shields.io/badge/tests-race%20clean-brightgreen)](#testing)
[![Status](https://img.shields.io/badge/status-verified%20%2F%20unmeasured%20perf-orange)](#status)

</div>

---

## What this is

RIFT is one Go module producing one binary that runs three network services:

| Component | What it actually does |
|---|---|
| **Load balancer** | TCP proxying with admission control and half-close handling; UDP proxying with per-session backend affinity; HTTP reverse proxying with an owned transport. Three pickers, active health checks with hysteresis, a closed retry-safety table, and hot reload that never drops established connections. |
| **DNS & TLS monitoring** | Its own RFC 1035 wire engine (the stdlib exposes no TTLs), a per-resolver engine with UDP→TCP fallback, a six-state propagation classifier that refuses to claim global propagation, a node→hub pipeline with bounded memory and counted drops, and a TLS prober that reports findings instead of aborting on them. |
| **Media server** | RFC 7233 single-range serving with strong ETags, `If-Range`, admission limits, and memory per stream bounded by configuration rather than file size. |

The load balancer, DNS node, and media server have each been **verified running
end to end** against real backends, real resolvers, and live TLS endpoints.

## Status

This project draws a hard line between *verified* and *claimed*.

| Area | Status |
|---|---|
| Build, vet, `-race` tests | ✅ green |
| Security controls | ✅ implemented and red-line tested — see [`SECURITY.md`](SECURITY.md) §9 |
| LB / DNS / TLS / media forwarding | ✅ implemented, tested, and verified running end to end |
| Retry, rate limiting, graceful drain, `/metrics` | ❌ **implemented and tested, but not wired into the services — see “Implemented but not yet wired” below** |
| **Performance numbers** | ❌ **none published — no suitable benchmark host yet** |

There are no throughput or latency figures in this repository. Every target in
[`PERFORMANCE.md`](PERFORMANCE.md) is marked `[unmeasured]`, and the benchmark
tooling actively refuses to produce a report without an environment card:

```console
$ rift bench report nosuchdir
rift: bench.report: refusing to produce a report without an environment card
```

That refusal is the point. A number without its environment is not a fact.

## Quick start

```bash
git clone https://github.com/abhrajyoti-01/rift.git
cd rift

go build ./...
go vet ./...
go test -race ./...

go run ./cmd/rift version
```

Write and validate a configuration:

```bash
go run ./cmd/rift init --config rift.yaml
go run ./cmd/rift config validate --config rift.yaml
```

## Try it against real services

Each of these performs a genuine network operation:

```bash
# Real DNS query to a real resolver
go run ./cmd/rift dns query example.com --type A --resolver 1.1.1.1:53

# Real TLS handshake and chain inspection
go run ./cmd/rift tls check example.com --floor 1.2

# Real environment card for the current host
go run ./cmd/rift bench env
```

Example TLS output:

```
target example.com:443
protocol TLS 1.3  cipher TLS_AES_128_GCM_SHA256
subject  CN=example.com
issuer   CN=Cloudflare TLS Issuing ECC CA 3,O=SSL Corporation,C=US
expires  2026-10-27T22:17:21Z (42 days)
san      example.com
san      *.example.com
findings none
```

## Run the services

```bash
# Load balancer: TCP, UDP, and HTTP listeners plus a loopback admin plane
go run ./cmd/rift lb --config rift.example.yaml

# DNS monitoring: start the hub, then any number of nodes.
# rift.hub.dev.yaml is loopback-only and enables plaintext ingest for local
# use; a real deployment uses mTLS (see SECURITY.md section 4).
go run ./cmd/rift dns hub --config rift.hub.dev.yaml
go run ./cmd/rift dns node --config rift.example.yaml

# Media server with a live index that rebuilds on SIGHUP.
# Generate the fixture first
go run ./scripts/makefixture
go run ./cmd/rift media --config rift.example.yaml
```

Verified end to end:

```console
$ curl http://127.0.0.1:8080/hello          # → through the LB
backend-ok path=/hello

$ curl -r 100-199 -D- -o/dev/null \
    http://127.0.0.1:8081/v1/media/sample.bin
HTTP/1.1 206 Partial Content
Accept-Ranges: bytes
Content-Length: 100
Content-Range: bytes 100-199/1048576
Etag: "100000-18d58fb3667e0da0"

$ curl 'http://127.0.0.1:19002/v1/observations?target=example.com.&type=A'
{"count":1,"observations":[{"NodeID":"test-node","Resolver":"1.1.1.1:53",
 "Answers":[{"TTL":136,"Data":"104.20.23.154"}],"Latency":31656000}]}
```

## Architecture at a glance

```
                        ┌──────────────────────────────┐
                        │          cmd/rift            │
                        │  composition root only       │
                        └──┬────────┬────────┬─────────┘
                           │        │        │
                  ┌────────▼──┐ ┌───▼────┐ ┌─▼───────┐ ──────────┐
                  │     lb    │ │ dnsmon │ │ tlsmon  │ │  media   │
                  │ l4 udp l7 │ │node hub│ │ probe   │ │ server   │
                  │picker hlth│ │resolver│ │         │ │          │
                  │  control  │ │  wire  │ │         │ │          │
                  ─────┬─────┘ ───┬────┘ ────┬──── └────┬─────┘
                        │           │           │           │
   ═════════════════════▼═══════════▼═══════════▼═══════════▼═════════
                     internal/platform  (dependency sink)
     errs · config · netx(SSRF guard) · pool · ratelimit · circuit
     retry · lifecycle · logging · metrics · health · httpx
   ════════════════════════════════════════════════════════════════════
```

Subsystems never import each other; `cmd/rift` wires them. Full detail,
including data-flow diagrams and the concurrency model, is in
[`ARCHITECTURE.md`](ARCHITECTURE.md).

## Documentation

| Document | Contents |
|---|---|
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Component boundaries, data flow, concurrency, failure modes |
| [`PRD.md`](PRD.md) | Requirements with an honest status for every one |
| [`SECURITY.md`](SECURITY.md) | Threat model, trust boundaries, control status, limitations |
| [`API.md`](API.md) | HTTP contracts with real request and response bodies |
| [`CLI.md`](CLI.md) | Every command, every flag, every exit code |
| [`OBSERVABILITY.md`](OBSERVABILITY.md) | Metrics, logging, health, dashboards |
| [`TESTING.md`](TESTING.md) | Test strategy and the defects the tests caught |
| [`PERFORMANCE.md`](PERFORMANCE.md) | Benchmark methodology and why there are no numbers yet |
| [`ROADMAP.md`](ROADMAP.md) | Phases, milestones, definition of done |

## Repository layout

```
cmd/rift/                 CLI entry point and wiring
internal/platform/        shared substrate (errs, config, netx, pool, ...)
internal/lb/              model, picker, health, l4, udp, l7, control
internal/dnsmon/          wire, resolver, probe, model, node, hub
internal/tlsmon/          model, probe
internal/media/           model, server
internal/bench/           env, loadgen, playersim, harness
bench/scenarios/          versioned load definitions
experiments/              toolchain probe programs
scripts/                  local development helpers
```

## Security highlights

Implemented and covered by red-line tests (see [`SECURITY.md`](SECURITY.md)):

- **SSRF guard at the dialer** — resolves once, checks *every* resolved address,
  dials the literal IP. Multi-answer hostnames are refused entirely; there is no
  re-resolution window.
- **No open proxy** — the upstream comes only from the configured pool; a forged
  `Host` header never selects a destination.
- **Smuggling defense** — `Content-Length` + `Transfer-Encoding` co-presence is
  rejected before forwarding, and HTTP framing stays stdlib-owned.
- **Admin plane** — loopback by default; routable binds require an explicit
  opt-in; reload requires authorization with constant-time comparison.
- **Hub ingest** — mTLS enforced on the listener
  (`RequireAndVerifyClientCert`): an unauthenticated client, or one whose
  certificate chains to a different CA, fails at the handshake. Node identity
  binding from the certificate is the remaining gap, stated in
  [`SECURITY.md`](SECURITY.md) §4.1.
- **Path traversal** — refused on the raw string, never normalized into a valid
  lookup.
- **Secrets** — file paths only, never inline; redaction applied before any
  schema is echoed, including diffs.

### Implemented but not yet wired

Honesty requires naming these, because configuration exposes them and they have
no effect:

| Feature | State |
|---|---|
| Retry policy (`retry.max_attempts`, `idempotent_put_delete`) | The closed method/phase table is implemented and tested, but the proxy never calls it. No request is retried. Safe (fewer retries than configured), but not the advertised feature. |
| Rate limiting (`rate_limit.per_source`, `per_pool`) | The sharded limiter is implemented and tested, but no service imports it. The configured limits are ignored. |
| Graceful drain (`shutdown_timeout`, exit code 4) | The `lifecycle` package is implemented and tested, but `rift lb` does not use it. In-flight connections are closed rather than drained. |
| Prometheus `/metrics` | Counters are maintained per component; nothing serves them yet. |

These are the consequences of this project's own rule: a feature counts as
working only when a test exercises it **through the code path a user actually
reaches**. Each row above has thorough unit tests and no production call site —
which is exactly the gap that rule exists to expose.

Known limitations are listed in [`SECURITY.md`](SECURITY.md) §10 — including the
deliberate absence of media authentication (LAN-only) and the not-yet-enforced
mTLS identity binding for hub ingest.

## Testing

```bash
go build ./... && go vet ./... && go test -race ./...
```

Tests use real loopback sockets rather than mocks and never depend on the public
internet. [`TESTING.md`](TESTING.md) §4 lists the real defects these tests found
during development, including an absolute-path traversal bug, a breaker state
machine that could never recover, a shutdown path that silently dropped data,
and a TLS finding that masked a hostname mismatch.

## Requirements

- Go 1.25.5 or newer
- Linux for production and for all performance work
- Windows and macOS for development and functional tests

## License

Apache License 2.0 — see [`LICENSE`](LICENSE).

---

<div align="center">
<sub>Everything in this repository is either verified or labelled as unverified.
There is no third category.</sub>
</div>