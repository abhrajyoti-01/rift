# DNS_SSL_TRACKER_SPEC.md — RIFT DNS Propagation & TLS/SSL Monitor

Status: **Revision A**. Owning requirements: FR-25..FR-34, NFR-9, NFR-10.
Identifiers: `TECHNICAL_SPEC.md` §5 (dnsmon) and §6 (tlsmon) are authoritative;
this document owns behaviour, classifier derivation, limitations, and alerting.
Scope decision recorded in PRD §4: multi-resolver node + real node protocol;
genuine geographic nodes are a documented milestone, not a claim.

---

## 1. What This Component Is Honest About

Three observation classes exist in reality and are **never merged**:

1. **Authoritative state** — what the zone's NS servers actually serve. RIFT
   queries these directly (`RD=0`). Ground truth for "what is published".
2. **Recursive-resolver state** — what configured public resolvers
   (1.1.1.1, 8.8.8.8, 9.9.9.9, …) serve from cache right now. RIFT queries
   these (`RD=1`).
3. **Observed client-facing state** — what arbitrary real clients see. **Depends
   on ISP resolvers, NAT'd networks, cache layers and TTLs that RIFT cannot see
   from its vantage points. Not implemented; not approximated.** Dashboards
   label views 1 and 2 separately. No code path emits a "propagated worldwide"
   verdict — enforced by test T-51 greping the state enum and the alert
   vocabulary.

**What resolver-based observation can and cannot tell you** — stated because
this is the component's central honesty requirement:

- RIFT observes a *sample*: the configured resolvers, at the scheduled moments,
  from the node's network. It does **not** observe the recursive-resolver
  population of the internet, and it never claims to.
- A resolver's cached answer is a function of (query time − last cache fill)
  vs TTL; two resolvers can legitimately disagree for up to (remaining TTL)
  after an authoritative change. The classifier treats divergence within the
  convergence window as `PropConverging`, not as an error.
- Some networks rewrite DNS (captive portals, split-horizon, RFC 8484
  forwarding); a node inside such a network reports what that network serves,
  which may not be what the public internet serves. Node `location` labels
  exist to make this visible, not to pretend it away.
- `NOERROR` with zero answers is a legitimate state (NODATA), distinct from
  `NXDOMAIN` and from transport failure. Conflating them produces false
  "outage" alerts; RIFT records rcode verbatim.

## 2. Wire Engine — `dnsmon/wire`

Own implementation, non-negotiable (AD-4): stdlib returns no TTL (`net.NS` is
`{Host string}`, `net.MX` is `{Host, Pref}`, `LookupCNAME` → bare string —
verified against pinned toolchain), no `RD=0`, no TC visibility, no raw message
access for cross-resolver diffing. `miekg/dns` was rejected: the wire engine is
the point of the component and is fuzzed from day one.

Behavioural rules (each is also a fuzz/test class — TESTING_SPEC §6):

- **Encode**: queries uncompressed (parse compression, never produce it —
  removes a whole correctness surface from the encoder). EDNS0 with
  `udpsize=1232` (the DNS flag-day recommendation). `ID` from `crypto/rand`.
  One allocation: the message buffer.
- **Decode**: iterative label walk (no recursion); pointer must target a
  strictly earlier offset (forward/self pointers → `ErrBadPointer`); name ≤ 255
  octets, labels ≤ 63; message-size caps 512/1232/65535 by transport;
  `Unknown` RData preserved as raw bytes, never guessed.
- **TCP fallback**: `TC=1` → re-query over TCP with 2-byte length prefix
  (RFC 7766). Both legs carry the same question; fresh transaction ID is fine.
- **Transaction matching**: one connected UDP socket per in-flight query
  (kernel source-filtering, ephemeral port), match on socket + ID + question.
  Shared-socket v1 rejected: isolation beats a marginal socket saving at
  monitoring QPS.
- **Spoofing resistance**: random source ports + random IDs; responses from
  non-queried sockets are dropped by the kernel before user space. Cache
  poisoning via off-path guessing requires guessing both port and ID.

Supported types: A, AAAA, CNAME, MX, TXT, NS. **Milestones**: DNSSEC
validation (AD/RRSIG parse), CAA, SRV, PTR, HTTPS/SVCB, DoH/DoT/DoQ transports.

## 3. Resolver Engine — `dnsmon/resolver`

Per-resolver `Engine`: its own bounded `pool.Executor` (default 8 concurrent),
per-attempt timeout (2s), per-resolver `circuit.Breaker` (5 consecutive
failures → open, 10s half-open probe). **One slow authoritative server cannot
consume the node's outbound budget** — the per-resolver cap + breaker is the
amplification defense (SECURITY_SPEC §5).

Stats per resolver (atomics, scraped as metrics): queries, timeouts,
truncated, rcode distribution, in-flight gauge, breaker state. Timeout is
`ClassTimeout`, distinct from `ClassNetwork` — conflating them hides the exact
attack/failure mode that matters (a resolver that answers everything slowly
vs one that drops packets).

## 4. Views and Propagation — `dnsmon/probe`, `dnsmon/model`

### 4.1 Recursive view

Queries each configured resolver (`RD=1`) on the schedule. Produces
`Observation{View: ViewRecursive, Resolver: "1.1.1.1:53", ...}`.

### 4.2 Authoritative view

NS discovery: query the TLD servers for referrals → zone NS set (A/AAAA glue
not trusted; resolved via the *recursive* engines, guarded) → query each NS
with `RD=0`. v1 caps NS servers probed per zone (`max_ns_probe: 4` — FD
safety, UD-5). `AA=1` responses recorded; if the NS set itself is being
changed (parent zone still refers to old NS), both old and new sets may be
queried during the transition — recorded, not hidden: this is a real
propagation phenomenon (NS churn), and the classifier's reference logic (§4.4)
prefers the freshest authoritative answer.

### 4.3 Observation schema

Per `TECHNICAL_SPEC.md` §5.4: NodeID, View, Resolver, QName, QType, RCode,
canonical sorted Answers (Name, Type, TTL, Data), Truncated, Transport,
Latency, Timestamp (UTC), ErrClass. Fingerprint = sorted `Answer.Data` set,
**TTL excluded** (TTL decay alone must not read as divergence — FR-34).

### 4.4 Classifier derivation

Reference = fingerprint of the freshest *successful authoritative*
observation. States (enum order = severity order for dashboards):

| State | Condition |
|---|---|
| `PropUnresolvable` | No successful authoritative observation in window |
| `PropInsufficientCoverage` | < `min_responding` distinct responsive resolvers (default 3) |
| `PropDivergent` | ≥1 responding resolver fingerprint ≠ reference |
| `PropConverging` | Reference timestamp within `convergence_window` (default 5m) of newest observation AND not all match |
| `PropConverged` | All responding resolvers match reference |
| `PropStateUnknown` | Window empty |

Boundary semantics: divergence that begins within the convergence window is
`Converging` (expected post-change); divergence persisting beyond it is
`Divergent` (a real finding). `rift_dns_view_divergence_seconds` = time from
authoritative change (reference timestamp) to all-responding-match, the
component's headline metric — the honest propagation time across *observed*
resolvers, not worldwide.

### 4.5 Node

Scheduler per target; per-resolver engines; fixed-capacity ring (default
65536) with drop-oldest + `rift_dns_observations_dropped_total{reason=
"ring_full"}` — **loss is counted, never silent**. Shipper: batch on size
(512) or age (5s), POST `/v1/ingest` over mTLS; 429/5xx → `retry.Policy`
backoff; ring full **and** shipping failing → bounded disk spool (64 MiB cap),
beyond which drop-oldest-with-counter. A monitor degrades by reporting gaps,
not by lying (ARCHITECTURE §13).

**Spool honesty rule**: the spool exists to survive hub outages *shorter*
than the spool cap. It is not a durability guarantee; the shipped-batch
acknowledgement is at-least-once (idempotent by (NodeID, Timestamp, QName,
QType, Resolver) — hub dedupes on that key).

### 4.6 Hub

- Ingest `/v1/ingest`: mTLS client cert required; batch ≤ 4096; per-request
  goroutine; sharded (target,view) window map, bounded `WindowKeys` (default
  4096 per shard target); NFR-9's 256 MiB ceiling enforced by window caps,
  not by hope.
- Persistence: append-only JSONL segments (128 MiB rotation, `fsync` per
  batch), filenames `seg-{UTC range}-{seq}.jsonl`. `rift dns replay`
  re-materializes windows from segments (FR-33).
- Query API: `/v1/observations?target=...`, `/v1/targets`,
  `/v1/propagation` (live window + segment-backed history ranges).
- Alert evaluator (30s cadence, fake-clock testable): conditions in §7.
- Hub HA is a non-goal (PRD §4). Single hub = documented SPOF.

## 5. TLS Monitor — `tlsmon`

Probe flow per `TECHNICAL_SPEC.md` §6: `tls.Dial` with
`InsecureSkipVerify: true` + `VerifyConnection` capturing the presented chain
→ manual chain build → `x509.Verify` against configured roots +
`VerifyHostname` → every failure becomes a **Finding** from a closed code
set, not an abort. The report *is* the product; a target with a bad chain is
a successfully monitored target (AD-5).

Finding codes (closed set): `EXPIRED`, `NOT_YET_VALID`, `HOSTNAME_MISMATCH`,
`UNTRUSTED_ROOT`, `INCOMPLETE_CHAIN`, `SELF_SIGNED`, `WEAK_KEY` (RSA < 2048 /
ECDSA < 256), `WEAK_SIG` (MD5/SHA-1), `OLD_TLS` (negotiated < floor,
default 1.2), `HANDSHAKE_REFUSED`, `CONN_REFUSED`, `CONN_TIMEOUT`,
`PROTO_MISMATCH` (no common protocol). Severity mapping: `EXPIRED`/
`NOT_YET_VALID`/`HOSTNAME_MISMATCH`/`OLD_TLS` → Critical; `WEAK_KEY`/
`WEAK_SIG`/`INCOMPLETE_CHAIN`/`UNTRUSTED_ROOT`/`SELF_SIGNED` → Warning;
protocol info → Info.

Alerts: `not_after − now < 14d` → Warning; `< 3d` or `find_begin > 0` →
Critical; ≥ 3 consecutive handshake/connection failures → Critical (a
silent-death TLS endpoint is an outage signal); `OLD_TLS` floor violation →
Warning. All thresholds configurable. Alert delivery in v1: log line +
`rift_tls_alerts_total{target,code,severity}` metric + `/v1/alerts` endpoint —
**webhook delivery is a milestone** (no fake notifications, ARCHITECTURE AR-6).

**TLS-version probing**: v1 records the *negotiated* version. Scanning all
versions (1.0/1.1) is a milestone; probing deprecated versions by default
would make RIFT itself the weak-handshake client. Floor is configurable for
compliance postures (`verify: strict` flips probe to failing mode).

**SSRF boundary**: `tlsmon` targets are operator-configured; the probe dials
through `netx.Guard` (deny loopback/private/metadata; allow-list overrides).
The same applies to `dnsmon`'s authoritative NS resolution. Enforced at the
dialer (AD-10), never per call site.

## 6. Configuration

```yaml
dns:
  role: node
  node_id: wsl2-home-01
  location: home-lan
  resolvers:
    - {addr: "1.1.1.1:53", concurrency: 8}
    - {addr: "8.8.8.8:53", concurrency: 8}
    - {addr: "9.9.9.9:53", concurrency: 8}
    - {addr: "208.67.222.222:53", concurrency: 4}
  targets:
    - {zone: "example.com.", name: "www.example.com.", type: A, view: both}
    - {zone: "example.com.", name: "example.com.", type: MX, view: both}
  interval: 60s
  max_ns_probe: 4
  ring_cap: 65536
  ship_every: 5s
  ship_size: 512
  hub: {url: "https://hub.internal:9001", client_cert_file: "...", client_key_file: "..."}
  spool_max: 64MiB
tls:
  targets:
    - {host: "example.com", port: 443, floor: "1.2"}
    - {host: "api.example.com", port: 443, floor: "1.3", verify: strict}
  interval: 3600s
  expiry_warn_days: 14
  expiry_crit_days: 3
  consecutive_failures: 3
  root: system           # system | {ca_file: "..."}
```

Validation: resolver IPs must be literal (no names — the monitor must not
depend on the thing it monitors for its own bootstrapping), max in-flight
bounded, `ring_cap` ≥ 1024, `spool_max` ≤ 1 GiB, target zone/name length ≤
253, type ∈ supported set, view ∈ {recursive, authoritative, both},
`expiry_crit_days < expiry_warn_days`, port ranges, allow-list entries must be
literal prefixes.

## 7. Alert Conditions (complete v1 set)

| Condition | Class | Severity | Delivery |
|---|---|---|---|
| Recursive fingerprint ≠ authoritative reference, beyond convergence window | Divergence | Critical | Log + metric + `/v1/alerts` |
| Recursive fingerprint ≠ reference, within window | Convergence | Info | Metric only |
| Cert expiry < 14d | Expiry | Warning | Log + metric + `/v1/alerts` |
| Cert expiry < 3d | Expiry | Critical | same |
| ≥3 consecutive handshake failures on target | Availability | Critical | same |
| `OLD_TLS` floor violation | Compliance | Warning | same |
| Ring drops > threshold per hour | Node health | Warning | Metric + log |
| Spool > 80% cap | Node health | Warning | Metric + log |
| Propagation state → `PropInsufficientCoverage` | Coverage | Warning | Metric only |

## 8. Metrics (component)

Per `OBSERVABILITY_SPEC.md` §3: `rift_dns_queries_total{node,resolver,view,rcode,transport}` ·
`rift_dns_query_duration_seconds{resolver,view}` ·
`rift_dns_timeouts_total{resolver}` · `rift_dns_truncated_total{resolver}` ·
`rift_dns_observations_dropped_total{node,reason}` ·
`rift_dns_propagation_state{target}` (enum gauge) ·
`rift_dns_view_divergence_seconds{target}` ·
`rift_dns_resolvers_healthy{resolver}` (breaker state) ·
`rift_tls_cert_not_after_seconds{target}` ·
`rift_tls_handshake_failures_total{target,reason}` ·
`rift_tls_protocol_version_info{target,version}` ·
`rift_tls_alerts_total{target,code,severity}` ·
`rift_hub_ingest_batch_size` (histogram) · `rift_hub_segments_total{state}` ·
`rift_hub_window_utilization{shard}` · `rift_dns_shipper_backoff_events_total`.

Cardinality bounds: `resolver` ≤ 32, `target` ≤ 256, `rcode` closed,
`reason`/`code`/`severity` closed enums. `qname` **never** a metric label
(unbounded) — appears only in logs, capped at 253 chars.

## 9. Limitations (stated, not hidden)

1. Node count in v1: one node process querying many real resolvers. Multi-node
   is *architecturally* real (protocol, mTLS, hub aggregation) and tested
   with two nodes over loopback + two WSL2 netns; genuine geographic spread is
   a milestone.
2. Resolver-based observation is a sample, not a census (§1).
3. Hub is a SPOF (no clustering, PRD §4).
4. No DNSSEC validation in v1 (milestone).
5. TLS scanning of deprecated versions is opt-in, milestone.
6. DoH/DoT/DoQ transports: milestone (UDP/TCP port 53 only in v1).
7. History: JSONL segments + replay; no SQL queries over history (AD-7).
8. `view: both` doubles query volume; `min_responding` can never be satisfied
   below 3 resolvers — validated at config time.

## 9a. Failure Modes (component)

| Failure | Detection | Behaviour | Rationale |
|---|---|---|---|
| Hub unreachable | ship retry/backoff | Ring buffers; spool; drop-oldest counted | Degrade by reporting gaps, not lying |
| Resolver times out | ctx deadline | `ClassTimeout` counter; breaker after 5 | Protects node budget; hides nothing |
| Malformed response | wire decode errors | Discard + `ClassSecurity` counter; never crashes | Fuzzed parser; garbage is expected input |
| Poisoned/rewriting network | cross-resolver divergence | Visible as `PropDivergent` with node location label | Sample honesty (§1) |
| NS churn during change | both old+new NS queried | Freshest authoritative answer is reference | Real phenomenon, recorded not hidden |
| Segment write fails | fsync error | Ingest 503 (backpressure upstream), alert | At-least-once beats silent data loss |
| Spool full | spool gauge | Drop-oldest with counter | Bounded disk is a correctness property |
| Cert store mismatch (hub roots) | verify errors | Findings, not aborts | Monitor must observe, not die |
| Ingest flood | batch cap + rate limit | 429; node backs off | Hub protects itself |

## 10. Acceptance (component)

- `dig` byte-equivalence: A/AAAA/CNAME/MX/TXT/NS answers and TTLs match
  `dig +ttldata` across ≥3 real resolvers (network-gated test).
- Fuzz corpus green: malformed/truncated/pointer-loop/oversized (T-5x).
- Classifier fixture suite: every state reachable via fixtures, including
  boundary (convergence-window edge) cases.
- Two-node integration: both nodes' observations in hub window; dedupe works;
  spool round-trip during hub outage (T-61).
- Alert evaluation under fake clock: every §7 row fires exactly on threshold,
  never early (synctest).
- NFR-9: RSS ≤ 256 MiB under observation flood for 10 min.
- NFR-10: wire encode 1 alloc / decode ≤ 3 allocs, asserted in benchmarks.
