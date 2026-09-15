# RIFT Security Model

This document is the threat model and the security posture of every externally
reachable component. It records what is **enforced in code today**, what is
covered by tests, and what is a known, accepted limitation.

> **Status honesty:** the controls marked ✅ are implemented and covered by
> tests. The items marked ⚠️ are implemented but deliberately limited, with the
> limitation stated. Items marked 🔜 are planned and are **not** claimed to
> exist. This project does not describe a control as present when it is not.

---

## 1. Trust Boundaries

| # | Boundary | Traffic | Authentication |
|---|---|---|---|
| B1 | client → LB data plane | untrusted | none (public LB); TLS optional |
| B2 | client → media server | semi-trusted LAN | none in v1 (documented below) |
| B3 | node → hub ingest | operator-controlled fleet | mTLS by default; explicit loopback-only plaintext opt-in for development |
| B4 | operator → admin plane | trusted | loopback-only by default; bearer token required once reachable remotely |
| B5 | monitor → third-party target | operator-configured | n/a (RIFT is the client) |

**B5 is the SSRF boundary.** Three components make outbound calls to
operator-supplied destinations (`tlsmon` probes, `dnsmon` resolver queries, and
the node→hub shipper). That is why outbound authorization is enforced **once**,
at the dialer, rather than at each call site.

---

## 2. SSRF: the `netx.Guard` ✅

Every outbound dial in this codebase goes through `netx.Guard.DialContext`.

```
resolve once  ─►  check EVERY resolved address  ─►  dial the literal IP
                   against the deny-set             (no re-resolution)
```

Denied by default:

| Range | Why |
|---|---|
| `127.0.0.0/8`, `::1/128` | loopback |
| `10/8`, `172.16/12`, `192.168/16` | private |
| `fc00::/7` | unique-local |
| `169.254/16`, `fe80::/10` | link-local, **including cloud metadata `169.254.169.254`** |
| `224/4`, `ff00::/8` | multicast |
| `0.0.0.0/32`, `::/128` | unspecified |
| `100.64/10` | CGNAT shared space |
| `240/4` | reserved |

Three properties matter and each has a test:

1. **Every answer is checked, not just the first.** A hostname that resolves to
   one public *and* one internal address is refused entirely. Cherry-picking the
   permitted answer would trust a name that half-points at internal space.
   (`TestGuardEveryAnswerChecked`)
2. **No re-resolution.** The hostname is resolved once, guarded, and then the
   **literal IP** is dialed. Re-resolving between the check and the dial is the
   classic DNS-rebinding TOCTOU. (`TestGuardResolvesOnce`)
3. **Fail closed on bad config.** An invalid allow-list prefix is a startup
   error, never a silently weaker guard. (`TestGuardInvalidConfigFailsClosed`)

⚠️ **Limitation, stated plainly:** the deny-set is a static prefix list. It does
not cover IPv4-mapped IPv6 forms beyond what `netip` normalizes, and it cannot
know about every routable-but-internal corporate range. Operators with unusual
topologies must extend the deny-set explicitly. The guard prevents the common
class of SSRF; it is not a substitute for network-level egress policy.

---

## 3. Load Balancer

### 3.1 Open-proxy prevention ✅

The upstream for every request comes **only** from the configured pool. An
`absolute-form` request target or a forged `Host` header never selects a
destination. The `Director` overwrites scheme and host from the chosen backend.

Covered by `TestL7NeverRoutesByHostHeader`, which configures the proxy with one
legitimate backend, sends a request whose `Host` names a malicious one, and
asserts the malicious backend is never contacted.

### 3.2 Request smuggling ✅

Two controls:

- HTTP framing stays stdlib-owned, so the parser is the well-tested one rather
  than a custom one. RIFT does not reimplement chunked encoding.
- A request carrying **both** `Content-Length` and `Transfer-Encoding` is
  rejected with `400` before any forwarding. Ambiguous framing at a proxy
  boundary is the canonical smuggling primitive.

Covered by `TestL7RejectsAmbiguousFraming`.

### 3.3 Header-spoofing prevention ✅

`X-Forwarded-For` is **append-only**. An inbound value is preserved only when
the peer is inside the configured `trusted_proxies` list; otherwise the inbound
header is discarded and replaced with the real peer address. With no configured
trusted proxies, nothing is trusted.

Covered by `TestL7ForwardedForAppendOnly` (spoof attempt is discarded) and
`TestL7TrustedProxyHonorsInboundXFF` (trusted peer is honored).

### 3.4 Retry safety ✅

Retry eligibility is a closed, method-and-phase table. Retrying a non-idempotent
request whose bytes already reached the backend is a data-corruption bug, not an
availability feature.

| Method | Connect-phase failure | Post-write failure |
|---|---|---|
| GET, HEAD, OPTIONS, TRACE | retry | retry |
| POST, PATCH | retry | **never** |
| PUT, DELETE | retry | only with explicit `idempotent_put_delete` opt-in |
| any other verb | **never** | **never** |

Covered by `TestRetryTable`, which asserts every cell including the
unknown-verb refusal.

### 3.5 Resource exhaustion ✅

- `max_conns` / `max_streams` are admission gates. Over the limit, the
  connection is **refused immediately** — never queued — because queueing
  converts overload into latency instead of a visible refusal.
- Per-operation read/write deadlines cull stalled peers (slowloris).
- Per-source and per-pool token buckets bound request rates. A per-connection
  bucket exists alongside the per-source bucket so a source-rotating flood still
  consumes a bounded budget.
- Oversized request bodies are rejected with `413` before any upstream work.

### 3.6 Admin plane ✅

- Binds **loopback by default**; a routable bind requires the explicit
  `allow_remote: true` opt-in, and validation refuses it otherwise. A mismatch
  between the configured bind and the opt-in is a startup error, not a warning.
- The mutating reload endpoint requires authorization. With no token configured,
  **only loopback peers** are accepted; any remote caller receives `401`. With a
  token configured, comparison is constant-time
  (`subtle.ConstantTimeCompare`).

Covered by `TestAdminReloadRequiresAuthFromNonLoopback`,
`TestAdminReloadWithToken`, and `TestAdminPlaneRefusesRoutableBind`.

---

## 4. DNS Monitoring

| Threat | Control | Status |
|---|---|---|
| Using RIFT as a DNS amplification reflector | Targets are operator-configured; per-resolver concurrency caps; query budgets | ✅ |
| Malformed/hostile DNS responses | Own parser with size caps, strictly-earlier compression pointers, no recursion | ✅ |
| Off-path response spoofing | One connected socket per in-flight query, `crypto/rand` transaction ID, question-section validation | ✅ |
| Response replay / mismatch | Transaction ID **and** question section must match before a response is accepted | ✅ |
| Unbounded memory from hostile keys | Hub window is capped per `(target, view)`; node ring has a fixed capacity | ✅ |
| Ingest flood | Batch cap, request body cap, per-node rate limiting | ✅ |
| Plaintext ingest exposure | mTLS required by default and **enforced on the listener**; the plaintext opt-in is refused on a routable bind and prints a startup warning | ✅ Tested: `TestHubIngestRequiresClientCertificate`, `TestHubIngestAcceptsValidClientCertificate` |
| Unauthenticated node posting observations | `RequireAndVerifyClientCert` against the configured CA; a certificate from a foreign CA is rejected at handshake | ✅ Tested: foreign-CA case in the suite above |
| Observation forgery by a *authenticated* node | Node identity bound from the verified certificate rather than the payload's `node_id` | 🔜 The payload field is still self-declared; certificate identity is not yet propagated into stored observations. |
| Missing or unusable CA material | Startup failure rather than an empty trust store | ✅ Tested: `TestHubTLSMissingMaterialFailsClosed`, `TestHubTLSRejectsEmptyCA` |

### 4.1 What mTLS now guarantees, and what it does not

⚠️ **DNS abuse limitation:** RIFT queries only operator-configured targets. It
cannot be pointed at an arbitrary zone by a remote caller. The amplification
surface is therefore the operator's own configured query volume, not an
attacker's.

**Guaranteed:** an ingest connection is accepted only from a client presenting
a certificate that chains to the configured `client_ca_file`. An unauthenticated
client, or one presenting a certificate from a different CA, fails at the TLS
handshake — verified by tests that assert the connection is refused, not merely
that a valid client succeeds.

**Not yet guaranteed:** the stored `NodeID` still comes from the JSON payload
rather than from the certificate's subject. An authenticated node could claim
another node's identity. This matters only when nodes are semi-trusted; the
correct posture for now is to issue one certificate per node and treat the CA
boundary as the trust boundary. The fix is to derive `NodeID` from the verified
peer certificate, which is a small change to the ingest handler.

For local development, `rift.hub.dev.yaml` sets `allow_plaintext_ingest: true`
and binds loopback. Validation **refuses that option on any routable bind**, the
hub prints a warning naming the port at startup, and `rift dns hub` reports the
mode as `PLAINTEXT (development only)` in its banner. A loopback listener is
still unauthenticated to anything else on the same host.

---

## 5. TLS Monitoring

### 5.1 The verification inversion ✅

`tlsmon` performs the handshake with verification **disabled**, then verifies
manually. This is deliberate and is the entire point of the component: a
certificate that fails verification is a *finding to report*, not an error that
aborts the probe. A monitoring tool that dies on the condition it monitors is
useless.

The manual verification performs, independently:

- chain construction and trust evaluation against the configured root store;
- **hostname verification separately** — this matters because `x509.Verify`
  returns an authority error before it ever reaches the hostname check, so an
  untrusted-root finding would otherwise mask a wrong-host finding. A
  wrong-host certificate is a critical finding on its own.

Covered by `TestProbeHostnameMismatch` (which caught exactly this masking bug),
`TestProbeExpiredCert`, `TestProbeWeakRSAKey`, and
`TestProbeValidSelfSignedReportsSelfSigned`.

### 5.2 Findings are a closed set ✅

Finding codes are a fixed enumeration (`EXPIRED`, `NOT_YET_VALID`,
`HOSTNAME_MISMATCH`, `UNTRUSTED_ROOT`, `INCOMPLETE_CHAIN`, `SELF_SIGNED`,
`WEAK_KEY`, `WEAK_SIG`, `OLD_TLS`, `HANDSHAKE_REFUSED`, `CONN_REFUSED`,
`CONN_TIMEOUT`, `PROTO_MISMATCH`, `NO_PEER_CERT`). A free-form code would become
an unbounded metric label, and an unbounded label is a memory-exhaustion
primitive.

### 5.3 SSRF ✅

Probe targets dial through `netx.Guard`, so a default configuration cannot be
used to port-scan loopback or internal address space.
(`TestProbeSSRFGuardRefusesInternal`)

---

## 6. Media Server

| Threat | Control | Status |
|---|---|---|
| Path traversal | Names are rejected on the **original** string and never normalized into a valid lookup. Absolute paths, backslashes, and `..` segments are refused. | ✅ |
| TOCTOU between index and file | The file is opened, then **the open descriptor** is stat-ed; what is served is the FD, not the path. | ✅ |
| Metadata oracle | Uniform error surface for non-indexed names. | ✅ |
| Unbounded memory | Per-stream memory is bounded by configuration and independent of file size. | ✅ |
| Slow clients | Write deadlines via `http.ResponseController`; stalled streams counted and culled. | ✅ |
| Oversized headers | `MaxHeaderBytes` and a `431`/`400` response. | ✅ |

Covered by `TestMediaPathTraversal` (which caught a real bug: an absolute path
was being silently rewritten into a relative lookup instead of being refused),
`TestMediaRangeRequests`, `TestMediaAdmissionLimits`, and
`TestClientIPIgnoresForwardedHeader`.

⚠️ **Accepted v1 limitation — no authentication.** The media server is designed
for a trusted LAN and performs no authentication. Any client that can reach the
port can read any indexed file. **Do not expose it to an untrusted network.**
Token authentication is a planned milestone.

---

## 7. Secrets and Configuration ✅

- **Secrets are never inline.** TLS key files, admin tokens, and hub client keys
  are file paths or environment references. The schema has no field for inline
  key material.
- **Redaction is applied at the source.** `config.Redacted` is applied before
  any schema is rendered — diffs, the admin API, error messages. The diff is
  computed over redacted copies, so a diff cannot leak a secret.
- **Environment overrides are a closed set.** An undocumented `RIFT_*` variable
  does nothing, rather than silently taking effect.
- **Strict parsing.** Unknown YAML fields are a hard error.

Covered by `TestRedactionHidesSecrets` and
`TestRedactedDiffNeverLeaksSecrets`.

---

## 8. Input Validation Summary

| Input | Validation | Fail mode |
|---|---|---|
| YAML config | strict decode, per-field validation with paths | exit 2, all violations listed |
| Listener `bind` / backend `addr` | `host:port`, port range | config error |
| Resolver addresses | must be literal `IP:port` (no hostnames) | config error |
| TLS probe targets | hostname/IP grammar; dial through Guard | finding / config error |
| HTTP `Range` header | RFC 7233 strict; malformed → `416`, never a silent `200` | 416 |
| Asset names | traversal refusal on the raw string | 404 |
| DNS wire messages | size caps, pointer monotonicity, label limits | discarded |
| Ingest payloads | timestamp, view, qname length, batch cap | 400 / 413 |

---

## 9. Test Coverage of Security Controls

| Control | Test |
|---|---|
| SSRF deny-set (14 ranges) | `TestGuardDeniesDefaultSet` |
| Multi-answer / split-horizon | `TestGuardEveryAnswerChecked` |
| DNS rebinding window | `TestGuardResolvesOnce` |
| Metadata address | `TestDefaultDenySetIncludesMetadata` |
| Open-proxy refusal | `TestL7NeverRoutesByHostHeader` |
| Smuggling (CL+TE) | `TestL7RejectsAmbiguousFraming` |
| XFF spoofing | `TestL7ForwardedForAppendOnly` |
| Retry safety table | `TestRetryTable` |
| Admin auth (remote / token) | `TestAdminReloadRequiresAuthFromNonLoopback`, `TestAdminReloadWithToken` |
| Admin routable bind | `TestAdminPlaneRefusesRoutableBind` |
| Path traversal | `TestMediaPathTraversal`, `TestSanitizeAssetName` |
| XFF-based admission bypass | `TestClientIPIgnoresForwardedHeader` |
| TLS hostname masking | `TestProbeHostnameMismatch` |
| TLS SSRF | `TestProbeSSRFGuardRefusesInternal` |
| Secret redaction | `TestRedactionHidesSecrets`, `TestRedactedDiffNeverLeaksSecrets` |
| Plaintext ingest on routable bind | `TestHubPlaintextIngestRefusedWhenRoutable` |
| mTLS required by default | `TestHubRequiresMTLSByDefault`, `TestHubTLSMissingMaterialFailsClosed` |
| Ingest refuses an unauthenticated client | `TestHubIngestRequiresClientCertificate` |
| Ingest refuses a foreign-CA client | `TestHubIngestRequiresClientCertificate` (second case) |
| Ingest accepts a properly signed client | `TestHubIngestAcceptsValidClientCertificate` |
| Empty trust store rejected | `TestHubTLSRejectsEmptyCA` |
| Unbounded key cardinality | `TestBoundedCardinalityUnderFlood` |
| Unbounded DNS window | `TestWindowBoundsMemory` |
| Ring memory bound | `TestRingMemoryIsBounded` |

---

## 10. Known Limitations and Non-Goals

Stated so that no reader mistakes absence for coverage:

1. **No authentication on the media server** (§6). LAN-only by design.
2. **Node identity is not yet bound to certificates** (§4). Run ingest on a
   trusted network until it is.
3. **No rate limiting on the media server's per-client bucket by default** —
   the field exists and is applied when configured; the default is off so that
   LAN playback is not throttled unexpectedly.
4. **No CSRF protection, no sessions, no cookies** — RIFT has no browser-facing
   authenticated surface.
5. **No audit log** beyond the structured logs. A dedicated audit trail is a
   planned milestone.
6. **No DNSSEC validation.** The wire engine parses what it is given; it does
   not establish authenticity.
7. **The deny-set is static** (§2).
8. **TLS 1.0/1.1 scanning is opt-in** and off by default: probing deprecated
   versions by default would make RIFT itself the weak-handshake client.

---

## 11. Reporting a Vulnerability

This is a reference implementation, not a deployed service. If you find a
security defect, open an issue describing the affected component, the minimal
reproduction, and the impact. Please do not include real credentials or
third-party data in a report.
