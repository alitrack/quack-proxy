# quack-proxy

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev)
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
    database: /data/analytics.db

  - name: logs
    database: /data/logs.db
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

```sql
-- From any DuckDB instance:
ATTACH 'quack:localhost:9491' AS analytics;
ATTACH 'quack:localhost:9492' AS logs;

-- Cross-shard query!
SELECT a.date, a.revenue, l.error_count
FROM analytics.events a
JOIN logs.errors l ON a.date = l.date;
```

Or get the ATTACH SQL automatically:

```bash
quack-proxy status --json | jq -r .coordinator_attach_sql
```

---

## Architecture

```
quack-proxy (Go daemon, ~10MB RSS)
├── Process Supervisor
│   ├── duckdb analytics.db → Quack :9491
│   ├── duckdb logs.db      → Quack :9492
│   └── duckdb users.db     → Quack :9493
├── Health Check Loop (every 5s)
│   └── HTTP GET / → unhealthy? kill → restart
├── Signal Handler (SIGHUP reload, SIGTERM shutdown)
├── HAProxy Config Generator
└── Optional: Coordinator DuckDB
    └── ATTACH all healthy endpoints
```

Each DuckDB process gets:
- Automatic Quack extension install + load
- Random 32-char auth token (or user-specified)
- `allow_other_hostname=true` for remote connections
- Graceful shutdown via SIGTERM (10s timeout → SIGKILL)

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
| `stop` | Graceful shutdown (SIGTERM → children → wait) |
| `status [--json]` | Show shard health, uptime, restarts |
| `reload` | SIGHUP → re-parse config → incremental update |
| `gen-proxy [-c config.yaml]` | Generate HAProxy/nginx config |
| `version` | Print version |

---

## Configuration Reference

See [`quack-proxy.example.yaml`](quack-proxy.example.yaml) for a fully annotated example.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `global.log_level` | string | `info` | debug, info, warn, error |
| `global.pid_file` | string | `/var/run/quack-proxy/quack-proxy.pid` | PID file path |
| `listener.bind_host` | string | `0.0.0.0` | Quack listen address |
| `listener.port_start` | int | `9491` | First port (auto-increments) |
| `listener.health_path` | string | `/` | Health check HTTP path |
| `listener.health_interval` | duration | `5s` | Health check interval |
| `shards[].name` | string | *required* | Logical shard name |
| `shards[].database` | string | *required* | Path to `.duckdb` file |
| `shards[].port` | int | auto | Override auto-assigned port |
| `shards[].token` | string | auto-generated | Quack auth token |
| `shards[].readonly` | bool | `false` | Read-only mode |
| `proxy.enabled` | bool | `false` | Enable HAProxy config gen |
| `proxy.output` | string | — | HAProxy config output path |
| `proxy.bind_port` | int | — | HAProxy frontend port |

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
go build -o quack-proxy ./cmd/...
./quack-proxy version
```

Requirements:
- Go 1.21+
- DuckDB ≥ 1.5.2 (with Quack extension support)

---

## License

MIT © [alitrack](https://github.com/alitrack)
