# SECURITY_SPEC.md — RIFT Security Model

Status: **Revision A**. Owning requirement: NFR-11, plus security posture rows
across component specs. Threat-model per externally reachable component. The
word "hardened" is banned below without a test ID next to it.

---

## 1. Trust Boundaries

| # | Boundary | Traffic | Authentication |
|---|---|---|---|
| B1 | client → LB data plane (80/443, raw TCP/UDP) | untrusted | none (public LB); TLS optional |
| B2 | client → media server | semi-trusted LAN | none in v1 (token: milestone) |
| B3 | node → hub ingest | operator-controlled fleet | mTLS client certs |
| B4 | operator → admin/control plane | trusted, loopback default | token (remote requires `allow_remote`+token) |
| B5 | hub/tlsmon/dnsmon → monitored targets | operator-configured destinations | n/a (we are the client) |
| B5 is the SSRF boundary | — | — | `netx.Guard` at the dial |

Three components make **outbound** calls to operator-supplied destinations —
`tlsmon` (dial target:443), `dnsmon` authoritative NS resolution, node → hub.
That fact drives the model: the SSRF surface is the central risk, and it is
enforced **once, at the dialer** (AD-10), not per call site — per-site
validation is exactly the pattern that produces TOCTOU rebinding bugs.

## 2. SSRF Deny-Set — `platform/netx.Guard`

Default deny (v1): loopback v4/v6 (`127/8`, `::1`), private v4 (`10/8`,
`172.16/12`, `192.168/16`) + ULA v6 (`fc00::/7`), link-local v4/v6
(`169.254/16`, `fe80::/10` — includes cloud metadata `169.254.169.254`),
multicast, unspecified, reserved. Operator allow-list (explicit prefixes)
overrides deny **for explicitly configured targets only**, never for
client-influenced addresses. Dial algorithm:

1. Resolve hostname (if any) **once**.
2. Guard **every** resolved address (not just the first — multi-answer DNS is
   the classic guard bypass).
3. Dial the **literal IP** (no re-resolution — closes the rebinding window),
   SNI/Host preserved from the original name.
4. Denied → `errs.ClassSecurity`, warn-level log with target and matched
   deny-rule, **never retried**, metric `rift_netx_guard_denied_total{rule}`.

LB backends are operator-configured and exempt from the deny-set (B1 trust:
the operator chose them) — but client-supplied destinations (absolute-form
URIs, `Host`-based routing) are **never** exempt: that distinction *is* the
open-proxy boundary. Enforced by depguard + review rule: no subsystem calls
`net.Dial` directly; all outbound dials go through `Guard.DialContext` (CI
grep T-35).

## 3. Per-Component Threat Model

### 3.1 LB (B1)

| Threat | Vector | Control | Test |
|---|---|---|---|
| Open proxy | absolute-form URI, forged `Host` selects upstream | upstream from configured pool only; `Host` never routes | T-36 |
| Request smuggling (CL.TE/TE.CL) | ambiguous framing at the proxy boundary | stdlib framing + reject CL+TE-before-forwarding | T-37 |
| Back-to-front forgery | spoofed `X-Forwarded-For` | append-only; attribution only from `trusted_proxies` | T-38 |
| Slowloris | idle conns | per-read deadline, `IdleTimeout`, admission gate | T-39 |
| Resource exhaustion | conn flood | `max_conns` < rlimit−margin, refusal + counter | T-40 |
| UDP session-table bomb | spoofed-source flood | `max_sessions` gate + idle sweep + drop counters | T-42 |
| Rate-limit bypass | source rotation/spoof | per-connection bucket in addition to per-IP; per-pool budget | T-45 |
| Oversized requests | huge bodies/headers | `max_body_bytes`, `MaxHeaderBytes`, early 413 | T-46 |
| TLS downgrade/weak ciphers | old clients | floor 1.2, modern cipher suites (Go defaults + floor), session tickets off by default | T-47 |
| Unknown methods | arbitrary verbs | reject 501 (LB-L7 posture; not something to forward on a client's behalf) | T-48 |
| ALP in `Forwarded`/XFF | header injection chars | RFC 7239 grammar validation before emit | T-49 |

### 3.2 DNS/TLS monitor (B5 outbound + B3 ingest)

| Threat | Vector | Control | Test |
|---|---|---|---|
| SSRF to internal DNS | target zone/NS resolution | Guard on all dials, incl. authoritative NS resolution | T-50 |
| DNS abuse (amplification via RIFT) | using RIFT as a reflection amplifier | operator-configured targets only; per-resolver concurrency caps; query budget | T-73 |
| Malformed responses | hostile/spoofed DNS answers | fuzzed wire parser; size caps; pointer budget; `ClassSecurity` | T-53 |
| Cache-poisoning observation | off-path spoofed responses | per-query socket, random ID + ephemeral port | T-54 |
| Node impersonation | forged observations at hub | mTLS client certs; node ID from cert CN | T-55 |
| Hub ingest flood | oversized/rapid batches | batch cap 4096, admin-plane rate limit, 429 | T-56 |
| TLS target probing misuse | scanning internal infra | Guard deny-set on targets; allow-list explicit | T-50 |
| Query-name injection into logs | hostile qname | 253-char cap, sanitized log fields | T-74 |
| Replay of old observations | batch replay | idempotency key + hub-side dedupe | T-75 |
| Segment tampering | local disk access | out of scope v1 (documented; disk access = host compromise) | — |

### 3.3 Media (B2)

| Threat | Vector | Control | Test |
|---|---|---|---|
| Path traversal | `..`, absolute, backslash, NUL, symlink escape | canonicalize-then-prefix-check; `follow_symlinks: false` default | T-82 |
| Range-parse abuse | crafted Range floods | fuzzed parser; per-client rate limit; closed outcome set | T-84 |
| FD exhaustion | open + stall | `max_streams` + per-client caps + write deadlines | T-85, T-86 |
| Bandwidth theft / abuse | unauthenticated LAN | accepted v1 risk (semi-trusted LAN, PRD §4); token auth = milestone | — |
| Timing/file-existence oracle | 416 vs 404 differences | Uniform error surface; asset names from index only | T-89 |
| Header abuse | oversized headers | `MaxHeaderBytes`, 431 | T-89b |

### 3.4 Admin plane (B4)

| Threat | Vector | Control | Test |
|---|---|---|---|
| Log-field injection / cardinality abuse | hostile values reaching log fields | closed log field set + value caps (OBSERVABILITY_SPEC §2) | T-66 |
| Unauthorized reload | remote POST | loopback default; `allow_remote`+token; refusal to start otherwise | T-67 |
| Secret leakage via config dump | `rift config diff`/snapshot | Redaction at source: secrets are file paths/env refs, never inline; redact function applied to any config echo | T-93 |
| Metrics exposure | scrape from LAN | admin listener loopback default; `allow_remote` gated | T-69 |
| Admin scan from LAN | routable bind | refuse-to-start without explicit `allow_remote: true` | T-70 |

### 3.5 Hub (B3)

| Threat | Vector | Control | Test |
|---|---|---|---|
| Forged observations | non-mTLS poster | mTLS required; CN=NodeID | T-55 |
| Ingest DoS | flood | batch cap, rate limit, 429, ring-bounded memory (NFR-9) | T-56 |
| Alert spam | divergence flapping | convergence window + hysteresis via rise/fall-style thresholds | T-71 |
| Query API abuse | unbounded queries | pagination + result caps + closed targets | T-72 |

## 4. Request Smuggling Posture

RIFT-L7 places a stdlib-owned parser at the boundary (AD-3) and adds a
pre-forward reject for CL+TE co-presence. Known residual risk accepted in v1:
desync via header folding weirdness is a stdlib-parser responsibility; RIFT's
contract is "no *worse* than stdlib framing" plus the pre-forward reject.
Backend protocol selection is explicit config (`h2c` off by default — AD-3),
removing the classic h2c-smuggling amplifier. Cleartext H2 to backends
requires opt-in and logs a warning naming the risk.

## 5. Rate Limiting and Abuse Convergence

Per-source (IP) + per-connection + per-pool token buckets — three layers, each
defeating a different bypass: spoofed sources (per-conn), rotating sources
(per-pool), and honest overload (per-source). UDP: no connection exists, so
per-source + global packet budget; drop-with-counter. Dropped/malformed
share a closed `reason` enum — security telemetry must be queryable, not
exhausting.

## 6. Security Testing (T-IDs summarized; full method in TESTING_SPEC)

Fuzz targets (CI nightly + release gate): DNS wire parser (corpus: malformed,
pointer-loop, oversized, truncated), Range header parser, config loader
(embedded secrets redaction), YAML bomb (alias-anchor depth cap — `yaml.v3`
handles nesting, we cap depth in our own walk), reload atomicity.

Red-line tests that must never regress: open-proxy (T-36), SSRF deny-set
(T-50), traversal (T-82), admin exposure (T-70), mTLS enforcement (T-55),
secret redaction (T-93). A red-line failure is a release blocker by policy.

## 7. Secrets and Config Security

- Secrets never inline in YAML (schema refuses `cert_file`-adjacent inline
  material fields): TLS key material, hub client keys, admin tokens are file
  paths or env refs. `rift config validate` refuses inline secrets at
  validation (before any dial).
- File perms checked at load: 0600 on secret files (non-fatal warning on
  Windows where POSIX perms don't map; fatal on Linux).
- Redaction: `config.Redacted()` rendering used by every echo path (diff,
  snapshot API, error messages); test T-93 greps output for known secret
  material.
- No secrets in metrics (labels are closed sets), logs (closed field set),
  or error strings (error text travels — secret-bearing errors are a
  ClassConfig bug).
- mTLS: node certs with CN=NodeID; hub requires + verifies; cert rotation =
  file reload on SIGHUP (same reload path).
- Admin token: 32-byte random, constant-time compare (`subtle.ConstantTimeCompare`).

## 8. Failure Modes (security-relevant)

| Failure | Detection | Behaviour | Why |
|---|---|---|---|
| Guard misconfig (over-broad allow) | config validation warns on allow ⊇ private space | Warn + require `acknowledge_ssrf_risk: true` | Refuse-to-start-quietly is how footguns ship |
| Cert expired (hub mTLS) | handshake fail | Node spools; alert; no plaintext fallback ever | No silent downgrade is a hard rule |
| Token brute force | admin plane rate limit + lockout counter | 429 with escalating Retry-After | Cheap defense, closed telemetry |
| Log flood from attack | sampling + level policy | Data plane unaffected | Availability of the *log* must not cost the *service* |
| Fuzz crash found | corpus regression | Release blocker | Parser crashes are RCE-adjacent |

## 9. Unresolved Security Decisions

| ID | Question | Default | Revisit |
|---|---|---|---|
| USD-1 | Media LAN token auth | Off (PRD non-goal) | If media ever binds non-LAN |
| USD-2 | TLS 1.0/1.1 *scanning* (probing old versions as a finding) | Off, opt-in | Compliance use case |
| USD-3 | DNSSEC validation | Milestone | With wire engine maturity |
| USD-4 | Per-tenant auth on hub query API | None (single-operator) | Multi-operator hosting |

## 10. Acceptance

All red-line tests green (T-36, T-50, T-55, T-82, T-93, T-70) in CI; fuzz
targets 10-minute nightly runs with committed seed corpora
(`internal/dnsmon/wire/testdata/corpus/`); SSRF suite covers every deny-rule
with positive and negative cases; redaction golden tests; admin exposure
port-scan green; constant-time token compare verified by timing-insensitive
test (equal-length random tokens, no early-exit — verified by code inspection
+ test asserting `subtle` usage, since timing tests are flaky by nature).
