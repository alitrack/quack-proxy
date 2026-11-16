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
# unless --proxy-bin is supplied), and network access on first run to fetch
# the pinned DuckDB CLI + the signed core `quack` extension.
#
# Usage:
#   bash scripts/quack-e2e.sh                       # download pinned DuckDB
#   bash scripts/quack-e2e.sh --benchmark 200      # also run 200 queries
#   bash scripts/quack-e2e.sh --duckdb /opt/duckdb --proxy-bin ./quack-proxy
#   DUCKDB_VERSION_OVERRIDE=1 bash scripts/quack-e2e.sh --duckdb ... # skip hash check
#
# Exit: 0 = all checks passed, non-zero = first failed check (see message).
set -euo pipefail

# DuckDB is PINNED. The historical benchmark used a 1.5.2 nightly and the
# RBAC notes mention 2.0.0 stable; this is the single supported baseline.
DUCKDB_VERSION="v2.0.0"
DUCKDB_SHA256="c6507206baf7793c6013dbd82368469a45f20dc0e17a6f92f204602f652ed2bc"

PORT="${QUACK_E2E_PORT:-19491}"
TOKEN="${QUACK_E2E_TOKEN:-e2e-secret-token}"
BENCH=0
WORK=""
DUCKDB_BIN=""
PROXY_BIN=""
KEEP=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
  sed -n '2,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --duckdb)     DUCKDB_BIN="$2"; shift 2 ;;
    --proxy-bin)  PROXY_BIN="$2"; shift 2 ;;
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

# --- 1. pinned DuckDB CLI -------------------------------------------------
if [[ -z "$DUCKDB_BIN" ]]; then
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
echo "==> duckdb: $("$DUCKDB_BIN" --version)"

# Quack is a SIGNED CORE extension on v2.0.0. `INSTALL quack FROM community`
# (as older docs suggested) returns HTTP 404; install it from the core repo
# so no -unsigned flag is needed. Production deployments are expected to
# pre-install the signed extension (SCOPE.md), so prefer a local copy: the
# extension CDN is an external dependency and can be unavailable (HTTP 502).
if "$DUCKDB_BIN" -c "LOAD quack;" >/dev/null 2>&1; then
  echo "==> signed core extension: already installed (offline)"
else
  echo "==> installing signed core extension: quack"
  # The extension CDN has served HTTP 502 in past runs. Retry INSTALL without
  # ever falling back to -unsigned or a community repository: the signature
  # check is part of the supply-chain guarantee (R-14).
  installed=0
  for attempt in 1 2 3; do
    if "$DUCKDB_BIN" -c "INSTALL quack; LOAD quack;" \
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
"$PROXY_BIN" start -c "$WORK/config.yaml" > "$WORK/daemon.log" 2>&1 &
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
  "$DUCKDB_BIN" -c "
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
