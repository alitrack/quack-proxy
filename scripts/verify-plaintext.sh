#!/usr/bin/env bash
# LAN cleartext audit for quack-proxy (R-05).
#
# This is an AUDIT, not a fix. It proves, with a real AF_PACKET capture, that
# when a shard is exposed on a LAN interface (listener.bind_host: 0.0.0.0)
# the shard token travels in cleartext: quack-proxy hardcodes disable_ssl=true
# and has no TLS termination. A TLS control channel on the same L2 segment
# carries a second canary that must NOT appear in the capture - that negative
# control proves the sniffer really parses wire payloads and is not just a
# grep over a log file. A third check uses the real CLI to confirm that
# proxy.ssl.enabled=true is rejected at config load ("not implemented").
#
# Topology (all local, no external downloads except the container image):
#   * one Docker user-defined bridge = a stand-in for a shared LAN segment;
#   * server container: the REAL quack-proxy daemon starts a shard backed by
#     scripts/drill-stub.go (DRILL_STUB_BIND=0.0.0.0), plus a TLS control
#     server. Runtime state lives in the container's own /tmp, not on the
#     bind mount, so 0600 checks are meaningful.
#   * sniffer container: a SEPARATE container attached with
#     --network container:<server>, i.e. an on-path observer on that segment.
#     It never reads the server's memory or logs - only raw Ethernet frames.
#   * client container: another container on the same bridge. It reaches the
#     server by its bridge IP/name, so traffic really crosses the segment.
#
# Scope limits (do not overstate): the shard endpoint is the drill stub, not
# the real DuckDB/Quack engine (that needs external downloads and is covered
# by scripts/quack-e2e.sh). The TLS control is a Python HTTPS server standing
# in for the "terminate TLS in an external proxy" mitigation. The audit proves
# the TRANSPORT property; it does not exercise the Quack protocol.
#
# When TLS termination is actually added, this job will (correctly) start
# failing: update the assertions and docs together instead of deleting it.
#
# Usage:
#   bash scripts/verify-plaintext.sh                      # builds with go
#   bash scripts/verify-plaintext.sh --proxy-bin /tmp/quack-proxy \
#                                    --stub-bin  /tmp/drill-stub
#   bash scripts/verify-plaintext.sh --image python:3.12-slim --work /tmp/pt
#
# Requirements: docker, openssl (host), python3 inside the image. If the two
# binaries are not supplied, `go` is needed to build them (CGO_ENABLED=0).
#
# Exit: 0 = every assertion held, non-zero = first failed assertion.
set -euo pipefail

HTTP_PORT="${PLAINTEXT_HTTP_PORT:-19741}"
TLS_PORT="${PLAINTEXT_TLS_PORT:-19742}"
TOKEN="qp-lan-cleartext-token"
TLS_TOKEN="qp-tls-control-token"
# A small Python 3 image: python3 + ssl + http.server, no extra packages.
# 3.11-slim is the tag verified locally; override with --image or the
# PLAINTEXT_IMAGE env var if your environment caches a different one.
IMAGE="${PLAINTEXT_IMAGE:-python:3.11-slim}"
PROXY_BIN=""
STUB_BIN=""
WORK=""

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
  sed -n '2,42p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --proxy-bin) PROXY_BIN="$2"; shift 2 ;;
    --stub-bin)  STUB_BIN="$2"; shift 2 ;;
    --image)     IMAGE="$2"; shift 2 ;;
    --work)      WORK="$2"; shift 2 ;;
    -h|--help)   usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 1 ;;
  esac
done

fail() { echo "FAIL: $*" >&2; exit 1; }
log()  { echo "==> $*"; }

CLEAN_WORK=0
if [[ -z "$WORK" ]]; then WORK="$(mktemp -d)"; CLEAN_WORK=1; else mkdir -p "$WORK"; fi
# Git Bash needs a Windows-style path for -v; Linux CI uses the path as-is.
if command -v cygpath >/dev/null 2>&1; then
  VOL="$(cygpath -w "$WORK" | tr '\\' '/')"
else
  VOL="$WORK"
fi

TAG="qp-plain-$$"
NET="${TAG}-net"
SRV="${TAG}-srv"
SNIFF="${TAG}-sniff"

cleanup() {
  local rc=$?
  set +e
  docker rm -f "$SNIFF" "$SRV" >/dev/null 2>&1
  docker network rm "$NET" >/dev/null 2>&1
  if [[ "$CLEAN_WORK" -eq 1 ]]; then rm -rf -- "$WORK"; fi
  return "$rc"
}
trap cleanup EXIT

# ---- preflight --------------------------------------------------------------
command -v docker >/dev/null 2>&1 || fail "docker is required"
docker info >/dev/null 2>&1 || fail "docker daemon is not reachable"
command -v openssl >/dev/null 2>&1 || fail "openssl is required to mint the TLS control cert"
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  log "pulling ${IMAGE} (3 attempts)"
  pulled=0
  for _ in 1 2 3; do
    if docker pull "$IMAGE" >/dev/null 2>&1; then pulled=1; break; fi
    sleep 2
  done
  [[ "$pulled" -eq 1 ]] || \
    fail "cannot pull ${IMAGE}; pass --image pointing at a locally cached Python 3 image"
fi

if [[ -z "$PROXY_BIN" || -z "$STUB_BIN" ]]; then
  command -v go >/dev/null 2>&1 || fail "go not found; pass --proxy-bin and --stub-bin"
  [[ -z "$PROXY_BIN" ]] && PROXY_BIN="$WORK/quack-proxy"
  [[ -z "$STUB_BIN" ]] && STUB_BIN="$WORK/drill-stub"
  (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$PROXY_BIN" ./cmd/quack-proxy) || fail "go build quack-proxy"
  (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$STUB_BIN" ./scripts/drill-stub.go) || fail "go build drill-stub"
fi
# Only check existence here: on a Windows bind mount Git Bash cannot see the
# execute bit even though Docker exposes the file as 0755 inside the container.
[[ -f "$PROXY_BIN" ]] || fail "proxy binary not found: $PROXY_BIN"
[[ -f "$STUB_BIN" ]] || fail "stub binary not found: $STUB_BIN"
cp "$PROXY_BIN" "$WORK/quack-proxy"
cp "$STUB_BIN" "$WORK/drill-stub"
chmod +x "$WORK/quack-proxy" "$WORK/drill-stub"

openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$WORK/key.pem" -out "$WORK/cert.pem" -days 1 \
  -subj "/CN=qp-plaintext-audit" >/dev/null 2>&1 || fail "openssl cert generation"

# ---- helpers (written into the shared work dir) -----------------------------
cat > "$WORK/sniff.py" <<'PY'
import os, socket, struct, sys

outdir, portlist, stop = sys.argv[1], sys.argv[2], sys.argv[3]
ports = {int(x) for x in portlist.split(",")}
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0003))
s.bind(("eth0", 0))
s.settimeout(0.3)
files, counts = {}, {}
while not os.path.exists(stop):
    try:
        pkt = s.recv(65535)
    except socket.timeout:
        continue
    except OSError as e:
        print("SNIFF_ERR", e, flush=True)
        break
    if len(pkt) < 34 or struct.unpack("!H", pkt[12:14])[0] != 0x0800:
        continue                                  # IPv4 only
    ihl = (pkt[14] & 0x0F) * 4
    if len(pkt) < 14 + ihl + 20 or pkt[14 + 9] != 6:
        continue                                  # TCP only
    total = struct.unpack("!H", pkt[16:18])[0]
    tcp = 14 + ihl
    sp, dp = struct.unpack("!HH", pkt[tcp:tcp + 4])
    off = (pkt[tcp + 12] >> 4) * 4
    payload = pkt[tcp + off:14 + total]
    if not payload:
        continue
    for p in (sp, dp):
        if p not in ports:
            continue
        if p not in files:
            files[p] = open(os.path.join(outdir, "port-%d.bin" % p), "ab", buffering=0)
        files[p].write(payload)
        counts[p] = counts.get(p, 0) + len(payload)
for f in files.values():
    f.close()
with open(os.path.join(outdir, "capture.summary"), "w") as out:
    for p in sorted(ports):
        out.write("%d %d\n" % (p, counts.get(p, 0)))
print("SNIFF_DONE", counts, flush=True)
PY

cat > "$WORK/tls_srv.py" <<'PY'
import http.server, ssl, sys

port, cert, key = int(sys.argv[1]), sys.argv[2], sys.argv[3]


class H(http.server.BaseHTTPRequestHandler):
    def _reply(self):
        n = int(self.headers.get("Content-Length", "0"))
        if n:
            self.rfile.read(n)
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"tls-control-ok")

    do_GET = _reply
    do_POST = _reply

    def log_message(self, *a):
        pass


http.server.ThreadingHTTPServer.allow_reuse_address = True
srv = http.server.ThreadingHTTPServer(("0.0.0.0", port), H)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
srv.serve_forever()
PY

cat > "$WORK/client.py" <<'PY'
import http.client, ssl, sys

host, hp, tp, tok, tls_tok = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4], sys.argv[5]


def send(conn, tok):
    body = ("SELECT * FROM secrets WHERE token='%s';" % tok).encode()
    conn.request("POST", "/", body=body, headers={
        "Authorization": "Bearer " + tok,
        "Content-Type": "text/plain",
        "Content-Length": str(len(body)),
    })
    r = conn.getresponse()
    return r.status, r.read()


print("HTTP_ENDPOINT", send(http.client.HTTPConnection(host, hp, timeout=5), tok))
ctx = ssl._create_unverified_context()
print("TLS_CONTROL", send(http.client.HTTPSConnection(host, tp, context=ctx, timeout=5), tls_tok))
PY

cat > "$WORK/config.yaml" <<YAML
global:
  log_level: info
  duckdb_bin: /work/drill-stub
  pid_file: /tmp/qp-plain/quack-proxy.pid
  status_file: /tmp/qp-plain/status.json
listener:
  bind_host: 0.0.0.0
  health_path: /
  health_interval: 1s
  health_expect: systemd-drill-greeting
shards:
  - name: lan
    database: /tmp/qp-plain/lan.db
    port: ${HTTP_PORT}
    token: ${TOKEN}
YAML

# A config that only LOOKS encrypted must be refused by the real CLI.
cat > "$WORK/ssl.yaml" <<YAML
global:
  duckdb_bin: /work/drill-stub
listener:
  bind_host: 127.0.0.1
shards:
  - name: only
    database: /tmp/qp-plain/only.db
    port: 19743
    token: t
proxy:
  enabled: true
  output: /work/out.cfg
  bind_port: 19744
  ssl:
    enabled: true
YAML

# ---- topology ---------------------------------------------------------------
log "creating isolated bridge ${NET} (${IMAGE})"
docker network create "$NET" >/dev/null || fail "docker network create"
docker run -d --name "$SRV" --network "$NET" --cap-add=NET_RAW \
  -e DRILL_STUB_BIND=0.0.0.0 -v "$VOL:/work" \
  --entrypoint sleep "$IMAGE" infinity >/dev/null || fail "start server container"
docker exec "$SRV" mkdir -p /tmp/qp-plain || fail "create runtime dir"

log "starting real quack-proxy daemon (bind_host 0.0.0.0, port ${HTTP_PORT})"
docker exec -d "$SRV" sh -c \
  '/work/quack-proxy start -c /work/config.yaml > /work/daemon.log 2>&1' \
  || fail "start daemon"
docker exec -d "$SRV" python3 /work/tls_srv.py "$TLS_PORT" /work/cert.pem /work/key.pem \
  || fail "start TLS control server"

ready=0
for _ in $(seq 1 80); do
  if docker exec "$SRV" python3 -c \
      "import urllib.request; urllib.request.urlopen('http://127.0.0.1:${HTTP_PORT}/', timeout=1).read()" \
      >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.25
done
if [[ "$ready" -ne 1 ]]; then
  echo "--- daemon.log ---" >&2
  docker exec "$SRV" cat /work/daemon.log >&2 || true
  fail "shard never became reachable"
fi
log "shard reachable; status.json present: $(docker exec "$SRV" test -f /tmp/qp-plain/status.json && echo yes || echo no)"

# ---- capture ----------------------------------------------------------------
rm -f "$WORK/stop" "$WORK/capture.summary" "$WORK/port-${HTTP_PORT}.bin" "$WORK/port-${TLS_PORT}.bin"
log "starting on-path sniffer (separate container, shared netns)"
docker run -d --name "$SNIFF" --network "container:$SRV" --cap-add=NET_RAW \
  -v "$VOL:/work" --entrypoint python3 "$IMAGE" \
  /work/sniff.py /work "$HTTP_PORT,$TLS_PORT" /work/stop >/dev/null \
  || fail "start sniffer"
sleep 0.5

log "client on the same bridge sends token over HTTP, control token over TLS"
CLIENT_OUT="$(docker run --rm --network "$NET" -v "$VOL:/work:ro" \
  --entrypoint python3 "$IMAGE" \
  /work/client.py "$SRV" "$HTTP_PORT" "$TLS_PORT" "$TOKEN" "$TLS_TOKEN" 2>&1)" \
  || { echo "$CLIENT_OUT" >&2; fail "client requests failed"; }
echo "$CLIENT_OUT" | sed 's/^/    /'
grep -q "HTTP_ENDPOINT (200," <<<"$CLIENT_OUT" || fail "plaintext endpoint did not answer 200"
grep -q "TLS_CONTROL (200," <<<"$CLIENT_OUT" || fail "TLS control endpoint did not answer 200"

sleep 0.5
touch "$WORK/stop"
for _ in $(seq 1 40); do [[ -f "$WORK/capture.summary" ]] && break; sleep 0.25; done
[[ -f "$WORK/capture.summary" ]] || fail "sniffer produced no summary (AF_PACKET unavailable?)"
log "capture summary: $(tr '\n' ' ' < "$WORK/capture.summary")"

HTTP_CAP="$WORK/port-${HTTP_PORT}.bin"
TLS_CAP="$WORK/port-${TLS_PORT}.bin"
[[ -s "$HTTP_CAP" ]] || fail "no TCP payload captured for the plaintext port ${HTTP_PORT}"
[[ -s "$TLS_CAP" ]] || fail "no TCP payload captured for the TLS control port ${TLS_PORT}"

# ---- assertions -------------------------------------------------------------
grep -aq "$TOKEN" "$HTTP_CAP" || fail "shard token NOT visible on the wire (did the capture break?)"
grep -aq "Authorization: Bearer" "$HTTP_CAP" || fail "HTTP headers not captured"
grep -aq "SELECT \* FROM secrets" "$HTTP_CAP" || fail "request body not captured"
echo "PLAINTEXT_TOKEN_VISIBLE=1 (token + Authorization header + SQL body in raw TCP payload)"

if grep -aq "$TLS_TOKEN" "$TLS_CAP"; then
  fail "TLS control canary leaked into the capture - the negative control is broken"
fi
if grep -aq "$TOKEN" "$TLS_CAP"; then
  fail "shard token appeared in the TLS control capture"
fi
echo "TLS_CONTROL_HIDDEN=1 (same L2 segment, TLS terminates before the wire)"

if docker exec "$SRV" /work/quack-proxy gen-proxy -c /work/ssl.yaml > "$WORK/ssl.log" 2>&1; then
  fail "proxy.ssl.enabled=true was accepted; product now claims TLS it may not have"
fi
grep -q "not implemented" "$WORK/ssl.log" || fail "unexpected rejection message: $(cat "$WORK/ssl.log")"
echo "PRODUCT_TLS_UNAVAILABLE=1 (real CLI refuses ssl.enabled=true: $(tail -1 "$WORK/ssl.log"))"

MODE="$(docker exec "$SRV" python3 -c \
  "import os,stat; print('%o' % stat.S_IMODE(os.stat('/tmp/qp-plain/status.json').st_mode))")"
[[ "$MODE" = "600" ]] || fail "status.json mode is ${MODE}, expected 600"
if docker exec "$SRV" python3 -c \
    "import sys; sys.exit(0 if '${TOKEN}' in open('/tmp/qp-plain/status.json').read() else 1)"; then
  echo "STATUS_TOKEN_AT_REST=1 (plaintext token on disk, protected only by mode ${MODE})"
else
  echo "STATUS_TOKEN_AT_REST=0 (token no longer persisted; update R-05 docs)"
fi

echo
echo "PLAINTEXT_AUDIT_PASSED"
echo "  This is evidence of a KNOWN weakness, not a fix. Do not expose a shard on"
echo "  an untrusted LAN without terminating TLS in an external proxy. See docs/SECURITY.md R-05."
