# quack-proxy

[![CI](https://github.com/alitrack/quack-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/alitrack/quack-proxy/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev)
[![DuckDB](https://img.shields.io/badge/DuckDB-1.5.2%20%E2%80%93%202.0.1-FFF000?style=flat&logo=duckdb)](https://duckdb.org)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Built by Hermes](https://img.shields.io/badge/built%20by-Hermes%20Agent-8A2BE2)](https://hermes-agent.nousresearch.com)

> One YAML, one command. Start and supervise multiple DuckDB + Quack endpoints on a single host.

> **Status (2026-11-19, remediation r17):** this is a **single-host process supervisor**, not a highly-available cluster. The daemon is a single point of failure, there is no cross-machine discovery, and `gen-proxy` only writes an HAProxy config file — quack-proxy is **not** in the data path. All nine CI gates are required (no advisory left), including a strictly-offline real-engine E2E, a real HAProxy + floating-IP drill, and an engine TLS capability audit. **The VIP drill is NOT an HA feature**: the address is moved by the drill (standing in for keepalived / a cloud floating IP); after failover the client reaches a different backend (`SESSION_DISCONTINUITY=1`), so sessions and data do not survive. **The TLS audit proves the pinned DuckDB engine CAN serve HTTPS; quack-proxy still serves plaintext HTTP.** The PRD never asked the product to terminate TLS: §4.2 specifies an external reverse proxy, and SCOPE §3 lists built-in TLS termination as a non-goal. **r15 verifies that existing deployment decision end to end** (`plaintext-audit` now also drives a real HAProxy TLS terminator in front of a loopback-bound shard and proves no credential is readable on the LAN) — this is operator deployment acceptance, **not** a product TLS feature; `disable_ssl=true`, the dead `proxy.ssl` config and the plain `gen-proxy` output are unchanged, and operators must still deploy and maintain that proxy. **Exposing a shard directly on a LAN remains a cleartext risk** (R-05 transport weakness is open). **Token-at-rest was fixed in r14**: `status.json` no longer carries any token by default (`global.persist_tokens: false`); only an explicit opt-in restores 0600-persisted auto-generated tokens, and with the default off every shard must supply its own token. **Offline deployments must pre-install both the signed `quack` and `httpfs` extensions** (DuckDB v2.0.0 needs httpfs to write a disk database). Read [SCOPE](docs/SCOPE.md), [REMEDIATION](docs/REMEDIATION.md) and [SECURITY](docs/SECURITY.md) before using it for anything important. **r16 is documentation-only**: it closes an internal contradiction in `docs/PRD.md` where R-17 (exponential-backoff restart) was still labelled "待设计决策 / pending a design decision" in §3.3/§7/§9 although the state table, REMEDIATION §3, SCOPE and this README all mark it fixed (`TestGate2RestartDoesNotRefreshStartupGrace` passes under `-race`). No code, script, CI gate or product behaviour changed; the remaining-open list is unchanged. **r17 pins the images the required gates actually execute**: the four default container images in the real-system drills were floating tags (including `alpine:latest`, which is republished constantly); they are now full sha256 manifest digests, enforced by `internal/cicontract.CheckImagePins` and `CheckNoImageOverrides` inside the already-required Go suite. Digests were verified locally with zero downloads (linux/amd64, `plaintext-audit` rc=0, `vip-failover` rc=0); the GitHub runner's first digest pull was NOT verifiable in this environment (registry probe returned EOF), so digests must be rotated deliberately when upgrading base images. Product code, the nine required gates and the remaining-open list are unchanged.

**quack-proxy** is a lightweight Go daemon that manages multiple DuckDB+[Quack](https://duckdb.org/docs/stable/extensions/quack.html) child processes with built-in health checks, automatic restart, and HAProxy config generation. Think of it as the "systemd for DuckDB Quack endpoints" — except you configure it in one YAML file and it handles everything. It does **not** provide control-plane redundancy, automatic failover, or a unified query endpoint.

---

## Why?

DuckDB's [Quack protocol](https://duckdb.org/2026/05/20/quack.html) (v1.5.2+) natively solves multi-client concurrent writes. But DuckDB doesn't provide process management or service orchestration. Before quack-proxy, running a production Quack cluster meant:

- Hand-writing shell scripts for each database file
- Managing N systemd units for N DuckDB files
- Custom health checks with cron+curl
- Manually generating and maintaining HAProxy configs

quack-proxy does all of this with a single YAML and a single binary.

---

## Where It Sits

DuckLake's own manifesto deliberately leaves out the serving layer — concurrent
users, authentication, authorization, and connection routing. Several projects
now fill that gap; quack-proxy occupies the **minimal** end of it:

| | [QoD](https://github.com/starlake-ai/qod) | [azure-ducklake-quack](https://github.com/MiguelElGallo/azure-ducklake-quack) | quack-proxy |
|---|---|---|---|
| Form | Python gateway + control-plane Postgres | Rust gateway, Azure-only (Easy Auth / Entra / Bicep) | **Go single binary, no external services** |
| Auth | Pluggable DB / JWT / OIDC / SCIM | Azure Entra groups only | Engine-side hooks (`init_sql` → any authn/authz you can express in SQL) |
| RBAC | Table/row/column, enforced on the bound plan, always-on | 2 connection roles (reader/writer) | Your policy, in the engine — e.g. [duckdb-luajit](https://github.com/alitrack/duckdb-luajit) RBAC with default-deny |
| Target | Multi-tenant warehouse platform | Azure-native deployment | Minimal Quack service / shard supervisor |

If you need multi-tenant billing, BI-tool JDBC access, or agent branch-and-merge
workflows, use QoD. If you need one static binary supervising a handful of
DuckDB files with auth enforced *inside the engine* rather than by a proxy-side
regex, quack-proxy is that.

> Proxy-side SQL filtering is bypassable (DuckDB's own security notes call out
> `WITH x AS (SELECT 1) INSERT ...` style bypasses against text matching).
> Engine-side hooks judge every statement the server actually executes, which
> is why quack-proxy ships RBAC as `init_sql` instead of a built-in ACL.

---

## Quick Start

### One-Click (No Go install needed)

Download, **read**, then run the helper script. Do not pipe remote scripts
straight into `bash`: it executes unpinned, unreviewed code.

```bash
curl -fLO https://raw.githubusercontent.com/alitrack/quack-proxy/main/scripts/quack-start.sh
less quack-start.sh                       # review before executing
bash quack-start.sh ./my-data mytoken 9491
```

This starts a Quack server on port 9491 with token `mytoken`. Connect immediately:

```sql
LOAD quack;
CREATE SECRET (TYPE QUACK, TOKEN 'mytoken');
ATTACH 'quack:localhost:9491' AS remote;
```

### Install

```bash
go install github.com/alitrack/quack-proxy/cmd/quack-proxy@latest
```

### Configure

```yaml
# quack-proxy.yaml
global:
  log_level: info
  pid_file: /tmp/quack-proxy.pid

listener:
  bind_host: 127.0.0.1    # loopback by default; see SECURITY.md before exposing
  port_start: 9491
  health_interval: 5s

shards:
  - name: analytics
    database: ./data/analytics.db
    token: replace-with-your-own-secret   # required by default (PRD §4.2)

  - name: logs
    database: ./data/logs.db
    token: replace-with-another-secret
```

**Tokens are required by default.** PRD §4.2 (implemented in r14) keeps shard
credentials off disk: `status.json` contains no `token` field unless you
explicitly set `global.persist_tokens: true` (which restores the legacy
32-character auto-generated token, persisted with mode 0600). With the default
off, an omitted token cannot be generated *and* distributed, so config loading
fails loud instead of starting a shard nobody can authenticate to. Manage the
tokens in your own secret store.

Create the data directory — the `.db` files **do not** need to exist (DuckDB
creates a fresh database for each on first open), but their **parent
directory does**:

```bash
mkdir -p ./data
```

### Run

```bash
# Start the daemon (starts all DuckDB+Quack processes)
quack-proxy start

# Check status
quack-proxy status

# Generate HAProxy config (single shard only — a port-forward, not a replica group)
quack-proxy gen-proxy

# Hot reload config
quack-proxy reload

# Stop gracefully
quack-proxy stop
```

### Connect

quack-proxy auto-generates an auth token for each shard. **Tokens are
redacted from `status` by default** — they are the only credential a Quack
endpoint has — so ask for them explicitly:

```bash
quack-proxy status --json --show-tokens | jq -r '.shards[] | "\(.name): token=\(.token) port=\(.port)"'
```

The status file does contain the tokens; it is written with mode `0600` and
must be treated as a secret. See [SECURITY.md](docs/SECURITY.md).

Then connect from DuckDB:

```sql
LOAD quack;

-- Register the token as a DuckDB secret (REQUIRED before ATTACH)
CREATE SECRET (TYPE QUACK, TOKEN 'your-token-here');

-- Now ATTACH each shard
ATTACH 'quack:localhost:9491' AS analytics;
ATTACH 'quack:localhost:9492' AS logs;

-- Cross-shard query!
SELECT a.date, a.revenue, l.error_count
FROM analytics.events a
JOIN logs.errors l ON a.date = l.date;
```

> **⚠️ ATTACH without CREATE SECRET will fail with "Could not find a Quack authentication token".**

---

## Architecture

```
quack-proxy (Go daemon, ~10MB RSS)
├── Process Supervisor
│   ├── duckdb analytics.db → Quack :9491
│   ├── duckdb logs.db      → Quack :9492
│   └── duckdb users.db     → Quack :9493
├── Health Check Loop (every 5s)
│   └── HTTP GET / → unhealthy? (after a 30s startup grace) kill whole
│       process group → restart (exponential backoff 1s→30s; parks a shard
│       in "stopped" after 10 failed tries)
├── Status File (status.json — live shards, ports, tokens, uptime)
├── Signal Handler (SIGHUP incremental reload, SIGTERM shutdown)
├── HAProxy Config Generator
└── Optional: Coordinator DuckDB
    └── ATTACH all healthy endpoints
```

Each DuckDB process gets:
- The signed core `quack` extension must be **pre-installed** (`INSTALL quack;`).
  Boot SQL only runs `LOAD quack;` — quack-proxy deliberately never downloads
  or executes remote extension code at shard start (see [SECURITY.md](docs/SECURITY.md)).
  `INSTALL quack FROM community` is wrong on v2.0.0 (HTTP 404; `quack` is a core
  extension there).
- A cryptographically random 32-char auth token (or user-specified)
- Strict Host-header checking by default; set `listener.allow_other_hostname: true` only when clients connect through a VIP or DNS name
- Its own process group, so a stop takes the whole tree (wrapper + engine +
  keep-alive) with it — no orphaned listener, no leaked FDs
- Graceful shutdown via SIGTERM to the group (10s timeout → SIGKILL)

### Shard lifecycle

Each shard moves through a small set of states, all reflected in `status` and
the status file:

| State | Meaning |
|-------|---------|
| `starting` | Process spawned; Quack not yet answering. For the first **30s** after the cold start, a failing health check keeps it here rather than restarting it — this absorbs a slow first boot. A restart deliberately keeps the ORIGINAL start timestamp, so a fresh grace cannot swallow the exponential backoff (R-17). |
| `healthy` | Answering the health check **and** having passed the auth probe. Until the probe reports, a listening shard stays `starting` and publishes no attach string. |
| `auth-failed` | The endpoint listens but refuses a real client round-trip made with the shard's own token. **No restart is attempted** — a restart cannot repair the engine binary — and no `coordinator_attach_sql` is published. Fix `duckdb_bin`. |
| `unhealthy` | Failing the health check and inside a restart backoff window. |
| `stopped` | The shard exhausted its retry budget (10 consecutive failed restarts) and has been parked. It stays stopped (no further churn) until `reload` revives it with a fresh budget, or the daemon is restarted. A healthy response clears the accumulated counter. |

On each unhealthy detection after the startup grace, the shard is restarted
with an exponential backoff (1s → 2s → 4s … capped at 30s) and a running
attempt counter. A healthy response resets both. The design goal is: a
transient blip recovers within a second or two, a genuinely broken shard stops
churning after a bounded number of tries and reports itself clearly, instead
of killing-and-relaunching forever.

#### The auth probe: a listening port is not a usable shard

Once a shard's port answers, quack-proxy makes **one real client round-trip**
(`quack_query(..., token := <the shard's own token>)`) and records the outcome
in `status` as `auth_probe` (`"ok"` or `"failed: …"`). This exists because
engines whose Quack token authentication is broken behave exactly like healthy
ones from the outside: the process runs, the port accepts connections, and the
HTTP health check returns 2xx — while every client, including the server's own
token, is answered `Authentication failed`. Some DuckDB 2.0.0 alphas do this.

Verified engine builds (`auth probe` pass, client writes work):

| DuckDB | Status |
|---|---|
| `v2.0.1` (8d78114e90) | works |
| `v2.0.0` (d54cbbe3df) | works |
| `v2.0.0-alpha43355`, `v2.0.0-alpha48730` | works |
| `v2.0.0-alpha39998` | **token auth broken** — always set `global.duckdb_bin` to a build you verified |

If the probe fails, the shard lands in `auth-failed`, `status` stops
advertising an attach string for it, and the daemon logs one ERROR line naming
the binary. Pin `global.duckdb_bin` to the path you verified rather than relying
on whatever `duckdb` resolves to on `PATH`.

---

## Historical benchmark (not a product metric)

> **⚠️ These numbers do NOT measure quack-proxy.** The benchmark hit an empty
> database's `GET /` RPC greeting **directly on the shard ports**; the daemon
> is not in the data path and HAProxy was not used. At c=50 throughput fell to
> ~33K req/s with P99 ~14.5ms, and the engine was a nightly build. Treat the
> table below as a historical note about the Quack endpoint, not as product
> performance. For an end-to-end `ATTACH/SELECT` measurement against a real
> Quack endpoint, run `bash scripts/quack-e2e.sh --benchmark 200` (R-08).

Benchmarked on a single WSL2 VM with 2 shards, c=20 concurrent connections:

| Metric | Value |
|--------|-------|
| **Single shard QPS** | 83,369 req/s (c=10) |
| **Dual shard combined** | 168,915 req/s (c=20 each) |
| **P99 latency** | <1.5ms |
| **Error rate** | 0.00015% (3 errors / 2M requests) |
| **DuckDB memory** | ~38MB RSS per shard (stable under load) |
| **quack-proxy memory** | ~10MB RSS (idle) |

**Fault recovery:**
- Single shard kill: recovers in ~2-6s
- Dual shard kill: both recover in ~6s
- 30+ minute sustained load: zero health check failures, zero memory growth

See [docs/benchmark.md](docs/benchmark.md) for full methodology and raw data.

**Real end-to-end reference (2026-11-13, pinned DuckDB v2.0.0, Linux container):**
`scripts/quack-e2e.sh --benchmark 50` produced **50 sequential `SELECT`
round-trips in 221 ms (~226 qps)** on the first run and **312 ms (~160 qps)**
on the final three-run regression. This is a single-client latency measurement
over the actual Quack protocol, *not* a throughput benchmark and **not**
comparable to the greeting-only numbers above; the spread is expected for a
single sequential client on a shared host. Reproduce with
`bash scripts/quack-e2e.sh --benchmark 200`.

---

## Commands

| Command | Description |
|---------|-------------|
| `start [-c config.yaml]` | Start daemon with all shards |
| `stop` | Graceful shutdown: SIGTERM to every shard's process group (SIGKILL after 10s) |
| `status [--json] [--show-tokens]` | Show shard health, uptime and restarts from the live status file. **Tokens are redacted unless `--show-tokens` is passed.** |
| `reload` | SIGHUP → re-parse config → incremental update (start added shards, stop removed ones, revive `stopped` shards, regenerate HAProxy config). Field-level diff: a changed shard is restarted; changing `duckdb_bin` / `bind_host` / `allow_other_hostname` / `allow_unsigned_extensions` restarts all shards; health-only settings take effect without a restart. Editing `init_sql_file`'s contents also triggers a restart. Reload HAProxy itself manually (R-07). |
| `gen-proxy [-c config.yaml]` | Generate HAProxy config (works with no daemon running). **Only a single shard may be fronted**: different `.db` files are partitions (non-deterministic round-robin), and the same `.db` cannot be opened by two DuckDB writers (file-lock conflict). Multi-shard backends are rejected (R-04). |
| `version` | Print version |

---

## Configuration Reference

See [`quack-proxy.example.yaml`](quack-proxy.example.yaml) for a fully annotated example.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `global.log_level` | string | `info` | debug, info, warn, error |
| `global.pid_file` | string | `/var/run/quack-proxy/quack-proxy.pid` | PID file path |
| `global.status_file` | string | `/var/run/quack-proxy/status.json` | Live cluster state (shards, ports, tokens, restarts) written every health-check cycle; read by `status` and `gen-proxy` |
| `global.duckdb_bin` | string | `duckdb` | DuckDB host binary/script. Can be a wrapper (e.g. a python-hosted engine) — invoked as `<duckdb_bin> <db>` with boot SQL on stdin. **Supported baseline: v2.0.0 stable** (`scripts/quack-e2e.sh` / `prefetch-quack-e2e.sh` pin the linux-amd64 CLI by SHA256). Alpha CLI builds have served broken token auth. On an offline host, pre-install the signed `quack` **and `httpfs`** core extensions — DuckDB v2.0.0 needs httpfs for the crypto backend when writing a disk database (r10). |
| `global.allow_unsigned_extensions` | bool | `false` | Adds `-unsigned` to the DuckDB invocation. Only enable after pre-installing and hash-verifying the extensions; unsigned extensions can load arbitrary native code. |
| `listener.allow_other_hostname` | bool | `false` | Relaxes Quack's Host-header check. Needed only when clients reach a shard via a VIP or DNS name. |
| `listener.bind_host` | string | `127.0.0.1` | Quack listen address. **Loopback by default**; exposing to a LAN sends plaintext HTTP with the token as the only credential — terminate TLS externally. |
| `listener.port_start` | int | `9491` | First port (auto-increments) |
| `listener.health_path` | string | `/` | Health check HTTP path. Must be a plain absolute path; schemes, `//`, userinfo (`@`) and whitespace are rejected (R-09). This is a liveness probe, not a Quack protocol handshake. |
| `listener.health_interval` | duration | `5s` | Health check interval (picked up on reload) |
| `listener.health_expect` | string | — | Optional substring the health response body must contain. A bare 2xx from an unrelated HTTP server on the shard port is otherwise a false positive (R-09). On DuckDB v2.0.0 pin `"This is a DuckDB Quack RPC endpoint"`; max 256 bytes. Not a protocol handshake or auth check. |
| `shards[].name` | string | *required* | Logical shard name (unique); must match `^[A-Za-z_][A-Za-z0-9_]*$` — it becomes an ATTACH alias, a temp-file name and an HAProxy server name. |
| `shards[].database` | string | *required* | Path to `.duckdb` file. The **directory** must exist; the `.db` file itself may not (DuckDB creates a new database on first open — a fresh shard is legitimate). A single quote or newline in the path is rejected. |
| `shards[].port` | int | auto | Override auto-assigned port |
| `shards[].token` | string | auto-generated | Quack auth token (crypto-random when unset) |
| `shards[].init_sql` | string | — | SQL injected at shard boot, after `LOAD quack` and before `quack_serve`. Use for RBAC hooks, extensions, seed setup. Delivered verbatim — multi-statement and dollar-quoted bodies are safe. |
| `shards[].init_sql_file` | string | — | Path to a `.sql` file appended after `init_sql`. The file must exist: a missing path fails shard startup loudly. |
| `proxy.enabled` | bool | `false` | Enable HAProxy config gen |
| `proxy.output` | string | — | HAProxy config output path |
| `proxy.bind_port` | int | — | HAProxy frontend port (required for `gen-proxy`) |
| `proxy.mode` | string | `roundrobin` | `roundrobin` or `leastconn`. Only meaningful with a single backend shard; multi-shard configs are rejected (R-04). |
| `proxy.ssl` | object | — | **Not implemented.** Setting `enabled: true` is rejected at config load rather than silently ignored; terminate TLS in an external proxy (R-05). |

---

## Per-shard RBAC via `init_sql`

`init_sql` runs inside each shard at boot — before the server accepts
connections. A common use is compiling a Lua policy
([duckdb-luajit](https://github.com/alitrack/duckdb-luajit)) and wiring quack's
authn/authz hooks:

```yaml
shards:
  - name: sales
    database: /data/sales.duckdb
    port: 19601
    token: server-side-token
    init_sql_file: /etc/quack-proxy/rbac-init.sql
```

`rbac-init.sql` shape (full example in `examples/`):

```sql
INSTALL luajit FROM community;
LOAD luajit;
SELECT * FROM luajit_module(mode := 'compile', sql_name := 'rbac', source := $rbac$
-- ...policy functions authn(p)/authz(p)...
return function(p)
  if p.op == 'authn' then return authn(p) end
  if p.op == 'authz' then return authz(p) end
end
$rbac$);
CREATE MACRO rbac_authn(sid, ct, st) AS (luajit_s('rbac', {op:'authn', sid:sid, client_token:ct, server_token:st})::BOOLEAN);
CREATE MACRO rbac_authz(sid, query) AS (luajit_s('rbac', {op:'authz', sid:sid, q:query})::BOOLEAN);
SET GLOBAL quack_authentication_function = 'rbac_authn';
SET GLOBAL quack_authorization_function = 'rbac_authz';
```

Verified rules (2026-11-09, DuckDB 2.0.0 stable engine):

- The two `SET GLOBAL` lines MUST come **after** `LOAD quack` (settings only
  exist once the extension is loaded) and **before** `quack_serve`.
- Quack endpoints speak **plaintext HTTP** (`disable_ssl := true` is hardcoded
  by quack-proxy). Bind to loopback unless you terminate TLS in an external proxy.
- Inside authn/authz, never re-enter DuckDB (`_duckdb_query`) during the
  handshake — it deadlocks the shard. Keep policy state in Lua memory.
- `quack_authorization_function` returns the SQL to actually execute when the
  macro is not cast to BOOLEAN (a rewrite hook, not just a gate).
- Lua policy bodies must be valid Lua — an early compile failure makes every
  token fail with `Authentication failed`.

### RBAC v2: scoped tokens + persistent audit (`examples/rbac2-init.sql`)

Built on the same hook mechanism, v2 adds what a multi-user shard actually
needs. E2E 13/13 PASS (`examples/rbac2-e2e.py`, 2026-11-16):

- **Scoped tokens** — each token carries a verb ceiling (`RO`/`RW`): an `RO`
  token cannot write even where its role holds write grants. `max_rows` and
  `stmt_timeout_ms` fields are reserved on the token record.
- **Write-bypass classification** — `WITH x AS (...) INSERT ...` shapes (the
  bypass DuckDB's own security notes flag against text matching) are
  classified as writes and stopped by the ceiling.
- **Fail-closed on unclassified statements** — `SET`, `CALL`, `PRAGMA`, …
  are denied unless explicitly classified.
- **Persistent audit with protected readout** — every authn/authz decision is
  written to an `rbac_audit` table (best-effort, pcall-guarded) and to an
  in-memory ring (4096) that never blocks the hook. The audit table itself is
  denied to every wire token — reading it would leak other users' queries.
  Operators read it from the shard host directly:
  `SELECT * FROM rbac_audit ORDER BY seq DESC LIMIT 50;`
- **Unknown-table default-deny** — a query naming any table outside the grant
  list is denied (the table need not exist).

```yaml
shards:
  - name: mimic
    database: /data/mimic.duckdb
    init_sql_file: /etc/quack-proxy/rbac2-init.sql   # v2 example
```

---

## Integration with duckdb_fdw

quack-proxy pairs naturally with [duckdb_fdw](https://github.com/alitrack/duckdb_fdw) for PostgreSQL-to-DuckDB federation:

```sql
-- Only meaningful when gen-proxy fronted a SINGLE shard (a port-forward).
-- For multiple shards, ATTACH each one separately instead: different .db
-- files are partitions, and the same .db cannot have two DuckDB writers (R-04).
CREATE SERVER quack_cluster FOREIGN DATA WRAPPER duckdb_fdw
OPTIONS (quack_host 'localhost:9490');  -- HAProxy VIP

CREATE USER MAPPING FOR current_user SERVER quack_cluster
OPTIONS (quack_token 'token_from_status_json');

IMPORT FOREIGN SCHEMA "remote" FROM SERVER quack_cluster INTO public;
```

See [PRD.md](docs/PRD.md) §6 for more integration patterns.

---

## Troubleshooting

### "Could not find a Quack authentication token"

This means you tried `ATTACH` without registering the token first. **Always run CREATE SECRET first:**

```sql
LOAD quack;
CREATE SECRET (TYPE QUACK, TOKEN 'your-token');
ATTACH 'quack:localhost:9491' AS remote;
```

Get your token from `quack-proxy status --json --show-tokens` or the `.token` file created by `quack-start.sh`.

### "shard unhealthy, restarting"

Usually caused by:

1. **Database path doesn't exist or no write permission** — ensure the parent directory exists:
   ```bash
   mkdir -p ./data
   ```

2. **DuckDB not installed or mismatched** — the supported baseline is **v2.0.0 stable**:
   ```bash
   duckdb --version
   ```

3. **Quack extension not pre-installed** — `LOAD quack` at shard boot
   requires it. Install the SIGNED CORE extension once per host:
   ```sql
   INSTALL quack; LOAD quack;   -- NOT "FROM community" (404 on v2.0.0)
   ```
   quack-proxy intentionally never runs `INSTALL` at shard start.

4. **Port already in use** — another process (or a leftover from a previous
   run) holds the shard's port. Find and stop it, then the next restart
   reclaims the port:
   ```bash
   lsof -nP -iTCP:9491 -sTCP:LISTEN
   ```

### A shard is stuck in `stopped`

This is the parking state a shard enters after **10 consecutive failed
retries** (see *Shard lifecycle*). It is not a crash you can wait out — the
supervisor has deliberately stopped churning. A healthy probe clears the
accumulated counter, and `reload` revives a parked shard with a fresh budget.
Fix the underlying cause, then one of:

- **`quack-proxy reload`** (SIGHUP) — re-reads the config; a shard that is
  still configured is started fresh with a reset retry budget.
- **`quack-proxy stop && quack-proxy start`** — full restart of the daemon
  and all shards.

The usual root causes are the same as *"shard unhealthy, restarting"* above,
plus: the database directory is not writable by the `quack-proxy` user, the
`duckdb_bin` path is wrong / not executable, or the Quack extension can't be
installed (no network to the extension repository, or an extension build
missing for your DuckDB version — see `examples/duckdb_serve_host.py` for the
version matrix).

### ATTACH fails with connection error

By default shards listen on `127.0.0.1` only. To accept connections from
another machine you must **deliberately** widen the exposure:

> **⚠️ Quack speaks plaintext HTTP and the token is the only credential.**
> Binding `0.0.0.0` on an untrusted LAN exposes that token to packet capture.
> This is **not hypothetical**: `scripts/verify-plaintext.sh` starts the real
> daemon on a container bridge and an independent `AF_PACKET` sniffer reads the
> token, the `Authorization: Bearer` header and the SQL body straight out of
> the TCP payload, while a TLS control channel on the same segment leaks
> nothing. Terminate TLS in an external proxy and restrict source IPs first.
> See [SECURITY.md](docs/SECURITY.md).

```yaml
listener:
  bind_host: 0.0.0.0
  allow_other_hostname: true   # only needed when clients use a VIP / DNS name
```

For a manually started `quack_serve` (without quack-proxy) the equivalent is:

```sql
CALL quack_serve('quack:0.0.0.0:9491', token := 'mypass', allow_other_hostname := true);
```

## Non-Goals

quack-proxy is a **process supervisor**, not a distributed database:

- ❌ No distributed transactions (use DuckDB's ATTACH for cross-shard queries)
- ❌ No automatic sharding / partitioning (you define shard layout)
- ❌ No Web UI or dashboard (CLI only)
- ❌ No cross-machine endpoint discovery (v0.1: single-machine focus)
- ✅ Manages DuckDB+Quack processes reliably
- ✅ Health checks + auto-restart
- ✅ HAProxy config generation
- ✅ Signal-based reload

Not provided (see [SCOPE.md](docs/SCOPE.md)):

- ❌ Control-plane redundancy / daemon failover — the daemon itself is a SPOF
- ❌ Cross-machine discovery, a resident Coordinator, or an in-process load balancer
- ❌ TLS termination inside quack-proxy

---

## Development

```bash
git clone https://github.com/alitrack/quack-proxy.git
cd quack-proxy
go build -o quack-proxy ./cmd/quack-proxy
./quack-proxy version
```

Requirements:
- Go 1.25+
- **DuckDB v2.0.0 stable, pinned** (linux-amd64 CLI SHA256
  `c6507206baf7793c6013dbd82368469a45f20dc0e17a6f92f204602f652ed2bc`). Older
  docs mentioned ≥1.5.2 and the historical benchmark used a 1.5.2 nightly;
  those are historical notes, not supported baselines. The `quack` extension
  is a **signed core** extension: pre-install it with `duckdb -c "INSTALL quack;"`.
  `INSTALL quack FROM community` returns HTTP 404 on v2.0.0 (SCOPE §4).
- Linux or macOS. **No Windows binary is published (r9 / R-18)**: PRD §8
  has always listed Windows as a non-goal. A real-Windows drill showed the
  old artifact's `version` worked while `start` failed with a misleading
  "duckdb_bin not executable" (Windows has no POSIX mode bits). CI still
  cross-compiles windows/amd64 for source compatibility, and the required
  `windows-runtime-guard` job runs on windows-latest to prove the binary now
  refuses to start with an actionable error pointing at Linux/macOS or WSL2.

**Running under systemd:** a ready-to-install unit with hardening and the
"why we don't auto-respawn the daemon" rationale lives in
[`deploy/systemd/quack-proxy.service`](deploy/systemd/quack-proxy.service).
r7 adds a **real install drill** (`scripts/systemd-install-drill.sh`): it
installs that exact unit on a booted systemd, starts it as the unprivileged
`quack-proxy` account, asserts `RuntimeDirectory` ownership/mode, 0600
pid/status files, a healthy shard and token redaction, then stops it and
asserts port/pid/status/process-group teardown. Its negative control proves
the same binary run as that account *without* `RuntimeDirectory` is refused.
Verified on real systemd 255 (3/3) and enforced as a required CI job.

**CI:** [`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs gofmt,
`go vet`, `go test -race -cover` with a coverage gate (Go 1.25 / 1.26 / 1.27),
a **required** version-pinned `govulncheck` job (go1.27.4, zero-finding
baseline), Linux + Windows cross-builds, the Python splitter suite, and nine
**required** real-system gates: `crash-drill` (daemon SIGKILL / R-22),
`deploy-verify` (`scripts/verify-deploy.sh`: real `haproxy -c -f`, live
forwarding, `systemd-analyze verify`, each with a negative control, plus the
R-23 hardening check), `systemd-drill` (above), `vuln-scan`,
`plaintext-audit` (below), `windows-runtime-guard` (a real
windows-latest runner proves the cross-compiled binary refuses to start
honestly, R-18), `quack-e2e` (strictly-offline real engine, below),
`vip-failover` (real HAProxy + floating IP, below), and
`engine-tls-capability` (r12: strictly-offline proof that the **pinned
DuckDB v2.0.0 engine** natively supports TLS, below). **Releases publish
linux/darwin only**; the Windows build
is a source-compatibility check, not a supported artifact. The **real
DuckDB + Quack E2E job** is **required on every event, including pull
requests** (r10; no advisory left): `prefetch-quack-e2e.sh` fills a
SHA256-verified cache with the pinned CLI + signed `quack` + signed `httpfs`
once on the runner (SHA-pinned `actions/cache`), and `quack-e2e.sh` then runs
**strictly offline** from it (`QUACK_E2E_OFFLINE=1`: no download, no
`INSTALL`; a missing or corrupt cache is a hard failure). It is proven in a
`--network none` container. Residual risk, stated plainly: a first-ever cache
miss still needs the upstream once. Every GitHub Action is
pinned to a full commit SHA; release binaries are built with the same patched
`go1.27.4` toolchain the vulnerability gate checks, and release checksums are
signed with cosign keyless OIDC (see [SECURITY.md](docs/SECURITY.md)).

**The gate contract is itself tested (r13, extended in r14):** `internal/cicontract`
parses the real `ci.yml` / `release.yml` inside the already-required
`go test ./...` job and asserts that all nine gates exist, none can be skipped
(`if`) or fail silently (`continue-on-error`), every action is SHA-pinned,
both consumers of the pinned engine cache use the **identical path and key**
(with the key still carrying the three SHA256 pin prefixes read from
`prefetch-quack-e2e.sh`), `engine-tls-capability` waits for `quack-e2e`, and
the offline switches are set. Nine negative controls mutate the real YAML one
property at a time and require **exactly** the intended check to fail — so
this is a live contract, not a comment. r14 adds `CheckScriptPins`: the three
offline scripts (`prefetch`, `quack-e2e`, `verify-engine-tls`) must declare
the **same** engine version, extension hashes and extension path as the cache
producer, with three single-constant drift negatives. "Remember to update all
pins when upgrading DuckDB" is now enforced too. It adds no CI job and no
downloads. The full CI chain
(prefetch offline assertion → real-engine E2E → engine TLS audit) was also
reproduced in a single `--network none` container (`OFFLINE_CI_CHAIN_PASSED`,
3/3 RC=0).

**HAProxy hardening (R-23):** the default generated config passes the vendor's
parser but contains no active `chroot`/`user` — those need a host-local
account and directory that quack-proxy cannot provision, and HAProxy >= 3.x
warns when started as root without them. For production set
`proxy.hardened: true` so `gen-proxy` emits
`chroot /var/lib/haproxy` / `user haproxy` / `group haproxy`, and first run:

```bash
sudo bash scripts/verify-hardening.sh --proxy-bin ./quack-proxy
```

That script pre-provisions the account and root-owned 0755 chroot, then proves
with the real HAProxy binary that the process drops root and still forwards
traffic; it also asserts (negative control) that an unprepared host is
rejected. Verified on HAProxy 3.4.7 and 2.8.31. Keeping the option off is a
deliberate choice: injecting the directives onto an unprepared host makes
HAProxy refuse to start.

**HAProxy + VIP drill (r11):** `scripts/verify-vip-failover.sh` is a required
CI job that runs the **generated** config on two real HAProxy 2.8 containers
and moves a real `/32` floating IP between them (gratuitous ARP, no client-side
ARP flush). It asserts `VIP_FAILOVER_DRILL_PASSED` with negative controls:
an unowned VIP must be unreachable, a withdrawn VIP must stop answering, a
multi-shard config must be refused by `gen-proxy` (R-04), and a broken config
must be refused by `haproxy -c -f`. **Read this carefully:** the VIP is moved
by the script, not by quack-proxy. Each node has its own backend, and the
drill asserts that the answering node changes after failover
(`SESSION_DISCONTINUITY=1`) — the entry point recovers, sessions and data do
not. This is an integration check of the generated config, **not** an HA
feature. Run it with `bash scripts/verify-vip-failover.sh` (needs docker; pass
`--proxy-bin` / `--stub-bin` if Go is unavailable).

**LAN cleartext audit (R-05):** `scripts/verify-plaintext.sh` is a required CI
job that proves the transport risk instead of merely documenting it. It builds
an isolated Docker bridge, starts the **real daemon** with
`listener.bind_host: 0.0.0.0`, and puts a separate sniffer container on the
same segment (`AF_PACKET` raw frames). A client container on that bridge sends
the shard token; the capture contains the token, the `Authorization: Bearer`
header and the SQL body. A TLS control channel on the same L2 segment carries
a second canary that must **not** be visible (negative control), and the real
CLI is checked to reject `proxy.ssl.enabled: true`. Run it locally with:

```bash
bash scripts/verify-plaintext.sh                      # needs docker + go + openssl
```

The **transport** part is an **audit, not a fix**: it asserts a known weakness.
If TLS termination is ever added, the job will (correctly) fail and the
assertions plus docs must be updated together — do not delete the gate. Since
r14 the same required job also **hard-asserts the separate token-at-rest fix**:
`status.json` must contain neither the token value nor a `token` field by
default (`STATUS_TOKEN_AT_REST=0`), so a regression that silently starts
persisting credentials fails CI immediately. The explicit opt-in path is
covered by the Go suite (`internal/supervisor/r14_token_at_rest_test.go`). Since **r15** the same required job also **accepts the PRD's existing external-TLS-termination deployment**: a second real daemon whose shard binds only `127.0.0.1`, a real HAProxy 2.8 TLS terminator sharing its netns, and `AF_PACKET` assertions that no credential is readable on the LAN, that the plain backend leg never reaches the wire, and that a LAN client cannot connect to the shard directly. `PLAINTEXT_INJECT_CLEARTEXT=1` is the negative control: the job must fail if a cleartext credential ever crosses that port. Still one required job — the gate count stays at nine.

**Engine TLS capability (r12, R-05):** `scripts/verify-engine-tls.sh` is a
required CI job that reuses `quack-e2e`'s pinned, SHA256-verified offline
cache and runs in `--network none`. It proves that the **engine** this project
ships against natively supports TLS: `quack_serve(disable_ssl := false,
ssl_cert_file := ..., ssl_key_file := ...)` returns an `https://` endpoint,
`openssl s_client` completes a TLSv1.3 handshake, and a client pinned to the
certificate's SHA256 fingerprint gets `42` back; wrong fingerprint, missing
fingerprint and wrong token are all rejected, while a plaintext control
endpoint still answers (so the negatives are not just "the service died").
The job also asserts that the product boot SQL still hardcodes
`disable_ssl = true`. **Read this as an engine audit, not a product feature:**
quack-proxy still serves plaintext HTTP. **r14 separately fixed token-at-rest**
(`status.json` no longer carries tokens by default); that is a different risk
from cleartext transport, which remains open **only when a shard is wrongly
exposed directly on a LAN**. The earlier wording "built-in TLS needs a design
decision" sounded like an engine limitation; r12 corrected the capability half.
**r15 corrected the rest**: PRD §4.2 (v0.1 original: "Nginx 反向代理做 SSL
termination + 固定 token"), SCOPE §3 (built-in TLS termination is a non-goal)
and the SECURITY operator checklist had **already decided the product does not
provide TLS**; the required `plaintext-audit` job now accepts that existing
decision end to end with a real HAProxy TLS terminator in front of a
loopback-bound shard (no readable credential on the LAN, zero payload on the
backend port, LAN cannot reach the shard directly, plus a cleartext-injection
negative control). **This is operator deployment acceptance, not a product TLS
feature** — `disable_ssl=true`, the dead `proxy.ssl` config and the plain
`gen-proxy` output are unchanged, and operators must still deploy and maintain
the proxy and certificates. Only if the scope is changed to enable built-in
TLS would a new product decision be needed. See
[SECURITY.md](docs/SECURITY.md) and
[docs/REMEDIATION.md](docs/REMEDIATION.md) §2 R-05.

### Testing

This project started as a rapid prototype — the initial development cycle
prioritized manual verification (`curl`, `ps`, direct stress testing) over
unit tests. The main bug found during that phase (signalling the bash wrapper
orphaning the whole child tree) was caught by integration testing, not unit
tests — a reminder that real process-management bugs need real subprocesses.

The Go suite covers all six packages (85 test functions; `go test ./...`
passes clean, `go test -race` runs in CI and passes locally in a Linux
container; total coverage 72.8% — the Windows-only runtime branch cannot be
exercised on a Linux runner). It exercises whole-process-group teardown, restart backoff and
retry parking, the cold-start grace, crypto token generation, config
validation (including injection rejection), health-check identity pinning,
HAProxy output, CLI pid-file handling, lock-freedom of health probes,
parallel shutdown, reload field-level diffs and orphaned-port rejection.

One of those tests exists because the real-engine E2E caught a case the
stub-based suite could not: `cmd.Wait()` only reaps the bash group leader, so
a DuckDB descendant that is slow to exit (or ignores SIGTERM) can keep the
Quack port bound after the daemon has already logged `shard stopped`. The
supervisor now confirms the whole process group is gone and hard-kills any
survivors; `TestStopAllReapsLingeringDescendants` asserts the port is free
immediately when `StopAll` returns (R-21).

Three verification layers close the old "testing illusion" gap (R-15):

- `internal/supervisor/integration_test.go` forks a **real child process
  group** serving HTTP and drives start → external kill → self-heal → stop,
  asserting no orphan survives. It runs under `-race` on Linux/macOS.
- `scripts/quack-e2e.sh` runs a **real pinned DuckDB v2.0.0 + signed Quack
  extension**: correct-token `ATTACH/SELECT`, wrong-token rejection, HTTP 200
  greeting + healthy status, an optional `ATTACH/SELECT` benchmark, and
  cleanup verification. Two modes: **without `--cache-dir`** it downloads the
  pinned CLI and `INSTALL quack` with retries; **with `--cache-dir`** (built by
  `scripts/prefetch-quack-e2e.sh`) it is strictly offline — no download, no
  `INSTALL`, SHA256-verified CLI + `quack` + `httpfs`, extensions seeded into
  an isolated HOME. `QUACK_E2E_OFFLINE=1` makes a bad cache a hard failure.
  r10 proof: a single `--network none` container ran the whole chain to
  `ALL_QUACK_E2E_CHECKS_PASSED` plus 4 negative controls.
- `scripts/verify-engine-tls.sh` (r12) reuses that same pinned cache, runs
  strictly offline, and proves the engine's native TLS capability: an
  `https://` Quack endpoint, a TLSv1.3 handshake, and a correct-fingerprint
  `quack_query` returning `42`. Three rejection negatives (wrong fingerprint,
  missing fingerprint, wrong token) are paired with a plaintext control
  endpoint that must still answer, so the rejections cannot be explained by a
  dead service; a separate `--control-variant` run must exit non-zero. It also
  asserts the product still hardcodes `disable_ssl = true`. Required CI job
  `engine-tls-capability` (`needs: quack-e2e`, so it adds zero downloads).
  **This is an engine capability audit, not a product TLS fix** — R-05 stays
  open; see the LAN cleartext section above.
- `scripts/daemon-crash-drill.sh` SIGKILLs a real daemon and asserts that a
  restart is **refused** while the orphaned shard still owns the port. Before
  the fix, the new daemon reported `healthy` because the orphan answered the
  health probe (R-22). This drill has no external dependencies and runs as a
  required CI job.
- `scripts/systemd-install-drill.sh` installs the shipped unit into a real
  systemd PID 1 and drives enable → start → healthy → stop, asserting the
  non-root account, `RuntimeDirectory`, 0600 files, token redaction and a
  clean teardown. Its negative control (start as that account with no
  `RuntimeDirectory`) must fail. Required CI job.
- `scripts/verify-hardening.sh` starts the real HAProxy binary with the opt-in
  hardened config and asserts privilege drop plus real traffic forwarding,
  with negative controls for an unprovisioned account and (HAProxy >= 3.x)
  the unhardened root warnings (R-23). Required CI step.
- `scripts/verify-vip-failover.sh` moves a real floating IP between two
  HAProxy containers and proves the generated config still forwards traffic,
  while asserting `SESSION_DISCONTINUITY=1` (the backend identity changes, so
  this is NOT data/session HA). Required CI job; see above for the full scope
  caveat.
- `scripts/verify-windows.sh` runs on a real Windows runner (or locally in
  Git Bash): it asserts `version` works but `start` refuses with the explicit
  R-18 / WSL2 message. It deliberately passes a nonexistent config path so
  seeing the platform error proves the guard runs before config loading;
  negative controls reject the old misleading "duckdb_bin not executable",
  `all shards started`, and any leftover pid file (R-18). Required CI job
  `windows-runtime-guard`. Releases no longer ship a Windows artifact.

`startShard` refuses to boot a shard whose port is already bound, with an
explicit "leftover shard from a crashed daemon" error. quack-proxy does not
adopt orphans (that would require a real HA control plane), but it will not
manufacture a false healthy status either. Operators must stop the leftover
process group before restarting.

Tests that rely on POSIX process groups, `signal(0)` and POSIX permission
bits are explicitly skipped on Windows (R-18) rather than faked. r9 removed
the extra skip that was only needed because `lookupBin` misread Windows mode
bits. See [REMEDIATION.md](docs/REMEDIATION.md).

**Real-engine E2E:** r9 re-ran `scripts/quack-e2e.sh` end to end with the
pinned DuckDB v2.0.0 CLI and the official signed core extension supplied
offline (`ALL_QUACK_E2E_CHECKS_PASSED`, 50 SELECTs in 207 ms). **r10 corrected
that wording**: the r9 container still had network, and DuckDB silently
autoloaded `httpfs`. With `--network none`, a disk-backed shard fails with
`read-only crypto module loaded` (`:memory:` works). r10 therefore pins
`httpfs` alongside `quack` and re-proved the full chain in a genuinely
network-less container (`ALL_QUACK_E2E_CHECKS_PASSED`, 50 SELECTs in 230 ms,
clean port release) with 4 negative controls. Offline deployments must
pre-install both extensions.

The Python statement splitter (`examples/duckdb_serve_host.py`) has its own
14-test suite (`examples/duckdb_serve_host_test.py`) that needs no DuckDB
install. CI runs both:

```bash
go vet ./...
go test ./... -race -count=1 -cover
python3 examples/duckdb_serve_host_test.py
bash scripts/prefetch-quack-e2e.sh --cache-dir ./engine-cache
QUACK_E2E_OFFLINE=1 bash scripts/quack-e2e.sh --cache-dir ./engine-cache --benchmark 50
# or, without a cache (downloads the pinned CLI + INSTALL quack on first run):
bash scripts/quack-e2e.sh --benchmark 50
```

### Verifying a downloaded release

Releases ship `SHA256SUMS` plus a cosign keyless signature and certificate.
Verifying proves the checksums were produced by this repository's release
workflow, not just that the download is intact:

```bash
sha256sum -c SHA256SUMS
cosign verify-blob \
  --certificate SHA256SUMS.pem --signature SHA256SUMS.sig \
  --certificate-identity-regexp '^https://github.com/alitrack/quack-proxy/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
```

---

## License

MIT © [alitrack](https://github.com/alitrack)
