#!/usr/bin/env bash
# Real DuckDB + Quack end-to-end validation for quack-proxy (R-08 / R-15).
#
# Unlike the Go integration test (which uses a stub endpoint), this script
# starts a REAL DuckDB + Quack shard through `quack-proxy start`, connects a
# second DuckDB client over the Quack protocol with a token secret, verifies
# that a wrong token is rejected, optionally runs an ATTACH/SELECT benchmark,
# and finally stops the daemon and checks the port is released.
#
# Requirements: Linux, bash, curl, sha256sum, tar, go (to build the daemon
# unless --proxy-bin is supplied). WITHOUT --cache-dir it needs network on
# first run to fetch the pinned DuckDB CLI + the signed core `quack` extension.
# WITH --cache-dir (produced by prefetch-quack-e2e.sh) it is STRICTLY OFFLINE:
# no download, no INSTALL; DuckDB loads the signed extension from an isolated
# $HOME. CI sets QUACK_E2E_OFFLINE=1, which makes a missing or corrupt cache a
# hard failure instead of a silent network fallback.
#
# Usage:
#   bash scripts/quack-e2e.sh --cache-dir ./engine-cache   # offline (CI path)
#   bash scripts/quack-e2e.sh                             # download pinned DuckDB
#   bash scripts/quack-e2e.sh --benchmark 200             # also run 200 queries
#   bash scripts/quack-e2e.sh --duckdb /opt/duckdb --proxy-bin ./quack-proxy
#   DUCKDB_VERSION_OVERRIDE=1 bash scripts/quack-e2e.sh --duckdb ... # skip hash check
#
# Exit: 0 = all checks passed, non-zero = first failed check (see message).
set -euo pipefail

# DuckDB is PINNED. The historical benchmark used a 1.5.2 nightly and the
# RBAC notes mention 2.0.0 stable; this is the single supported baseline.
DUCKDB_VERSION="v2.0.0"
DUCKDB_SHA256="c6507206baf7793c6013dbd82368469a45f20dc0e17a6f92f204602f652ed2bc"

# Offline cache pins (r10). Kept in sync with prefetch-quack-e2e.sh; a mismatch
# here is a build error, not a reason to relax the pin. DuckDB re-verifies the
# core extension signatures on LOAD, so a forged cache cannot pass silently.
# httpfs is required for the disk-backed database: DuckDB v2.0.0 otherwise
# starts with a read-only crypto module. Having the file present is enough
# (autoload picks it up); the daemon's boot SQL is intentionally unchanged.
EXT_SHA256="9c893bd543ec2c5253d0c47d5614f90a1bfa1791c735dc6bbcea4e8bd3811a97"
HTTPFS_SHA256="77e6c429aa04f25da7962572f12b19113e0ce46080fa1365ad66017aadcdb68d"
EXT_RELPATH="${DUCKDB_VERSION}/linux_amd64"
CACHE_MANIFEST="quack-e2e-manifest.txt"

PORT="${QUACK_E2E_PORT:-19491}"
TOKEN="${QUACK_E2E_TOKEN:-e2e-secret-token}"
BENCH=0
WORK=""
DUCKDB_BIN=""
PROXY_BIN=""
CACHE_DIR="${QUACK_E2E_CACHE_DIR:-}"
OFFLINE="${QUACK_E2E_OFFLINE:-0}"
# In cache mode this is an isolated HOME holding the signed extension, so both
# the daemon's boot SQL (LOAD quack) and the client's LOAD resolve offline.
# go build deliberately does NOT use it: changing HOME would discard GOCACHE.
DUCK_HOME=""
KEEP=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
  awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "${BASH_SOURCE[0]}"
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --duckdb)     DUCKDB_BIN="$2"; shift 2 ;;
    --proxy-bin)  PROXY_BIN="$2"; shift 2 ;;
    --cache-dir)  CACHE_DIR="$2"; shift 2 ;;
    --work)       WORK="$2"; KEEP=1; shift 2 ;;
    --port)       PORT="$2"; shift 2 ;;
    --benchmark)  BENCH="$2"; shift 2 ;;
    --keep)       KEEP=1; shift ;;
    -h|--help)    usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 1 ;;
  esac
done

if [[ -z "$WORK" ]]; then WORK="$(mktemp -d /tmp/quack-e2e.XXXXXX)"; fi
mkdir -p "$WORK"
DAEMON_PID=""
cleanup() {
  local rc=$?
  if [[ -n "$DAEMON_PID" ]] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill -TERM "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  # Kill any shard group the daemon itself left behind (its leader is a
  # process-group leader, so a negative PID kills bash + duckdb + tail).
  if [[ -f "$WORK/quack-proxy.pid" ]]; then
    pkill -TERM -f "quack-proxy-${PORT}" 2>/dev/null || true
  fi
  if [[ "$KEEP" -eq 0 ]]; then rm -rf -- "$WORK"; fi
  exit "$rc"
}
trap cleanup EXIT

echo "==> work dir: $WORK"

# Only duckdb / the daemon get the isolated HOME. `go build` below keeps the
# real HOME so GOCACHE / GOMODCACHE are not discarded.
run_with_duck_home() {
  if [[ -n "$DUCK_HOME" ]]; then env HOME="$DUCK_HOME" "$@"; else "$@"; fi
}

if [[ "$OFFLINE" == "1" && -z "$CACHE_DIR" ]]; then
  echo "FAIL: QUACK_E2E_OFFLINE=1 requires --cache-dir (or QUACK_E2E_CACHE_DIR)" >&2
  exit 1
fi

# --- 1. pinned DuckDB CLI + signed core extension -------------------------
cache_ready() {
  [[ -n "$CACHE_DIR" ]] || return 1
  local cli="$CACHE_DIR/duckdb-cli-linux-amd64.tar.gz"
  local ext="$CACHE_DIR/quack.duckdb_extension"
  local httpfs="$CACHE_DIR/httpfs.duckdb_extension"
  printf '%s  %s\n' "$DUCKDB_SHA256" "$cli" | sha256sum -c - >/dev/null 2>&1 || return 1
  printf '%s  %s\n' "$EXT_SHA256" "$ext" | sha256sum -c - >/dev/null 2>&1 || return 1
  printf '%s  %s\n' "$HTTPFS_SHA256" "$httpfs" | sha256sum -c - >/dev/null 2>&1 || return 1
  grep -qx "extension_relpath=${EXT_RELPATH}" "$CACHE_DIR/$CACHE_MANIFEST" 2>/dev/null || return 1
  return 0
}

if [[ -n "$CACHE_DIR" ]]; then
  # Cache mode is ALWAYS offline: no curl, no INSTALL. A bad cache must fail
  # loudly rather than silently reintroduce the CDN dependency we removed.
  if ! cache_ready; then
    echo "FAIL: offline cache missing or corrupt: ${CACHE_DIR}" >&2
    echo "      run: bash scripts/prefetch-quack-e2e.sh --cache-dir ${CACHE_DIR}" >&2
    echo "      (QUACK_E2E_OFFLINE=${OFFLINE}; no network fallback in cache mode)" >&2
    exit 1
  fi
  echo "==> verifying pinned offline cache"
  echo "${DUCKDB_SHA256}  ${CACHE_DIR}/duckdb-cli-linux-amd64.tar.gz" | sha256sum -c -
  echo "${EXT_SHA256}  ${CACHE_DIR}/quack.duckdb_extension" | sha256sum -c -
  echo "${HTTPFS_SHA256}  ${CACHE_DIR}/httpfs.duckdb_extension" | sha256sum -c -
  mkdir -p "$WORK/engine"
  tar xzf "$CACHE_DIR/duckdb-cli-linux-amd64.tar.gz" -C "$WORK/engine"
  DUCKDB_BIN="$WORK/engine/duckdb"
  # DuckDB v2.0.0 ignores DUCKDB_EXTENSION_DIRECTORY and the daemon's boot SQL
  # is fixed (LOAD quack, no SET). A per-process HOME is the only way to make
  # BOTH the shard and the client resolve the signed extension offline.
  DUCK_HOME="$WORK/home"
  mkdir -p "$DUCK_HOME/.duckdb/extensions/${EXT_RELPATH}"
  cp "$CACHE_DIR/quack.duckdb_extension" \
     "$DUCK_HOME/.duckdb/extensions/${EXT_RELPATH}/quack.duckdb_extension"
  cp "$CACHE_DIR/httpfs.duckdb_extension" \
     "$DUCK_HOME/.duckdb/extensions/${EXT_RELPATH}/httpfs.duckdb_extension"
  echo "OFFLINE_CACHE_USED=1"
elif [[ -z "$DUCKDB_BIN" ]]; then
  url="https://github.com/duckdb/duckdb/releases/download/${DUCKDB_VERSION}/duckdb-cli-linux-amd64.tar.gz"
  echo "==> fetching pinned DuckDB ${DUCKDB_VERSION}"
  # GitHub Releases has returned transient 5xx for this asset. curl retries
  # transient errors itself; the outer loop also covers connection-level
  # failures so a single CDN hiccup does not decide the whole run.
  fetched=0
  for attempt in 1 2 3; do
    if curl -fsSL --retry 3 --retry-delay 3 --connect-timeout 20 -m 300 \
      -o "$WORK/duckdb.tar.gz" "$url"; then fetched=1; break; fi
    echo "==> download attempt ${attempt} failed; retrying" >&2
    sleep 3
  done
  [[ "$fetched" -eq 1 ]] || { echo "FAIL: cannot download pinned DuckDB CLI: $url" >&2; exit 1; }
  echo "${DUCKDB_SHA256}  $WORK/duckdb.tar.gz" | sha256sum -c -
  mkdir -p "$WORK/engine"
  tar xzf "$WORK/duckdb.tar.gz" -C "$WORK/engine"
  DUCKDB_BIN="$WORK/engine/duckdb"
fi
[[ -x "$DUCKDB_BIN" ]] || { echo "FAIL: duckdb binary not executable: $DUCKDB_BIN" >&2; exit 1; }
echo "==> duckdb: $(run_with_duck_home "$DUCKDB_BIN" --version)"

# Quack is a SIGNED CORE extension on v2.0.0. `INSTALL quack FROM community`
# (as older docs suggested) returns HTTP 404; install it from the core repo
# so no -unsigned flag is needed. Production deployments are expected to
# pre-install the signed extension (SCOPE.md). In cache mode it is already
# seeded into the isolated HOME, so LOAD must succeed with NO network at all.
if run_with_duck_home "$DUCKDB_BIN" -c "LOAD quack;" >/dev/null 2>&1; then
  echo "==> signed core extension: loaded offline (signature verified by DuckDB)"
elif [[ -n "$CACHE_DIR" ]]; then
  echo "FAIL: LOAD quack failed although the signed extension was pre-seeded" >&2
  echo "      expected: ${DUCK_HOME}/.duckdb/extensions/${EXT_RELPATH}/quack.duckdb_extension" >&2
  ls -laR "$DUCK_HOME/.duckdb/extensions" >&2 || true
  exit 1
else
  echo "==> installing signed core extension: quack"
  # The extension CDN has served HTTP 502 in past runs. Retry INSTALL without
  # ever falling back to -unsigned or a community repository: the signature
  # check is part of the supply-chain guarantee (R-14).
  installed=0
  for attempt in 1 2 3; do
    if run_with_duck_home "$DUCKDB_BIN" -c "INSTALL quack; LOAD quack;" \
      > "$WORK/extension-install.log" 2>&1; then installed=1; break; fi
    echo "==> extension install attempt ${attempt} failed; retrying" >&2
    sleep 3
  done
  if [[ "$installed" -ne 1 ]]; then
    echo "FAIL: cannot install signed core extension quack" >&2
    tail -20 "$WORK/extension-install.log" >&2 || true
    exit 1
  fi
fi

# --- 2. build the daemon --------------------------------------------------
if [[ -z "$PROXY_BIN" ]]; then
  echo "==> building quack-proxy"
  (cd "$REPO_ROOT" && go build -o "$WORK/quack-proxy" ./cmd/quack-proxy)
  PROXY_BIN="$WORK/quack-proxy"
fi
[[ -x "$PROXY_BIN" ]] || { echo "FAIL: quack-proxy binary not executable: $PROXY_BIN" >&2; exit 1; }

# --- 3. config ------------------------------------------------------------
cat > "$WORK/config.yaml" <<YAML
global:
  log_level: info
  duckdb_bin: ${DUCKDB_BIN}
  pid_file: ${WORK}/quack-proxy.pid
  status_file: ${WORK}/status.json
listener:
  bind_host: 127.0.0.1
  health_path: /
  health_interval: 2s
shards:
  - name: shard1
    database: ${WORK}/shard1.db
    port: ${PORT}
    token: ${TOKEN}
    init_sql: "CREATE TABLE IF NOT EXISTS probe AS SELECT 42 AS answer;"
YAML

# --- 4. start -------------------------------------------------------------
echo "==> starting quack-proxy (port ${PORT})"
# The daemon forks duckdb, which must inherit the isolated HOME containing the
# signed extension. `env` execs in place, so $! is still the daemon PID.
run_with_duck_home "$PROXY_BIN" start -c "$WORK/config.yaml" > "$WORK/daemon.log" 2>&1 &
DAEMON_PID=$!

port_open() { (exec 3<>"/dev/tcp/127.0.0.1/${PORT}") 2>/dev/null && { exec 3<&-; exec 3>&-; }; }
ready=0
# A cold container (fresh go build + first extension load) can take well over
# 30s to get DuckDB's Quack listener up. Give it 60s; a real regression still
# fails, but a slow cold start no longer looks like one (observed once in r5).
for _ in $(seq 1 240); do
  if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
    echo "FAIL: daemon exited early" >&2; tail -60 "$WORK/daemon.log" >&2; exit 1
  fi
  if port_open; then ready=1; break; fi
  sleep 0.25
done
if [[ "$ready" -ne 1 ]]; then
  echo "FAIL: Quack endpoint never bound" >&2
  echo "--- daemon log (last 60 lines) ---" >&2
  tail -60 "$WORK/daemon.log" >&2 || true
  echo "--- boot SQL file ---" >&2
  sql_file="/tmp/quack-proxy-shard1-${PORT}.sql"
  ls -l "$sql_file" >&2 || true
  cat "$sql_file" >&2 || true
  echo "--- status.json ---" >&2
  cat "$WORK/status.json" >&2 || true
  echo "--- processes ---" >&2
  ps -ef >&2 || true
  exit 1
fi
echo "==> endpoint is listening"

# The health loop probes GET / and expects a 2xx. Check that a REAL Quack
# endpoint satisfies that (a false negative here would restart a perfectly
# healthy shard forever). Also wait for the daemon to publish "healthy".
http_code="$(curl -s -o "$WORK/health.body" -w '%{http_code}' -m 5 "http://127.0.0.1:${PORT}/" || true)"
echo "==> GET / -> HTTP ${http_code} (first $(head -c 60 "$WORK/health.body" | tr -d '\0' | tr '\n' ' ')...)"
if [[ ! "$http_code" =~ ^2 ]]; then
  echo "FAIL: health probe only accepts 2xx but Quack returned ${http_code}" >&2
  echo "      the supervisor would restart this healthy shard forever" >&2
  exit 1
fi
healthy=0
for _ in $(seq 1 90); do
  if grep -q '"status": "healthy"' "$WORK/status.json" 2>/dev/null; then healthy=1; break; fi
  sleep 0.5
done
[[ "$healthy" -eq 1 ]] || {
  echo "FAIL: daemon never reported the shard as healthy" >&2
  cat "$WORK/status.json" 2>/dev/null >&2 || true
  tail -30 "$WORK/daemon.log" >&2
  exit 1
}
echo "==> health loop reports healthy"

# --- 5. client over the Quack protocol ------------------------------------
client_sql() {
  local tok="$1" sql="$2"
  run_with_duck_home "$DUCKDB_BIN" -c "
LOAD quack;
CREATE OR REPLACE SECRET (TYPE QUACK, TOKEN '${tok}');
ATTACH 'quack:127.0.0.1:${PORT}' AS s;
${sql}
" 2>&1
}

echo "==> ATTACH + SELECT with the correct token"
GOOD="$(client_sql "$TOKEN" "SELECT answer FROM s.probe;" || true)"
if ! grep -q '42' <<<"$GOOD"; then
  echo "FAIL: correct token did not return the expected row" >&2
  echo "$GOOD" >&2; exit 1
fi
echo "==> correct token: OK"

echo "==> ATTACH with a WRONG token (must be rejected)"
BAD="$(client_sql "definitely-the-wrong-token" "SELECT answer FROM s.probe;" || true)"
if grep -q '42' <<<"$BAD"; then
  echo "FAIL: an invalid token was accepted — authentication is broken" >&2
  echo "$BAD" >&2; exit 1
fi
if ! grep -qiE 'token|auth|denied|error|fail' <<<"$BAD"; then
  echo "WARN: wrong token produced an unexpected result (inspect manually):" >&2
  echo "$BAD" >&2
fi
echo "==> wrong token: rejected"

# --- 6. optional end-to-end ATTACH/SELECT benchmark -----------------------
if (( BENCH > 0 )); then
  echo "==> benchmark: ${BENCH} SELECT round-trips over the Quack protocol"
  sql=""
  for _ in $(seq 1 "$BENCH"); do sql+="SELECT count(*) FROM s.probe; "; done
  start_ns="$(date +%s%N)"
  client_sql "$TOKEN" "$sql" > "$WORK/bench.out"
  end_ns="$(date +%s%N)"
  elapsed_ms=$(( (end_ns - start_ns) / 1000000 ))
  if (( elapsed_ms > 0 )); then
    echo "==> ${BENCH} queries in ${elapsed_ms} ms (~$(( BENCH * 1000 / elapsed_ms )) qps, single client, includes ATTACH)"
  else
    echo "==> ${BENCH} queries completed in <1 ms"
  fi
  # NOTE: this measures a single client's sequential round-trips against a
  # real Quack endpoint. It is NOT comparable to docs/benchmark.md's HTTP
  # GET / greeting numbers, which bypassed quack-proxy entirely (R-08).
fi

# --- 7. stop + verify cleanup --------------------------------------------
echo "==> stopping daemon"
"$PROXY_BIN" stop -c "$WORK/config.yaml" >/dev/null 2>&1 || true
for _ in $(seq 1 40); do
  if ! kill -0 "$DAEMON_PID" 2>/dev/null; then break; fi
  sleep 0.25
done
closed=0
for _ in $(seq 1 30); do
  if ! port_open; then closed=1; break; fi
  sleep 0.2
done
if [[ "$closed" -ne 1 ]]; then
  echo "FAIL: port ${PORT} still open after stop (orphan shard leaked)" >&2
  tail -20 "$WORK/daemon.log" >&2
  exit 1
fi
echo "==> port released, no orphan listeners"

echo "ALL_QUACK_E2E_CHECKS_PASSED"
