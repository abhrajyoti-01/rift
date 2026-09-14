# MEDIA_SERVER_SPEC.md — RIFT High-Throughput Media Server

Status: **Revision A**. Owning requirements: FR-35..FR-41, NFR-7, NFR-8.
Identifiers: `TECHNICAL_SPEC.md` §7 authoritative. Scope decision (PRD §4):
progressive HTTP + single byte-range in v1; HLS/DASH packaging, transcode,
container parsing are milestones (AD-9). "Zero buffering" is never claimed;
measurable objectives with defined instruments are given instead.

---

## 1. Design Objectives (measurable, with instruments)

| Objective | Definition | Instrument | v1 target |
|---|---|---|---|
| Sustained throughput | aggregate bytes/s at the plateau across concurrent streams | `rift_media_bytes_sent_total` rate over scenario window | ≥ 0.8 × min(disk, NIC) measured ceilings [E3] |
| Startup latency | request → first response byte + playout buffer reaches initial threshold | `bench/playersim` (client-side; server first-byte metric reported separately, never merged) | p50 ≤ 500 ms, p95 ≤ 1.5 s on LAN [E3] |
| Rebuffer rate | stalls per hour of playback | `playersim` playback-clock model (P3) | p95 of clients ≤ 1 stall/h at rated capacity [E3] |
| Concurrent capacity | streams at target bitrate before admission or throughput cliff | `media.capacity-ramp` scenario | 10 × 25 MB/s (4K-class) + 50 × 2–6 MB/s (lossless-audio class) [E3] |
| Memory | RSS at rated capacity | environment card + sampler | ≤ 200 MiB + 1 MiB × active streams |
| CPU | cores at rated capacity | `mpstat` in scenario harness | < 50% of one core per 10 streams at splice-path [E3] |
| Network utilization | vs measured `iperf3` ceiling | card + `ss -ti` | ≥ 80% of ceiling at capacity point |

**Why the bottleneck is not the server in v1:** memory per stream is one
readahead buffer (buffered mode) or socket buffers only (sendfile mode);
per-GiB allocation budget is ≤ 100 MiB (NFR table); the copy path is
sendfile/`ReadFrom`. What *does* bound performance, in order: (1) disk
sustained read at queue depth > 1, (2) NIC/transport path (on WSL2, the
virtual-NIC/host bridge adds measurable distortion — E3 quantifies, AD-11),
(3) client-side decode, (4) concurrency-induced seek thrash on spinning
disks — hence `posix_fadvise` sequential hints (§3). "The server adds
< X% overhead vs raw `dd`/`iperf3` ceiling" is the claim form; X is [E3].

## 2. HTTP Semantics — RFC 7233 exact

| Case | Response |
|---|---|
| No `Range` header | `200` full body, `Accept-Ranges: bytes` |
| `bytes=0-0` | `206`, `Content-Range: bytes 0-0/size` |
| `bytes=500-999` | `206` that window |
| `bytes=500-` | `206` from 500 to EOF |
| `bytes=-500` | `206` last 500 bytes (suffix) |
| `bytes=0-0,2-10` | v1: full `200` (multi-range MAY be ignored per RFC 7233 §3.1), counted `rift_media_range_requests_total{outcome="multi"}` — documented, not silent |
| `Range: bytes=999999999-` (beyond EOF) | `416` + `Content-Range: bytes */size` |
| `bytes=5-2` (start > end) | `416` |
| `bytes=-0` | `416` |
| `Range: bytes=abc` | `416` (RFC 7233: unsatisfiable/invalid → 416, not 200 — invalid ≠ absent) |
| `Range: bytes=1-2, …` with `If-Range` strong ETag match | `206` single window |
| `If-Range` mismatch | `200` full |
| `HEAD` | headers mirror GET, no body |
| 0-byte asset | any range → `416`; GET full → `200` with `Content-Length: 0` |
| Zero-length range (start == end+1 …) | normalized `RangeSpec{Start, Length:1}` minimum | 

Range parse outcomes: `RangeNone | RangeOK | RangeMulti | RangeInvalid`
(→ `400`). **Invalid ranges produce `416`, not `200`** — RFC 7233 §4.4 is
explicit; tools that fall back to 200 on malformed Range are common and wrong.

`ETag`: strong, `"hex(size)-hex(mtime.UnixNano())"` — cheap, unique per
representation change, collision-safe for rebuilds. `Last-Modified` served as
courtesy; `If-Range` honors strong ETag only.

## 2a. What Affects Playback (the physics RIFT documents, not fights)

1. **Network bandwidth** — the hard ceiling; measured per-environment card
   (`iperf3`), not assumed. 10 concurrent 4K streams at 25 MB/s = 250 MB/s ≈
   2 Gbit/s, beyond 1GbE — the card makes this visible *before* the scenario
   "fails mysteriously".
2. **Disk throughput** — `fio` measured ceiling at QD≥1; spinning disks seek-thrash
   under interleaved seeks from concurrent streams; `posix_fadvise` sequential
   reduces readahead thrash; SSD/NVMe largely immune. RIFT measures, does not
   assume.
3. **Client decode** — 4K decode on a weak client rebuffers regardless of
   server throughput; `playersim` models network+buffer, not decode. Documented
   limitation of the instrument (playersim README §3).
4. **Container/codec behavior** — bitrate variance (VBR peaks 2–5× average)
   means "25 MB/s average" streams can burst to 100 MB/s+; capacity scenarios
   use a configured burst factor (default 2) so admission math is honest.
5. **WSL2 transport** — host↔WSL2 traffic traverses a virtual NIC/bridge with
   its own ceiling; E3 quantifies; ratios against measured ceilings keep claims
   portable (AD-11).

## 3. Copy Path

Two modes, both real, both shipped; default chosen by E5 measurement (AR-5):

- **sendfile mode (default)**: `io.Copy(w, io.NewSectionReader(f, start, len))`
  → runtime `*os.File → *net.TCPConn` fast path. No per-stream heap buffer
  exists. `posix_fadvise(POSIX_FADV_SEQUENTIAL)` on the fd (Linux only,
  feature-detected via `x/sys/unix`, no-op elsewhere). Cross-OS note: on
  Windows development hosts the same code path falls back to the runtime
  buffered copy automatically; benchmarks run on Linux only.
- **buffered mode**: explicit readahead (default 1 MiB) per stream; for
  pathological small-range patterns (seek-heavy clients) where sendfile
  syscall-per-range overhead beats a buffered stream. E5 measures both across
  1/4/16 streams and the shipped default is whichever wins, numbers committed
  in `bench/results/E5-iomode.txt`.

**No whole-file buffering. No mmap in v1** (milestone: readahead via
`madvise` after a profile justifies it). Memory per stream = 1 buffer +
socket buffers, bounded by config independent of file size (FR-36) — asserted
by the 10-GiB-fixture memory test.

## 4. Admission and Limits

- Global `max_streams` (default 64) + per-client (default 4). Exhausted →
  `429` + `Retry-After: 2` + `rift_media_admissions_rejected_total{scope}`.
- Per-client token bucket (default 120 req/s) — a seek-happy client cannot
  monopolize range-parse CPU.
- Slow-client policy: `WriteTimeout` per stream (seek resets), send-stall
  accounting (accumulated `Write` block > 100 ms), cull at deadline with
  `rift_media_client_stalls_total`. **Bounded goroutines/FDs is a correctness
  property**, not a nicety.
- Disk high-water mark (config, default 90% of measured ceiling): at
  high-water, new admissions shed (`429`) while in-flight streams continue —
  protects in-flight playback over admitting more (ARCHITECTURE §13).

## 5. Index and Library

- Built from filesystem metadata only: name, size, mtime (AD-9). No container
  parsing, no probing file contents. 100k assets < 2 s (NFR-8, asserted).
- URL-space: `GET /v1/media/{name}`; `name` is URL-safe, `/`-separated
  relative path under root. Path traversal (`..`, absolute, backslash on
  Windows, NUL, symlink escape) rejected `400`/`404` — canonicalize-then-prefix-check,
  plus root-local symlink policy: symlinks resolving outside root are
  rejected (config `follow_symlinks: false` default).
- Rebuild on SIGHUP/reload, atomic swap behind `atomic.Pointer[Index]`
  (same pattern as LB snapshot, AD-6).

## 6. Bandwidth Monitoring

Per-stream accounting: bytes, duration, first-byte, send-stall — sampled into
metrics; `rift_media_active_streams` sampled gauge. Client-side throughput is
measured by `playersim`, reported beside server-side bytes, never merged
(server bytes ≠ client experience; P3).

## 7. Configuration

```yaml
media:
  bind: "0.0.0.0:8080"
  root: "/srv/media"
  max_streams: 64
  max_streams_per_client: 4
  readahead: 1MiB
  io_mode: sendfile        # sendfile | buffered — default per E5
  timeouts:
    read_header: 10s
    idle: 120s
    write: 300s            # per stream; seek resets
  rate_limit_per_client: 120/s
  index:
    rebuild_on_reload: true
    exclude: ["*.tmp", ".git"]
  follow_symlinks: false
  disk_high_water: 0.90    # fraction of measured ceiling
```

Validation mirrors LB patterns (field-path errors, unknown fields rejected,
value-range checks, port < 1024 rule, root must exist at startup — a media
server whose root does not exist is a config error, exit 2).

## 8. Metrics (component)

`rift_media_bytes_sent_total{stream_class}` · `rift_media_active_streams` ·
`rift_media_stream_bytes` (histogram) · `rift_media_first_byte_latency_seconds`
(server-side) · `rift_media_send_stall_seconds` ·
`rift_media_client_stalls_total` · `rift_media_readahead_hit_ratio` (gauge,
buffered mode) · `rift_media_range_requests_total{outcome}` ·
`rift_media_admissions_rejected_total{scope}` ·
`rift_media_index_assets` (gauge) · `rift_media_index_build_seconds`.

`stream_class` from closed set {v4k, vhigh, vstd, alossless, astd} assigned by
directory convention or explicit manifest — closed set, not filename patterns
(an unbounded label from filenames is the anti-pattern OBSERVABILITY_SPEC §3
prohibits).

## 9. Failure Modes

| Failure | Detection | Behaviour | Rationale |
|---|---|---|---|
| File deleted mid-stream | read error past EOF → `404`/close | Stream ends cleanly, counted | Open fd keeps serving; deletion is not client-visible corruption |
| Root unmounted | periodic root probe | `readyz=false`, serve 503, keep admin plane | Observable degradation |
| Seek storm (many small ranges) | per-client rate limit | `429` with `Retry-After` | Protects CPU and disk queue |
| Disk saturation | throughput ratio vs ceiling | shed new admissions at high-water | In-flight playback preferred |
| Slow client | write deadline + stall metric | Cull at deadline | Bounded FDs/goroutines = correctness |
| Index rebuild vs requests | atomic pointer swap | Requests never see partial index | Same AD-6 pattern |
| Range flood (parse abuse) | rate limit + closed outcomes | 429/400 | Parser is fuzzed; abuse bounded |
| Oversized header | `MaxHeaderBytes` | 431/400 immediate | Resource exhaustion defense |

## 10. Acceptance

- RFC 7233 test matrix (§2 table, every row) green.
- 10-GiB fixture memory assertion: RSS delta ≤ readahead + socket buffers per
  stream (FR-36).
- E5 result committed; shipped default justified by numbers.
- `media.4k.x10` + `media.lossless` scenarios: NFR-7 ratio measured and
  recorded; `playersim` stall counts reported client-side alongside server
  throughput.
- `media.capacity-ramp` finds the actual cliff; cliff documented vs card
  ceilings (not vs hope).
- Path-traversal suite green (SECURITY_SPEC §6).
