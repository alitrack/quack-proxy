# quack-proxy

[![CI](https://github.com/alitrack/quack-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/alitrack/quack-proxy/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev)
[![DuckDB](https://img.shields.io/badge/DuckDB-%E2%89%A51.5.2-FFF000?style=flat&logo=duckdb)](https://duckdb.org)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Built by Hermes](https://img.shields.io/badge/built%20by-Hermes%20Agent-8A2BE2)](https://hermes-agent.nousresearch.com)

> One YAML, one command. Turn any set of DuckDB database files into a highly-available Quack service cluster.

**quack-proxy** is a lightweight Go daemon that manages multiple DuckDB+[Quack](https://duckdb.org/docs/stable/extensions/quack.html) child processes with built-in health checks, automatic restart, and HAProxy config generation. Think of it as the "systemd for DuckDB Quack endpoints" — except you configure it in one YAML file and it handles everything.

---

## Why?

DuckDB's [Quack protocol](https://duckdb.org/2026/05/20/quack.html) (v1.5.2+) natively solves multi-client concurrent writes. But DuckDB doesn't provide process management or service orchestration. Before quack-proxy, running a production Quack cluster meant:

- Hand-writing shell scripts for each database file
- Managing N systemd units for N DuckDB files
- Custom health checks with cron+curl
- Manually generating and maintaining HAProxy configs

quack-proxy does all of this with a single YAML and a single binary.

---

## Quick Start

### One-Click (No Go install needed)

```bash
# Works without installing quack-proxy — uses DuckDB + Quack directly
curl -fsSL https://raw.githubusercontent.com/alitrack/quack-proxy/main/scripts/quack-start.sh | bash -s -- ./my-data mytoken 9491
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
  bind_host: 0.0.0.0
  port_start: 9491
  health_interval: 5s

shards:
  - name: analytics
    database: ./data/analytics.db

  - name: logs
    database: ./data/logs.db
```

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

# Generate HAProxy config for load balancing
quack-proxy gen-proxy

# Hot reload config
quack-proxy reload

# Stop gracefully
quack-proxy stop
```

### Connect

quack-proxy auto-generates an auth token for each shard. Get your tokens and connect:

```bash
# Show tokens for all shards
quack-proxy status --json | jq -r '.shards[] | "\(.name): token=\(.token) port=\(.port)"'
```

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
- Automatic Quack extension install + load
- A cryptographically random 32-char auth token (or user-specified)
- `allow_other_hostname=true` for remote connections
- Its own process group, so a stop takes the whole tree (wrapper + engine +
  keep-alive) with it — no orphaned listener, no leaked FDs
- Graceful shutdown via SIGTERM to the group (10s timeout → SIGKILL)

### Shard lifecycle

Each shard moves through a small set of states, all reflected in `status` and
the status file:

| State | Meaning |
|-------|---------|
| `starting` | Process spawned; Quack not yet answering. For the first **30s** after any (re)start, a failing health check keeps it here rather than restarting it — this absorbs a slow first boot (DuckDB `INSTALL`/`LOAD` can take tens of seconds before `quack_serve` binds). |
| `healthy` | Answering the health check. |
| `unhealthy` | Failing the health check and inside a restart backoff window. |
| `stopped` | **Terminal** — the shard exhausted its retry budget (10 consecutive failed restarts) and has been parked. It stays stopped (no further churn) until the config is reloaded or the daemon is restarted. |

On each unhealthy detection after the startup grace, the shard is restarted
with an exponential backoff (1s → 2s → 4s … capped at 30s) and a running
attempt counter. A healthy response resets both. The design goal is: a
transient blip recovers within a second or two, a genuinely broken shard stops
churning after a bounded number of tries and reports itself clearly, instead
of killing-and-relaunching forever.

---

## Performance

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

---

## Commands

| Command | Description |
|---------|-------------|
| `start [-c config.yaml]` | Start daemon with all shards |
| `stop` | Graceful shutdown: SIGTERM to every shard's process group (SIGKILL after 10s) |
| `status [--json]` | Show shard health, uptime, restarts, and tokens (read from the live status file) |
| `reload` | SIGHUP → re-parse config → incremental update (start added shards, stop removed ones) |
| `gen-proxy [-c config.yaml]` | Generate HAProxy config from the configured shards (works with no daemon running) |
| `version` | Print version |

---

## Configuration Reference

See [`quack-proxy.example.yaml`](quack-proxy.example.yaml) for a fully annotated example.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `global.log_level` | string | `info` | debug, info, warn, error |
| `global.pid_file` | string | `/var/run/quack-proxy/quack-proxy.pid` | PID file path |
| `global.status_file` | string | `/var/run/quack-proxy/status.json` | Live cluster state (shards, ports, tokens, restarts) written every health-check cycle; read by `status` and `gen-proxy` |
| `global.duckdb_bin` | string | `duckdb` | DuckDB host binary/script. Can be a wrapper (e.g. a python-hosted engine) — invoked as `<duckdb_bin> -unsigned <db>` with boot SQL on stdin. Pin a known-good version: alpha CLI builds have served broken token auth (see repo wiki). |
| `listener.bind_host` | string | `0.0.0.0` | Quack listen address |
| `listener.port_start` | int | `9491` | First port (auto-increments) |
| `listener.health_path` | string | `/` | Health check HTTP path |
| `listener.health_interval` | duration | `5s` | Health check interval (picked up on reload) |
| `shards[].name` | string | *required* | Logical shard name (unique) |
| `shards[].database` | string | *required* | Path to `.duckdb` file. The **directory** must exist; the `.db` file itself may not (DuckDB creates a new database on first open — a fresh shard is legitimate). A single quote or newline in the path is rejected. |
| `shards[].port` | int | auto | Override auto-assigned port |
| `shards[].token` | string | auto-generated | Quack auth token (crypto-random when unset) |
| `shards[].init_sql` | string | — | SQL injected at shard boot, after `LOAD quack` and before `quack_serve`. Use for RBAC hooks, extensions, seed setup. Delivered verbatim — multi-statement and dollar-quoted bodies are safe. |
| `shards[].init_sql_file` | string | — | Path to a `.sql` file appended after `init_sql`. The file must exist: a missing path fails shard startup loudly. |
| `proxy.enabled` | bool | `false` | Enable HAProxy config gen |
| `proxy.output` | string | — | HAProxy config output path |
| `proxy.bind_port` | int | — | HAProxy frontend port (required for `gen-proxy`) |
| `proxy.mode` | string | `roundrobin` | `roundrobin` or `leastconn` |

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
- `0.0.0.0` binds speak plain HTTP to LAN clients — `disable_ssl := true` is
  added automatically by quack-proxy.
- Inside authn/authz, never re-enter DuckDB (`_duckdb_query`) during the
  handshake — it deadlocks the shard. Keep policy state in Lua memory.
- `quack_authorization_function` returns the SQL to actually execute when the
  macro is not cast to BOOLEAN (a rewrite hook, not just a gate).
- Lua policy bodies must be valid Lua — an early compile failure makes every
  token fail with `Authentication failed`.

---

## Integration with duckdb_fdw

quack-proxy pairs naturally with [duckdb_fdw](https://github.com/alitrack/duckdb_fdw) for PostgreSQL-to-DuckDB federation:

```sql
-- PG → quack-proxy managed Quack cluster
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

Get your token from `quack-proxy status --json` or the `.token` file created by `quack-start.sh`.

### "shard unhealthy, restarting"

Usually caused by:

1. **Database path doesn't exist or no write permission** — ensure the parent directory exists:
   ```bash
   mkdir -p ./data
   ```

2. **DuckDB not installed or too old** — requires DuckDB ≥ 1.5.2:
   ```bash
   duckdb --version
   ```

3. **Quack extension not available** — install it:
   ```sql
   INSTALL quack; LOAD quack;
   ```

4. **Port already in use** — another process (or a leftover from a previous
   run) holds the shard's port. Find and stop it, then the next restart
   reclaims the port:
   ```bash
   lsof -nP -iTCP:9491 -sTCP:LISTEN
   ```

### A shard is stuck in `stopped`

This is the terminal state a shard parks into after **10 consecutive failed
retries** (see *Shard lifecycle*). It is not a crash you can wait out — the
supervisor has deliberately stopped churning. Fix the underlying cause, then
one of:

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

If connecting from a different machine, the config must use `bind_host: 0.0.0.0` (not localhost):

```yaml
listener:
  bind_host: 0.0.0.0
```

And the server must be started with `allow_other_hostname := true`:

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
- DuckDB ≥ 1.5.2 (with Quack extension support)
- Linux or macOS (the supervisor drives each shard through a `bash` process
  group; a Windows port is a non-goal for v0.1)

**Running under systemd:** a ready-to-install unit with hardening and the
"why we don't auto-respawn the daemon" rationale lives in
[`deploy/systemd/quack-proxy.service`](deploy/systemd/quack-proxy.service).

**CI:** [`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs gofmt,
`go vet`, Linux + Windows cross-builds, the Go suite, and the Python
splitter suite on every push/PR.

### Testing

This project started as a rapid prototype — the initial development cycle
prioritized manual verification (`curl`, `ps`, direct stress testing) over
unit tests. The main bug found during that phase (signalling the bash wrapper
orphaning the whole child tree) was caught by integration testing, not unit
tests — a reminder that real process-management bugs need real subprocesses.

The Go suite covers all five packages (34 tests, `go test ./...` passes
clean). It exercises the parts that used to be wrong: whole-process-group
teardown, restart backoff and retry parking, the slow-boot startup grace,
crypto token generation, config validation (including quote-injection
rejection), health-check logic, HAProxy output correctness, and CLI
pid-file handling. Lifecycle tests use a stub `duckdb_bin` (`true`/`cat`) so
they stay fast and hermetic.

The Python statement splitter (`examples/duckdb_serve_host.py`) has its own
14-test suite (`examples/duckdb_serve_host_test.py`) that needs no DuckDB
install. CI runs both:

```bash
go test ./... -v -count=1
python3 examples/duckdb_serve_host_test.py
```

---

## License

MIT © [alitrack](https://github.com/alitrack)
