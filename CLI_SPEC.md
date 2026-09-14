# CLI_SPEC.md — RIFT Command-Line Interface

Status: **Revision A**. Owning requirements: FR-1, FR-6, FR-32, FR-45..FR-48.
The user's brief proposed `netperf <subcommand>`; RIFT adopts the structure but
renames the binary `rift` (module `github.com/rift/rift`, directory `Rift`) —
`netperf` collides with the established Hewlett-Packard netperf benchmarking
tool, and a repo whose binary name collides with a 25-year-old benchmarking
standard invites permanent confusion in search, package managers, and
operators' muscle memory. Command *structure* below is otherwise the
professional shape the brief asked for.

---

## 1. Design Rules

- Subcommand-first: `rift <service> <verb>`; flags are configuration delivery,
  not behavior switches — the config file is the single source of truth
  (config overrides: `--config` path, `--set key=value` dot-path overrides,
  validated identically to the file).
- Every command exits per `errs.ExitCode` (0/1/2/3/4/124) and prints
  machine-readable output with `-o json` where data is produced.
- No interactive prompts (CI-safe by construction, FR-2).
- Global flags: `--config <path>` (default `./rift.yaml`, then
  `$RIFT_CONFIG`), `-o text|json`, `--verbose` (debug logs), `--no-color`.

## 2. Command Tree

```
rift init                          write commented starter rift.yaml (idempotent; refuses overwrite without --force)
rift config validate [--config]   validate only; exit 2 on any violation; lists ALL violations with field paths
rift config diff [--config]        field-path diff vs running snapshot (admin plane) or vs another file (--against)
rift lb [--config]                 run load balancer           (SIGINT/SIGTERM → graceful; SIGHUP → reload)
rift dns node [--config]           run DNS monitoring node
rift dns hub [--config]            run aggregation hub
rift dns query NAME[@resolver] [type] [flags]   one-shot live query (real wire, real resolver; -v=v4|v6, --view=recursive|authoritative)
rift dns replay --segments DIR --target T      re-materialize window from JSONL segments (offline)
rift tls run [--config]            run TLS monitoring service
rift tls check HOST[:port]         one-shot live TLS probe → findings, chain, negotiated version/cipher
rift media [--config]               run media server (SIGHUP → index rebuild)
rift bench env [-o json]           environment card
rift bench run --scenario ID [--duration] [--rate] [--conns]   run scenario (stable scenario IDs, PERFORMANCE_SPEC §4)
rift bench compare --against FILE  tolerance-band comparison (exit 1 on out-of-band)
rift bench report --samples DIR    render report (refuses without environment card)
rift bench scenarios               list scenario IDs + definitions
rift health [--addr]               probe a running service's /healthz + /readyz, print phases + checks
rift metrics [--addr] [-o text|json|prom]   dump live metrics from admin plane
rift version                       version, commit, go version, build tags
rift completion bash|zsh|fish      shell completion
```

Rejected command shapes (recorded): `rift server` (ambiguous — which server?);
`rift start` (same); flat verbs like `rift dnsmon` (service names should read
as nouns, verbs as verbs: `rift dns node` runs, `rift dns query` acts). The
`dns node`/`dns hub` split mirrors the actual deployment split — a node runs
where the vantage is, the hub runs where the data lives; pretending they are
one command would hide a real operational boundary.

## 3. One-Shot Tools Are Real (no fake data)

`rift dns query` performs a real wire query to a real resolver (default: the
first configured resolver; `@1.1.1.1` overrides). Output (text):

```
;; www.example.com. A @1.1.1.1:53 (udp, 12.3 ms)
www.example.com.  300  IN  A  93.184.216.34
;; rcode=NOERROR, aa=0, tc=0, view=recursive
```

`rift tls check example.com` performs a real handshake and chain
verification; findings print in severity order with the closed code set
(EXPIRED, HOSTNAME_MISMATCH, …). `-o json` gives the full `tlsmon.Report`.
These tools share the same engine code as the monitoring services — a
one-shot tool that behaves differently from the service is a bug.

## 4. Output Conventions

- Text mode: aligned columns, no spinners, no ANSI when piped
  (auto-detected; `--no-color` forces).
- JSON mode: stable field order, RFC 3339 UTC timestamps.
- Errors: `rift: <subcommand>: <message>` on stderr; config errors add
  ` (field: lb.pools[0].backends[2].addr)`; exit code per §5. Errors list
  every violation, not the first.
- Progress: long-running commands print phase transitions
  (`serving`, `draining`, `flushing`) — observable shutdown (FR-3).

## 5. Exit Codes

| Code | Meaning | Example |
|---|---|---|
| 0 | success | validate passed |
| 1 | runtime fault | target unreachable in one-shot tool |
| 2 | config invalid | `rift lb` with bad yaml — lists all violations |
| 3 | bind/resource failure | port in use, rlimit exceeded |
| 4 | drain deadline exceeded | shutdown with in-flight conns over budget |
| 124 | bench timeout | scenario exceeded wall budget |

Supervisor guidance (documented in README): exit 2 = "fix the file, don't
restart"; exit 1/3 = "restart me"; exit 4 = "restart, and check what was
in-flight".

## 6. Configuration Delivery

- `rift <service> [flags]` reads `--config` / `$RIFT_CONFIG` / `./rift.yaml`
  in that order; missing file → exit 2 with the search paths tried.
- `--set` overrides: dot-path `lb.listeners[0].max_conns=2000`, validated
  with the same rules; overrides logged at startup (redacted).
- Env: `RIFT_<SECTION>_<KEY>` for documented scalar keys only (FR-7), listed
  in `rift config validate --help` — env keys that silently do nothing are
  the enemy of operators everywhere.

## 6a. Behaviour Notes

- `rift init` output is a *valid* config (validation round-trip asserted in
  T-95) with comments explaining every section; it must never write an
  example that itself fails validate.
- `rift dns replay` is the offline tool for FR-33: reads segments, rebuilds
  windows, prints/dumps propagation history; pure offline (no network).
- `rift health` speaks plain HTTP to a *given* admin addr (default
  127.0.0.1:9000); it never starts a listener itself.

## 7. Contract Tests (T-95)

Subcommand matrix: every command with valid/invalid config × {text, json};
exit codes asserted per §5; `init → validate` round-trip; `--set` override
round-trip; completion generation runs clean; no command writes to anything
but its declared outputs; `--help` for every node (help coverage is part of
the test, not a nicety — a hidden flag is a bug).
