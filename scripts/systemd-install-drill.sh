#!/usr/bin/env bash
# Real, non-root systemd install drill for quack-proxy (SCOPE.md DoD #1, R-06).
#
# `systemd-analyze verify` only parses a unit. This script INSTALLS the shipped
# unit into /etc/systemd/system, starts it on a REAL systemd PID 1, and proves:
#
#   * a non-root service account can start (RuntimeDirectory creates
#     /run/quack-proxy, which an unprivileged process cannot create itself);
#   * the daemon runs as that account, with 0600 pid/status files;
#   * the health loop reaches healthy and the shard answers real HTTP;
#   * `systemctl stop` tears down the whole process group: port released,
#     no orphan stub, pid/status removed, runtime directory gone.
#
# NEGATIVE CONTROL (R-06): before installing the unit, start the very same
# binary AS the service account with /run/quack-proxy absent. It MUST fail.
# Without that control a drill that never exercised the privilege boundary
# could still pass, e.g. if something else had already created the directory.
#
# Requirements: root, a booted systemd (running|degraded), runuser, python3
# (or curl), and either Go or prebuilt --proxy-bin / --stub-bin.
#
# Local (Docker, WSL2 cgroup v2):
#   docker run -d --name qp-sd --privileged --cgroupns=host \
#     --tmpfs /run --tmpfs /run/lock \
#     -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
#     -v "$REPO:/src:ro" -v "$WORK:/work" \
#     --entrypoint /lib/systemd/systemd jrei/systemd-ubuntu:24.04 \
#     --system --unit=basic.target
#   docker exec qp-sd bash /src/scripts/systemd-install-drill.sh \
#     --proxy-bin /work/quack-proxy --stub-bin /work/drill-stub \
#     --unit-src /src/deploy/systemd/quack-proxy.service
#
# CI (ubuntu-latest already boots systemd): sudo bash scripts/systemd-install-drill.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROXY_BIN=""
STUB_BIN=""
UNIT_SRC="$REPO/deploy/systemd/quack-proxy.service"
UNIT_NAME="quack-proxy"
PORT="19711"
WORK=""
GREETING="systemd-drill-greeting"
TOKEN="systemd-drill-secret-token"

CONF="/etc/quack-proxy/quack-proxy.yaml"
DEST_BIN="/usr/local/bin/quack-proxy"
DEST_STUB="/usr/local/bin/quack-proxy-drill-stub"
RUNTIME_DIR="/run/quack-proxy"

usage() {
  cat <<'USAGE'
usage: systemd-install-drill.sh [options]   (must run as root)
  --proxy-bin PATH      prebuilt quack-proxy binary (default: go build)
  --stub-bin PATH       prebuilt scripts/drill-stub.go binary (default: go build)
  --unit-src PATH       unit file to install (default: deploy/systemd/quack-proxy.service)
  --unit-name NAME      installed unit name (default: quack-proxy)
  --port N              shard port (default: 19711)
  --work DIR            scratch dir (default: mktemp -d, auto-cleaned)
  -h|--help
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --proxy-bin) PROXY_BIN="$2"; shift 2 ;;
    --stub-bin) STUB_BIN="$2"; shift 2 ;;
    --unit-src) UNIT_SRC="$2"; shift 2 ;;
    --unit-name) UNIT_NAME="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --work) WORK="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "FAIL: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

fail() { echo "FAIL: $*" >&2; exit 1; }
log()  { echo "==> $*"; }

# Only auto-remove a work dir this script created; a caller-supplied --work
# directory must not be deleted underneath them.
CLEAN_WORK=0
if [[ -z "$WORK" ]]; then
  WORK="$(mktemp -d)"
  CLEAN_WORK=1
else
  mkdir -p "$WORK"
fi

cleanup() {
  set +e
  systemctl stop "$UNIT_NAME.service" >/dev/null 2>&1
  systemctl disable "$UNIT_NAME.service" >/dev/null 2>&1
  systemctl reset-failed "$UNIT_NAME.service" >/dev/null 2>&1
  rm -f "/etc/systemd/system/$UNIT_NAME.service"
  systemctl daemon-reload >/dev/null 2>&1
  rm -f "$CONF" "$DEST_BIN" "$DEST_STUB"
  rmdir /etc/quack-proxy >/dev/null 2>&1
  rm -f "$RUNTIME_DIR/quack-proxy.pid" "$RUNTIME_DIR/status.json"
  rmdir "$RUNTIME_DIR" >/dev/null 2>&1
  if [[ "$CLEAN_WORK" -eq 1 ]]; then rm -rf -- "$WORK"; fi
  return 0
}
trap cleanup EXIT

# ---- preflight --------------------------------------------------------------
[[ "$(id -u)" -eq 0 ]] || fail "must run as root (the SERVICE still runs unprivileged)"
command -v systemctl >/dev/null 2>&1 || fail "systemctl not found"
command -v runuser  >/dev/null 2>&1 || fail "runuser not found (util-linux)"
command -v ps       >/dev/null 2>&1 || fail "ps not found (procps)"
[[ -f "$UNIT_SRC" ]] || fail "unit not found: $UNIT_SRC"

STATE="$(systemctl is-system-running 2>/dev/null || true)"
case "$STATE" in
  running|degraded) log "systemd is-system-running=$STATE ($(systemctl show -p Version --value 2>/dev/null))" ;;
  *) fail "systemd is not running as PID 1 (is-system-running=$STATE)" ;;
esac

if command -v python3 >/dev/null 2>&1; then HTTP=python3; else
  command -v curl >/dev/null 2>&1 || fail "need python3 or curl for HTTP probes"
  HTTP=curl
fi

# ---- build / locate binaries ------------------------------------------------
if [[ -z "$PROXY_BIN" ]]; then
  command -v go >/dev/null 2>&1 || fail "go not found; pass --proxy-bin"
  (cd "$REPO" && go build -o "$WORK/quack-proxy" ./cmd/quack-proxy) || fail "go build quack-proxy"
  PROXY_BIN="$WORK/quack-proxy"
fi
if [[ -z "$STUB_BIN" ]]; then
  command -v go >/dev/null 2>&1 || fail "go not found; pass --stub-bin"
  (cd "$REPO" && go build -o "$WORK/drill-stub" ./scripts/drill-stub.go) || fail "go build drill-stub"
  STUB_BIN="$WORK/drill-stub"
fi
[[ -x "$PROXY_BIN" ]] || fail "proxy binary not executable: $PROXY_BIN"
[[ -x "$STUB_BIN"  ]] || fail "stub binary not executable: $STUB_BIN"

# ---- install the artefacts exactly as the README says -----------------------
log "installing binaries, config and the shipped unit"
getent group  quack-proxy >/dev/null 2>&1 || groupadd -r quack-proxy
getent passwd quack-proxy >/dev/null 2>&1 || \
  useradd -r -g quack-proxy -s /usr/sbin/nologin -d /var/lib/quack-proxy quack-proxy
install -D -m 0755 "$PROXY_BIN" "$DEST_BIN"
install -D -m 0755 "$STUB_BIN"  "$DEST_STUB"
install -d -m 0755 /etc/quack-proxy
install -d -m 0755 /var/lib/quack-proxy
chown quack-proxy:quack-proxy /var/lib/quack-proxy
cat > "$CONF" <<YAML
global:
  log_level: info
  duckdb_bin: $DEST_STUB
  # PRD §4.2 (r14): this drill verifies --show-tokens redaction, which needs
  # a token IN status.json. Opt in explicitly. The DEFAULT (off) is verified
  # by the negative control below and by the required Go suite.
  persist_tokens: true
listener:
  bind_host: 127.0.0.1
  health_path: /
  health_interval: 500ms
  health_expect: $GREETING
shards:
  - name: drill
    database: /var/lib/quack-proxy/drill.db
    port: $PORT
    token: $TOKEN
YAML
chmod 0644 "$CONF"

# ---- NEGATIVE CONTROL: without RuntimeDirectory the account cannot start ----
# This is the actual R-06 regression: pid/status default to /run/quack-proxy,
# which a non-root process cannot create under root-owned /run.
log "negative control: start as quack-proxy with no runtime directory"
rm -f "$RUNTIME_DIR/quack-proxy.pid" "$RUNTIME_DIR/status.json"
rmdir "$RUNTIME_DIR" >/dev/null 2>&1 || true
[[ ! -d "$RUNTIME_DIR" ]] || fail "could not remove $RUNTIME_DIR before negative control"
NEG_RC=0
runuser -u quack-proxy -- "$DEST_BIN" start -c "$CONF" > "$WORK/negative.log" 2>&1 || NEG_RC=$?
if [[ "$NEG_RC" -eq 0 ]]; then
  cat "$WORK/negative.log" >&2
  fail "negative control: unprivileged start SUCCEEDED without RuntimeDirectory — the drill is not exercising the privilege boundary"
fi
grep -qiE 'permission denied|failed to write pid|mkdir|read-only' "$WORK/negative.log" || {
  cat "$WORK/negative.log" >&2
  fail "negative control failed for an unexpected reason (expected the pid-directory permission error)"
}
[[ ! -d "$RUNTIME_DIR" ]] || fail "negative control left a runtime directory behind"
echo "SYSTEMD_DRILL_NEGATIVE_CONTROL_OK=1 (unprivileged start refused: $(head -1 "$WORK/negative.log"))"

# ---- r14 negative control: no cleartext token on disk by default ------------
# PRD §4.2: with global.persist_tokens false (the default), a config that
# omits the shard token must be REFUSED by the shipped binary. Otherwise the
# daemon would generate a credential it can no longer distribute, leaving a
# shard nobody can authenticate to. This exercises config.Load in the real
# CLI, not a Go unit test; the main drill config above opts in explicitly.
log "negative control: default config with an auto-generated token is refused"
cat > "$WORK/nopersist.yaml" <<YAML
global:
  duckdb_bin: $DEST_STUB
  pid_file: $RUNTIME_DIR/quack-proxy.pid
  status_file: $RUNTIME_DIR/status.json
listener:
  bind_host: 127.0.0.1
shards:
  - name: drill
    database: $WORK/no-token.db
    port: $((PORT + 1))
YAML
NP_RC=0
"$DEST_BIN" start -c "$WORK/nopersist.yaml" > "$WORK/nopersist.log" 2>&1 || NP_RC=$?
if [[ "$NP_RC" -eq 0 ]]; then
  cat "$WORK/nopersist.log" >&2
  fail "default config with no explicit token was accepted (PRD §4.2 regression)"
fi
grep -q "persist_tokens" "$WORK/nopersist.log" || {
  cat "$WORK/nopersist.log" >&2
  fail "default-token rejection failed for an unexpected reason"
}
echo "TOKEN_PERSIST_DEFAULT_REJECTED=1 (real CLI refuses an undistributable token)"

# ---- positive: real systemd start -------------------------------------------
log "installing $UNIT_NAME.service and starting it via systemctl"
install -D -m 0644 "$UNIT_SRC" "/etc/systemd/system/$UNIT_NAME.service"
systemctl daemon-reload
systemctl reset-failed "$UNIT_NAME.service" >/dev/null 2>&1 || true
systemctl enable "$UNIT_NAME.service" > "$WORK/enable.log" 2>&1 || {
  cat "$WORK/enable.log" >&2; fail "systemctl enable failed"; }
systemctl start "$UNIT_NAME.service"

ACTIVE=""
for _ in $(seq 1 80); do
  ACTIVE="$(systemctl is-active "$UNIT_NAME.service" 2>/dev/null || true)"
  [[ "$ACTIVE" == "active" ]] && break
  [[ "$ACTIVE" == "failed" ]] && break
  sleep 0.25
done
if [[ "$ACTIVE" != "active" ]]; then
  systemctl status "$UNIT_NAME.service" --no-pager -l >&2 || true
  journalctl -u "$UNIT_NAME.service" --no-pager -n 60 >&2 || true
  fail "service did not become active (is-active=$ACTIVE)"
fi
echo "SYSTEMD_SERVICE_ACTIVE=1"

http_body() {
  if [[ "$HTTP" == python3 ]]; then
    python3 - "$PORT" <<'PY' 2>/dev/null
import sys, urllib.request
try:
    with urllib.request.urlopen("http://127.0.0.1:%s/" % sys.argv[1], timeout=2) as r:
        print(r.read().decode("utf-8", "replace").strip())
except Exception:
    sys.exit(1)
PY
  else
    curl -sS -m 2 "http://127.0.0.1:$PORT/"
  fi
}

BODY=""
for _ in $(seq 1 120); do
  BODY="$(http_body || true)"
  [[ "$BODY" == "$GREETING" ]] && break
  sleep 0.25
done
if [[ "$BODY" != "$GREETING" ]]; then
  journalctl -u "$UNIT_NAME.service" --no-pager -n 80 >&2 || true
  fail "shard never served the greeting (last body='$BODY')"
fi
echo "SYSTEMD_SHARD_HTTP_OK=1 (127.0.0.1:$PORT -> $BODY)"

# Wait for the health loop to promote the shard from starting to healthy.
HEALTHY=0
for _ in $(seq 1 120); do
  if "$DEST_BIN" status --json -c "$CONF" > "$WORK/status.json" 2>"$WORK/status.err"; then
    if grep -q '"healthy"' "$WORK/status.json"; then HEALTHY=1; break; fi
  fi
  sleep 0.25
done
[[ "$HEALTHY" -eq 1 ]] || {
  cat "$WORK/status.err" "$WORK/status.json" >&2 || true
  fail "shard never reached healthy in status.json"; }
echo "SYSTEMD_STATUS_HEALTHY=1"

# ---- assertions: privileges, files, redaction -------------------------------
SVC_USER="$(systemctl show -p User --value "$UNIT_NAME.service")"
[[ "$SVC_USER" == "quack-proxy" ]] || fail "unit User=$SVC_USER, want quack-proxy"
MAIN_PID="$(systemctl show -p MainPID --value "$UNIT_NAME.service")"
[[ "$MAIN_PID" =~ ^[0-9]+$ ]] && [[ "$MAIN_PID" -gt 1 ]] || fail "bad MainPID=$MAIN_PID"
PROC_USER="$(ps -o user= -p "$MAIN_PID" | tr -d ' ')"
[[ "$PROC_USER" == "quack-proxy" ]] || fail "daemon runs as '$PROC_USER', want quack-proxy"
echo "SYSTEMD_SERVICE_NONROOT=1 (MainPID=$MAIN_PID user=$PROC_USER)"

[[ -d "$RUNTIME_DIR" ]] || fail "RuntimeDirectory $RUNTIME_DIR was not created"
DIR_META="$(stat -c '%U %G %a' "$RUNTIME_DIR")"
[[ "$DIR_META" == "quack-proxy quack-proxy 750" ]] || \
  fail "RuntimeDirectory meta='$DIR_META', want 'quack-proxy quack-proxy 750'"
PID_META="$(stat -c '%U %a' "$RUNTIME_DIR/quack-proxy.pid")"
[[ "$PID_META" == "quack-proxy 600" ]] || fail "pid file meta='$PID_META', want 'quack-proxy 600'"
STATUS_MODE="$(stat -c '%a' "$RUNTIME_DIR/status.json")"
[[ "$STATUS_MODE" == "600" ]] || fail "status file mode=$STATUS_MODE, want 600"
echo "SYSTEMD_RUNTIME_DIR_OK=1 ($DIR_META; pid/status 0600)"

"$DEST_BIN" status --json -c "$CONF" > "$WORK/redacted.json"
if grep -q "$TOKEN" "$WORK/redacted.json"; then fail "status --json leaked the token"; fi
"$DEST_BIN" status --json --show-tokens -c "$CONF" > "$WORK/shown.json"
grep -q "$TOKEN" "$WORK/shown.json" || fail "--show-tokens did not reveal the token (redaction control is meaningless)"
echo "SYSTEMD_TOKEN_REDACTION_OK=1"

# ---- stop: full teardown, no orphans ----------------------------------------
log "stopping service and checking teardown"
systemctl stop "$UNIT_NAME.service"
for _ in $(seq 1 80); do
  ST="$(systemctl is-active "$UNIT_NAME.service" 2>/dev/null || true)"
  [[ "$ST" != "active" ]] && break
  sleep 0.25
done
[[ "$ST" != "active" ]] || fail "service still active after systemctl stop"

PORT_FREE=0
for _ in $(seq 1 40); do
  # Python exits 0 only when the connect FAILS (i.e. the port is free).
  # R-21: allow a short drain window — a shard group can be mid-exit when
  # systemctl stop returns, and that is not the same as an orphan.
  if python3 - "$PORT" <<'PY' 2>/dev/null
import socket, sys
s = socket.socket()
s.settimeout(0.4)
try:
    s.connect(("127.0.0.1", int(sys.argv[1])))
except OSError:
    sys.exit(0)
finally:
    s.close()
sys.exit(1)
PY
  then PORT_FREE=1; break; fi
  sleep 0.2
done
[[ "$PORT_FREE" -eq 1 ]] || fail "port $PORT still listening after stop (orphan shard)"

if command -v pgrep >/dev/null 2>&1; then
  ORPHANS="$(pgrep -f 'quack-proxy-drill-stub' || true)"
else
  ORPHANS="$(ps -eo args= | grep '[q]uack-proxy-drill-stub' || true)"
fi
[[ -z "$ORPHANS" ]] || fail "orphan stub processes survived stop: $ORPHANS"

[[ ! -e "$RUNTIME_DIR/quack-proxy.pid" ]] || fail "pid file survived stop"
[[ ! -e "$RUNTIME_DIR/status.json"   ]] || fail "status file survived stop"
[[ ! -d "$RUNTIME_DIR" ]] || fail "RuntimeDirectory survived stop (systemd should remove it)"
echo "SYSTEMD_STOP_CLEAN=1 (port released, no orphan, pid/status/runtime dir gone)"

echo "SYSTEMD_INSTALL_DRILL_PASSED"
