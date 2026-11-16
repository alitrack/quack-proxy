#!/usr/bin/env bash
# Verify the R-23 production hardening path against the REAL HAProxy binary.
#
# r6 found that `haproxy -c -f` says "Configuration file is valid" even for a
# config that runs as root with no chroot/user: HAProxy >= 3.x only complains
# at STARTUP. Documenting the fix was not enough. This script closes the loop:
#
#   * proxy.hardened: true makes gen-proxy EMIT chroot/user/group;
#   * a hardened config without a pre-provisioned account is REJECTED by
#     `haproxy -c -f` (negative control — proves the directive is real, and
#     is exactly why the option must stay opt-in);
#   * an UN-hardened config started as root emits HAProxy's root/chroot
#     warnings (negative control, HAProxy >= 3.x);
#   * the hardened config starts, DROPS privileges to the haproxy account,
#     emits no root warning, and still really forwards traffic.
#
# Must run as root (starting as root is the only way HAProxy can chroot and
# then drop privileges). Uses python3 for the backend stub and the probe, so
# it works on both glibc (Ubuntu CI) and musl (Alpine local) hosts.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROXY_BIN=""
HAPROXY_BIN="haproxy"
WORK=""
FRONT=19620
BACK=19621
PLAIN_FRONT=19622
CHROOT="/var/lib/haproxy"
HP_USER="haproxy"
BODY="quack-hardening-ok"

usage() {
  cat <<'USAGE'
usage: verify-hardening.sh [options]   (must run as root)
  --proxy-bin PATH      prebuilt quack-proxy (default: CGO_ENABLED=0 go build)
  --haproxy-bin PATH    haproxy binary (default: haproxy)
  --work DIR            scratch dir (default: mktemp -d, auto-cleaned)
  --front-port N        hardened frontend (default: 19620)
  --back-port N         backend stub port (default: 19621)
  --plain-front-port N  unhardened frontend for the negative control (19622)
  --chroot DIR          chroot directory (default: /var/lib/haproxy)
  --haproxy-user NAME   account to drop to (default: haproxy)
  -h|--help
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --proxy-bin) PROXY_BIN="$2"; shift 2 ;;
    --haproxy-bin) HAPROXY_BIN="$2"; shift 2 ;;
    --work) WORK="$2"; shift 2 ;;
    --front-port) FRONT="$2"; shift 2 ;;
    --back-port) BACK="$2"; shift 2 ;;
    --plain-front-port) PLAIN_FRONT="$2"; shift 2 ;;
    --chroot) CHROOT="$2"; shift 2 ;;
    --haproxy-user) HP_USER="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "FAIL: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

fail() { echo "FAIL: $*" >&2; exit 1; }
log()  { echo "==> $*"; }

CLEAN_WORK=0
if [[ -z "$WORK" ]]; then WORK="$(mktemp -d)"; CLEAN_WORK=1; else mkdir -p "$WORK"; fi

HPID=""; SPID=""
cleanup() {
  set +e
  [[ -n "$HPID" ]] && kill "$HPID" 2>/dev/null
  [[ -n "$SPID" ]] && kill "$SPID" 2>/dev/null
  [[ -n "$HPID" ]] && wait "$HPID" 2>/dev/null
  [[ -n "$SPID" ]] && wait "$SPID" 2>/dev/null
  # Do NOT delete the provisioned account or chroot directory: on a real host
  # those are persistent operator-managed prerequisites.
  [[ "$CLEAN_WORK" -eq 1 ]] && rm -rf -- "$WORK"
  return 0
}
trap cleanup EXIT

# ---- preflight --------------------------------------------------------------
[[ "$(id -u)" -eq 0 ]] || fail "must run as root (HAProxy must start as root before it can chroot/drop privileges)"
command -v python3 >/dev/null 2>&1 || fail "python3 is required for the stub and probe"
command -v "$HAPROXY_BIN" >/dev/null 2>&1 || [[ -x "$HAPROXY_BIN" ]] || \
  fail "haproxy binary not found: $HAPROXY_BIN"
HP_VERSION="$("$HAPROXY_BIN" -v 2>&1 | head -1)"
log "$HP_VERSION"

if [[ -z "$PROXY_BIN" ]]; then
  command -v go >/dev/null 2>&1 || fail "go not found; pass --proxy-bin"
  # Static: this script also runs inside Alpine/musl containers.
  (cd "$REPO" && CGO_ENABLED=0 go build -o "$WORK/quack-proxy" ./cmd/quack-proxy) || \
    fail "go build quack-proxy"
  PROXY_BIN="$WORK/quack-proxy"
fi
[[ -x "$PROXY_BIN" ]] || fail "proxy binary not executable: $PROXY_BIN"

# ---- generate both artefacts ------------------------------------------------
write_yaml() {
  local out="$1" cfg="$2" port="$3" hardened="$4"
  cat > "$out" <<YAML
global:
  duckdb_bin: duckdb
listener:
  bind_host: 127.0.0.1
  health_path: /
shards:
  - name: only
    database: $WORK/only.db
    port: $BACK
    token: tok-hardening
proxy:
  enabled: true
  output: $cfg
  bind_port: $port
YAML
  if [[ "$hardened" == "yes" ]]; then echo "  hardened: true" >> "$out"; fi
}

write_yaml "$WORK/plain.yaml" "$WORK/plain.cfg" "$PLAIN_FRONT" no
write_yaml "$WORK/hard.yaml"  "$WORK/hard.cfg"  "$FRONT"       yes
"$PROXY_BIN" gen-proxy -c "$WORK/plain.yaml" > "$WORK/gen-plain.log" 2>&1 || {
  cat "$WORK/gen-plain.log" >&2; fail "gen-proxy (plain) failed"; }
"$PROXY_BIN" gen-proxy -c "$WORK/hard.yaml" > "$WORK/gen-hard.log" 2>&1 || {
  cat "$WORK/gen-hard.log" >&2; fail "gen-proxy (hardened) failed"; }

for want in "    chroot $CHROOT" "    user $HP_USER" "    group $HP_USER"; do
  grep -qxF "$want" "$WORK/hard.cfg" || fail "hardened config missing active directive: '$want'"
done
if grep -qE '^    (chroot|user|group) ' "$WORK/plain.cfg"; then
  fail "default config must NOT contain active hardening directives (R-23 opt-in)"
fi
echo "HARDENING_DIRECTIVES_EMITTED=1 (default stays opt-in)"

# ---- provision the host prerequisites ---------------------------------------
log "provisioning $HP_USER account and $CHROOT chroot directory"
if command -v useradd >/dev/null 2>&1; then
  getent group "$HP_USER" >/dev/null 2>&1 || groupadd -r "$HP_USER"
  getent passwd "$HP_USER" >/dev/null 2>&1 || \
    useradd -r -g "$HP_USER" -s /usr/sbin/nologin -d "$CHROOT" "$HP_USER"
else
  # Alpine / BusyBox
  addgroup -S "$HP_USER" >/dev/null 2>&1 || true
  adduser -S -D -H -G "$HP_USER" "$HP_USER" >/dev/null 2>&1 || true
fi
getent passwd "$HP_USER" >/dev/null 2>&1 || fail "could not create account $HP_USER"
HP_UID="$(id -u "$HP_USER")"
[[ "$HP_UID" != "0" ]] || fail "$HP_USER resolved to uid 0"
mkdir -p "$CHROOT"
chown root:root "$CHROOT"
chmod 0755 "$CHROOT"
DIR_META="$(stat -c '%U %G %a' "$CHROOT")"
[[ "$DIR_META" == "root root 755" ]] || fail "chroot dir meta='$DIR_META', want 'root root 755'"
echo "HARDENING_PREREQUISITES_OK=1 (uid=$HP_UID, $CHROOT root:root 0755)"

# ---- negative control A: unprovisioned account MUST be rejected -------------
# This is the reason proxy.hardened is opt-in: injecting these directives onto
# an unprepared host makes HAProxy refuse to start at all.
sed "s/^    user $HP_USER$/    user no_such_user_r23/" "$WORK/hard.cfg" > "$WORK/bad-user.cfg"
if "$HAPROXY_BIN" -c -f "$WORK/bad-user.cfg" > "$WORK/bad-user.log" 2>&1; then
  fail "haproxy accepted a hardened config whose account does not exist — directive is not real"
fi
grep -qi "cannot find user id" "$WORK/bad-user.log" || {
  cat "$WORK/bad-user.log" >&2
  fail "negative control failed for an unexpected reason"; }
echo "HARDENING_UNPROVISIONED_REJECTED=1 (why the option is opt-in)"

# ---- negative control B: unhardened config warns about running as root ------
# HAProxy >= 3.x emits this at STARTUP while `haproxy -c -f` still returns 0.
# Older distro packages (e.g. 2.8) may word it differently or not at all;
# gate on the major version so the gate stays honest rather than being skipped
# silently.
HAS_ROOT_WARNING=0
"$HAPROXY_BIN" -db -f "$WORK/plain.cfg" > "$WORK/plain.log" 2>&1 &
HPID=$!
sleep 1.2
kill "$HPID" 2>/dev/null || true
wait "$HPID" 2>/dev/null || true
HPID=""
if grep -qi "started as root without any" "$WORK/plain.log" &&
   grep -qi "does not make use of" "$WORK/plain.log"; then
  HAS_ROOT_WARNING=1
fi
HP_MAJOR="$(printf '%s' "$HP_VERSION" | sed -n 's/.*version \([0-9][0-9]*\)\..*/\1/p')"
if [[ "$HP_MAJOR" -ge 3 ]] 2>/dev/null; then
  [[ "$HAS_ROOT_WARNING" -eq 1 ]] || {
    cat "$WORK/plain.log" >&2
    fail "HAProxy >= 3 did not warn about the unhardened root config — negative control is blind"; }
  echo "HARDENING_NEGATIVE_CONTROL_OK=1 (plain config warns about root/chroot/user)"
else
  echo "HARDENING_ROOT_WARNING_CONTROL_SKIPPED=1 (HAProxy major=$HP_MAJOR; warning is a >=3.x behaviour)"
fi

# ---- positive: hardened config really drops privileges and forwards ---------
log "starting hardened HAProxy and checking privilege drop + real forwarding"
cat > "$WORK/stub.py" <<'PY'
import http.server, socketserver, sys
body = sys.argv[2].encode()
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass
class S(socketserver.TCPServer):
    # Repeated runs in the same container otherwise hit TIME_WAIT on the
    # listening port; this is a throwaway test stub, not a production server.
    allow_reuse_address = True
with S(("127.0.0.1", int(sys.argv[1])), H) as srv:
    srv.serve_forever()
PY
python3 "$WORK/stub.py" "$BACK" "$BODY" > "$WORK/stub.log" 2>&1 &
SPID=$!
for _ in $(seq 1 30); do
  kill -0 "$SPID" 2>/dev/null || break
  python3 - "$BACK" <<'PY' 2>/dev/null && break
import socket, sys
s = socket.socket(); s.settimeout(0.3)
try:
    s.connect(("127.0.0.1", int(sys.argv[1]))); sys.exit(0)
except OSError:
    sys.exit(1)
finally:
    s.close()
PY
  sleep 0.2
done
kill -0 "$SPID" 2>/dev/null || {
  cat "$WORK/stub.log" >&2
  fail "backend stub failed to start on port $BACK"; }

"$HAPROXY_BIN" -c -f "$WORK/hard.cfg" > "$WORK/hard-check.log" 2>&1 || {
  cat "$WORK/hard-check.log" >&2; fail "haproxy -c rejected the hardened config"; }
"$HAPROXY_BIN" -db -f "$WORK/hard.cfg" > "$WORK/hard.log" 2>&1 &
HPID=$!
sleep 1.5
kill -0 "$HPID" 2>/dev/null || {
  cat "$WORK/hard.log" >&2; fail "hardened HAProxy exited immediately"; }

if grep -qiE "started as root without any|does not make use of" "$WORK/hard.log"; then
  cat "$WORK/hard.log" >&2
  fail "hardened config still emits root/chroot/user warnings"
fi
PROC_UID="$(awk '/^Uid:/{print $2}' "/proc/$HPID/status")"
[[ "$PROC_UID" == "$HP_UID" ]] || \
  fail "HAProxy real uid=$PROC_UID, want $HP_UID ($HP_USER); it did not drop root"
echo "HARDENING_DROPPED_ROOT=1 (uid=$PROC_UID, no root warning)"

GOT=""
for _ in $(seq 1 40); do
  GOT="$(python3 - "$FRONT" <<'PY' 2>/dev/null
import sys, urllib.request
try:
    with urllib.request.urlopen("http://127.0.0.1:%s/" % sys.argv[1], timeout=2) as r:
        print(r.read().decode("utf-8", "replace").strip())
except Exception:
    sys.exit(1)
PY
)" || true
  [[ "$GOT" == "$BODY" ]] && break
  sleep 0.25
done
[[ "$GOT" == "$BODY" ]] || {
  cat "$WORK/hard.log" >&2
  fail "hardened HAProxy did not forward traffic (body='$GOT', want '$BODY')"; }
echo "HARDENING_FORWARD_OK=1 (frontend $FRONT -> backend $BACK)"

kill "$HPID" "$SPID" 2>/dev/null || true
wait "$HPID" 2>/dev/null || true
wait "$SPID" 2>/dev/null || true
HPID=""; SPID=""

echo "HARDENING_VERIFY_PASSED"
