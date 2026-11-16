#!/usr/bin/env bash
# Reproduce the real blast radius of R-01 (the daemon is a single point of
# failure) on a POSIX host: start a daemon with a real child process group,
# SIGKILL the daemon, then observe orphans, port occupancy, stale pid/status
# files and what a naive restart does.
#
# This is a DRILL, not a pass/fail test: the orphaned shard is the EXPECTED
# and documented behaviour. The point is to have measured evidence instead of
# the systemd unit's own comment claiming it happens.
#
# Requirements: Linux, bash, go (to build the daemon + a tiny HTTP stub).
# Usage:
#   bash scripts/daemon-crash-drill.sh
#   bash scripts/daemon-crash-drill.sh --port 19888 --work /tmp/drill --keep
#
# Exit: 0 = drill completed and environment cleaned up.
set -euo pipefail

PORT="${QUACK_DRILL_PORT:-19771}"
WORK=""
KEEP=0
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --port)   PORT="$2"; shift 2 ;;
    --work)   WORK="$2"; KEEP=1; shift 2 ;;
    --keep)   KEEP=1; shift ;;
    -h|--help) sed -n '2,18p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "$WORK" ]]; then WORK="$(mktemp -d /tmp/quack-drill.XXXXXX)"; fi
mkdir -p "$WORK"

QP="$WORK/quack-proxy"
STUB="$WORK/quack-stub"
DAEMON_PID=""
RESTART_PID=""
SHARD_PID=""
SECOND_SHARD_PID=""

port_open() { (exec 3<>"/dev/tcp/127.0.0.1/${PORT}") 2>/dev/null && { exec 3<&-; exec 3>&-; }; }
json_str() { [[ -f "$1" ]] || return 0; tr -d ' \n' < "$1" | grep -o "\"$2\":\"[^\"]*\"" | head -1 | cut -d'"' -f4 || true; }
json_num() { [[ -f "$1" ]] || return 0; tr -d ' \n' < "$1" | grep -o "\"$2\":[0-9]*" | head -1 | cut -d: -f2 || true; }

cleanup() {
  local rc=$?
  [[ -n "$RESTART_PID" ]] && kill -TERM "$RESTART_PID" 2>/dev/null || true
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null || true
  # The child is a process-group leader, so a negative PID kills the whole
  # group (bash wrapper + engine). Do this for BOTH generations.
  for pid in "$SHARD_PID" "$SECOND_SHARD_PID"; do
    if [[ -n "$pid" ]]; then kill -KILL "-$pid" 2>/dev/null || true; fi
  done
  if [[ "$KEEP" -eq 0 ]]; then rm -rf -- "$WORK"; fi
  exit "$rc"
}
trap cleanup EXIT

echo "==> drill work dir: $WORK"

echo "==> building daemon and a minimal HTTP stub"
(cd "$REPO_ROOT" && go build -o "$QP" ./cmd/quack-proxy)
cat > "$WORK/stub.go" <<'GO'
package main

import (
	"net/http"
	"os"
)

func main() {
	// Each daemon generation gets a distinct body, so the drill can tell
	// WHICH process is actually answering the new daemon's health probes.
	body := os.Getenv("QUACK_STUB_GREETING")
	if body == "" {
		body = "quack-crash-drill-stub"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	_ = http.ListenAndServe("127.0.0.1:"+os.Args[1], nil)
}
GO
go build -o "$STUB" "$WORK/stub.go"
cat > "$WORK/stub.sh" <<EOF
#!/bin/sh
port="\$(basename "\$1" | tr -cd '0-9')"
exec "$STUB" "\$port"
EOF
chmod +x "$WORK/stub.sh"

cat > "$WORK/config.yaml" <<YAML
global:
  log_level: info
  duckdb_bin: ${WORK}/stub.sh
  pid_file: ${WORK}/daemon.pid
  status_file: ${WORK}/status.json
listener:
  bind_host: 127.0.0.1
  health_path: /
  health_interval: 1s
shards:
  - name: crashy
    database: ${WORK}/shard-${PORT}.db
    port: ${PORT}
    token: drill-token
YAML

echo "==> starting daemon (port ${PORT})"
export QUACK_STUB_GREETING="drill-OLD"
"$QP" start -c "$WORK/config.yaml" > "$WORK/daemon.log" 2>&1 &
DAEMON_PID=$!
ready=0
for _ in $(seq 1 120); do
  if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
    echo "FAIL: daemon exited early" >&2; tail -30 "$WORK/daemon.log" >&2; exit 1
  fi
  if port_open; then ready=1; break; fi
  sleep 0.2
done
if [[ "$ready" -ne 1 ]]; then echo "FAIL: port never opened" >&2; exit 1; fi

SHARD_PID="$(json_num "$WORK/status.json" pid)"
PID_FROM_FILE="$(cat "$WORK/daemon.pid" 2>/dev/null || true)"
echo "==> daemon pid: ${PID_FROM_FILE:-unknown}; shard pid: ${SHARD_PID:-unknown}"

echo "==> SIGKILL the daemon (simulating an OOM kill / host crash)"
kill -9 "$DAEMON_PID"
wait "$DAEMON_PID" 2>/dev/null || true
sleep 1

alive=0;   if kill -0 "$SHARD_PID" 2>/dev/null; then alive=1; fi
open=0;    if port_open; then open=1; fi
stalepid=0;   if [[ -f "$WORK/daemon.pid" ]]; then stalepid=1; fi
stalestatus=0; if [[ -f "$WORK/status.json" ]]; then stalestatus=1; fi

echo "==> observations after daemon death"
echo "ORPHAN_SHARD_ALIVE=$alive"
echo "ORPHAN_PORT_OPEN=$open"
echo "STALE_PID_FILE=$stalepid"
echo "STALE_STATUS_FILE=$stalestatus"
echo "STALE_STATUS_VALUE=$(json_str "$WORK/status.json" status)"

echo "==> naive restart while the orphan still owns the port"
export QUACK_STUB_GREETING="drill-NEW"
"$QP" start -c "$WORK/config.yaml" > "$WORK/restart.log" 2>&1 &
RESTART_PID=$!
# HealthLoop has a hard-coded 15s initial grace, then ticks every 1s.
for _ in $(seq 1 70); do
  if ! kill -0 "$RESTART_PID" 2>/dev/null; then break; fi
  sleep 0.3
done
NEW_SHARD_PID="$(json_num "$WORK/status.json" pid)"
SECOND_SHARD_PID="$NEW_SHARD_PID"
STATUS_AFTER="$(json_str "$WORK/status.json" status)"
BODY_AFTER="$(curl -s --max-time 2 "http://127.0.0.1:${PORT}/" || true)"
NEW_SHARD_ALIVE=0
if [[ -n "$NEW_SHARD_PID" ]] && kill -0 "$NEW_SHARD_PID" 2>/dev/null; then NEW_SHARD_ALIVE=1; fi
echo "RESTART_DAEMON_ALIVE=$(kill -0 "$RESTART_PID" 2>/dev/null && echo 1 || echo 0)"
RESTART_REFUSED=0
if grep -q "already occupied" "$WORK/restart.log" 2>/dev/null; then RESTART_REFUSED=1; fi
echo "RESTART_REFUSED_ON_ORPHAN_PORT=$RESTART_REFUSED  (R-22: must be 1)"
echo "RESTART_SHARD_PID=$NEW_SHARD_PID"
echo "RESTART_SHARD_ALIVE=$NEW_SHARD_ALIVE"
echo "RESTART_REPORTED_STATUS=$STATUS_AFTER"
echo "HTTP_BODY_AFTER_RESTART=$BODY_AFTER"
# A healthy status is meaningless if the response comes from the ORPHANED
# generation. The stub body identifies which process actually answered.
if [[ "$STATUS_AFTER" == "healthy" && "$BODY_AFTER" == "drill-OLD" ]]; then
  echo "FALSE_HEALTHY=1  (health probe is served by the ORPHAN, not the new shard)"
else
  echo "FALSE_HEALTHY=0"
fi

echo "==> removing the orphan group: who was really holding the port?"
if [[ -n "$SHARD_PID" ]]; then kill -KILL "-$SHARD_PID" 2>/dev/null || true; fi
sleep 1
PORT_AFTER_ORPHAN_KILL=0
if port_open; then PORT_AFTER_ORPHAN_KILL=1; fi
echo "PORT_AFTER_ORPHAN_KILL=$PORT_AFTER_ORPHAN_KILL"
echo "STATUS_AFTER_ORPHAN_KILL=$(json_str "$WORK/status.json" status)"

echo "==> daemon log tail"
tail -8 "$WORK/daemon.log" || true
echo "==> restart log tail"
tail -8 "$WORK/restart.log" || true

# This drill is also a regression gate: R-22 made the daemon refuse to boot
# on a port still owned by an orphan. If that guard disappears, the drill
# must fail instead of just printing observations.
if [[ "$RESTART_REFUSED" -ne 1 ]]; then
  echo "DRILL_FAIL: a restart was NOT refused on the orphaned port (R-22 regression)" >&2
  exit 1
fi
if [[ "$alive" -ne 1 || "$open" -ne 1 ]]; then
  echo "DRILL_FAIL: the orphan scenario was not reproduced (shard/port already gone)" >&2
  exit 1
fi

echo "DAEMON_CRASH_DRILL_DONE"
