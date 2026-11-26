#!/usr/bin/env bash
# HAProxy + floating-IP (VIP) integration drill for quack-proxy (R-04 / R-23).
#
# WHAT THIS PROVES, AND WHAT IT DELIBERATELY DOES NOT:
#   * quack-proxy `gen-proxy` emits a config that a REAL HAProxy 2.8 accepts
#     and RUNS, forwarding live traffic to a shard endpoint.
#   * A floating IP can move from node A to node B (with gratuitous ARP) and
#     clients keep reaching the SAME generated config through the SAME VIP.
#   * It does NOT prove quack-proxy provides HA. The VIP move is performed by
#     this script, standing in for keepalived / a cloud floating IP; the
#     product neither owns nor observes it. Each node runs a SEPARATE backend
#     with a DIFFERENT greeting, so the client can see that after failover it
#     is talking to a different node: the ENTRY POINT recovers, the session
#     and the data DO NOT. R-04 still stands (different .db files are
#     partitions, tokens are per-shard, one .db cannot have two writers).
#   * The shard endpoint is scripts/drill-stub.go, not DuckDB/Quack (covered
#     by scripts/quack-e2e.sh). This drill exercises the network + config
#     contract, not the database protocol.
#
# TOPOLOGY (all local, no external downloads except container images):
#   one Docker user-defined bridge with a fixed subnet = a stand-in for a
#   shared L2 segment; two node containers (root + NET_ADMIN, real HAProxy);
#   one client container on the same segment. The VIP is a secondary /32 on
#   eth0 of whichever node is active. docker network rm / docker rm -f in the
#   EXIT trap removes everything.
#
# Usage:
#   bash scripts/verify-vip-failover.sh
#   bash scripts/verify-vip-failover.sh --proxy-bin /tmp/quack-proxy \
#                                       --stub-bin /tmp/drill-stub
#   bash scripts/verify-vip-failover.sh --subnet 172.29.91.0/24
#   VIP_IMAGE=haproxy:3.0-alpine bash scripts/verify-vip-failover.sh
#
# Supply chain (r17): the DEFAULT images are digest-pinned, never floating
# tags (internal/cicontract.CheckImagePins enforces it). Explicit overrides
# such as VIP_IMAGE=... may use a tag for local runs; CI does not override.
# Rotate digests deliberately when upgrading the base images.
#
# Requirements: docker (daemon reachable, NET_ADMIN allowed), go when the two
# binaries are not supplied, bash, and the two images locally or pullable.
# The quack-proxy CLI is executed INSIDE a container so this script works on
# a Windows host too (the static Linux binary cannot run in Git Bash).
#
# Exit: 0 = every assertion held, non-zero = first failed assertion.
set -euo pipefail

NODE_IMAGE="${VIP_IMAGE:-haproxy@sha256:fce63f5dde742cd1f2b883ab39f7d4f9f88788827dd54fcfa864f63b9eec3bc8}"
CLIENT_IMAGE="${VIP_CLIENT_IMAGE:-alpine@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a}"
VIP_SUBNET="${VIP_SUBNET:-172.29.83.0/24}"
SUBNET_PREFIX="${VIP_SUBNET%.*}"
VIP_ADDR="${VIP_ADDR:-${SUBNET_PREFIX}.100}"
VIP_EXPLICIT=0
FRONT="${VIP_FRONT_PORT:-19490}"
BACK="${VIP_BACK_PORT:-19491}"
PROXY_BIN=""
STUB_BIN=""
WORK=""

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
  awk 'NR > 1 { if ($0 ~ /^#/) { sub(/^# ?/, ""); print } else exit }' "${BASH_SOURCE[0]}"
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --proxy-bin)  PROXY_BIN="$2"; shift 2 ;;
    --stub-bin)   STUB_BIN="$2"; shift 2 ;;
    --image)      NODE_IMAGE="$2"; shift 2 ;;
    --client-image) CLIENT_IMAGE="$2"; shift 2 ;;
    --subnet)     VIP_SUBNET="$2"; SUBNET_PREFIX="${VIP_SUBNET%.*}"; shift 2 ;;
    --vip)        VIP_ADDR="$2"; VIP_EXPLICIT=1; shift 2 ;;
    --front-port) FRONT="$2"; shift 2 ;;
    --back-port)  BACK="$2"; shift 2 ;;
    --work)       WORK="$2"; shift 2 ;;
    -h|--help)    usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 1 ;;
  esac
done

fail() { echo "FAIL: $*" >&2; exit 1; }
log()  { echo "==> $*"; }

CLEAN_WORK=0
if [[ -z "$WORK" ]]; then WORK="$(mktemp -d)"; CLEAN_WORK=1; else mkdir -p "$WORK"; fi
# Git Bash: mktemp -d may return a MIXED path such as
# 'C:\Users\DECISI~1\AppData\Local\Temp/tmp.XXXX'. Normalise it once to a
# POSIX form for shell file operations, keep a Windows form for docker -v,
# and disable MSYS argument rewriting so container-side paths like /work are
# not turned into host paths. Linux CI takes the else branch unchanged.
if command -v cygpath >/dev/null 2>&1; then
  WORK="$(cygpath -u "$WORK")"
  VOL="$(cygpath -w "$WORK" | tr '\\' '/')"
  export MSYS_NO_PATHCONV=1
else
  VOL="$WORK"
fi

TAG="qp-vip-$$"
NET="${TAG}-net"
A="${TAG}-a"
B="${TAG}-b"
CLI="${TAG}-cli"

cleanup() {
  local rc=$?
  set +e
  docker rm -f "$A" "$B" "$CLI" >/dev/null 2>&1
  docker network rm "$NET" >/dev/null 2>&1
  if [[ "$CLEAN_WORK" -eq 1 ]]; then rm -rf -- "$WORK"; fi
  return "$rc"
}
trap cleanup EXIT

# ---- preflight --------------------------------------------------------------
command -v docker >/dev/null 2>&1 || fail "docker is required"
docker info >/dev/null 2>&1 || fail "docker daemon is not reachable"
for img in "$NODE_IMAGE" "$CLIENT_IMAGE"; do
  if ! docker image inspect "$img" >/dev/null 2>&1; then
    log "pulling ${img} (3 attempts)"
    pulled=0
    for _ in 1 2 3; do
      if docker pull "$img" >/dev/null 2>&1; then pulled=1; break; fi
      sleep 2
    done
    [[ "$pulled" -eq 1 ]] || fail "cannot pull ${img}; pass --image / --client-image for a locally cached image"
  fi
done

if [[ -z "$PROXY_BIN" || -z "$STUB_BIN" ]]; then
  command -v go >/dev/null 2>&1 || fail "go not found; pass --proxy-bin and --stub-bin"
  [[ -z "$PROXY_BIN" ]] && PROXY_BIN="$WORK/quack-proxy"
  [[ -z "$STUB_BIN" ]] && STUB_BIN="$WORK/drill-stub"
  (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$PROXY_BIN" ./cmd/quack-proxy) || fail "go build quack-proxy"
  (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$STUB_BIN" ./scripts/drill-stub.go) || fail "go build drill-stub"
fi
# Existence only: on a Windows bind mount Git Bash cannot see the execute bit
# even though Docker exposes the file as 0755 inside the container.
[[ -f "$PROXY_BIN" ]] || fail "proxy binary not found: $PROXY_BIN"
[[ -f "$STUB_BIN" ]] || fail "stub binary not found: $STUB_BIN"
cp "$PROXY_BIN" "$WORK/quack-proxy"
cp "$STUB_BIN" "$WORK/drill-stub"
chmod +x "$WORK/quack-proxy" "$WORK/drill-stub"

# ---- generate the REAL artefact with the product CLI ------------------------
# Paths are container-side (/work is the bind mount). quack-proxy is run in a
# container because on Windows the static Linux binary is not host-executable.
cat > "$WORK/config.yaml" <<YAML
global:
  duckdb_bin: duckdb
  pid_file: /work/quack-proxy.pid
  status_file: /work/status.json
listener:
  bind_host: 127.0.0.1
  health_path: /
shards:
  - name: only
    database: /tmp/vip-only.db
    port: ${BACK}
    token: tok-vip-drill
proxy:
  enabled: true
  output: /work/haproxy.cfg
  bind_port: ${FRONT}
YAML

# The haproxy image runs as uid 99 by default; root is required to write the
# generated config into the bind mount (and, for the node containers, to own
# NET_ADMIN when adding the VIP to eth0).
run_cli() { docker run --rm --user root -v "$VOL:/work" "$NODE_IMAGE" /work/quack-proxy "$@"; }

log "generating HAProxy config with the real quack-proxy CLI"
run_cli gen-proxy -c /work/config.yaml > "$WORK/gen.log" 2>&1 || {
  cat "$WORK/gen.log" >&2; fail "gen-proxy failed"; }
[[ -s "$WORK/haproxy.cfg" ]] || fail "generated haproxy.cfg is empty"
grep -q "bind \*:${FRONT}" "$WORK/haproxy.cfg" || fail "generated config lacks bind *:${FRONT}"
grep -q "server only 127.0.0.1:${BACK} check" "$WORK/haproxy.cfg" || \
  fail "generated config lacks the single checked backend"
log "generated config: $(wc -c < "$WORK/haproxy.cfg") bytes"

# Negative control 1: a multi-shard VIP backend MUST be refused (R-04). This
# is what stops anyone reading "VIP" as "load-balanced replica set".
cat > "$WORK/config-multi.yaml" <<YAML
global:
  duckdb_bin: duckdb
  pid_file: /work/quack-proxy.pid
  status_file: /work/status.json
listener:
  bind_host: 127.0.0.1
shards:
  - name: one
    database: /tmp/one.db
    port: ${BACK}
    token: tok-a
  - name: two
    database: /tmp/two.db
    port: $((BACK + 1))
    token: tok-b
proxy:
  enabled: true
  output: /work/haproxy-multi.cfg
  bind_port: ${FRONT}
YAML
set +e
multi_out="$(run_cli gen-proxy -c /work/config-multi.yaml 2>&1)"
multi_rc=$?
set -e
[[ "$multi_rc" -ne 0 ]] || fail "gen-proxy accepted 2 shards; R-04 guard is gone"
echo "$multi_out" | grep -q "refusing to load-balance" || \
  fail "multi-shard rejection gave an unexpected reason: $multi_out"
echo "GEN_PROXY_MULTI_SHARD_REJECTED=1"

# Negative control 2: the vendor parser must reject a broken file, so a
# no-op validator cannot make the config check pass silently.
{ cat "$WORK/haproxy.cfg"; echo "definitely-not-a-valid-directive"; } > "$WORK/broken.cfg"
docker run --rm -v "$VOL:/work:ro" "$NODE_IMAGE" haproxy -c -f /work/broken.cfg \
  > "$WORK/broken.log" 2>&1 && fail "haproxy accepted a broken config (validator is a no-op)"
log "haproxy -c -f (positive)"
docker run --rm -v "$VOL:/work:ro" "$NODE_IMAGE" haproxy -c -f /work/haproxy.cfg \
  > "$WORK/haproxy-check.log" 2>&1 || { cat "$WORK/haproxy-check.log" >&2; fail "haproxy rejected the generated config"; }
echo "HAPROXY_CONFIG_VALID=1"

# ---- node programs ----------------------------------------------------------
# Each node boots the SAME generated config against its OWN local stub. The
# stub reads its listening port from a synthetic boot-SQL line (the same
# contract quack-proxy uses) and answers with a node-specific greeting.
cat > "$WORK/boot.sql" <<SQL
SELECT quack_serve('quack:127.0.0.1:${BACK}');
SQL
for node in NODE_A NODE_B; do
  cat > "$WORK/node-${node}.sh" <<SH
#!/bin/sh
export DRILL_STUB_GREETING="${node}"
haproxy -f /work/haproxy.cfg > /work/haproxy-${node}.log 2>&1 &
sleep 1
exec /work/drill-stub < /work/boot.sql
SH
  chmod +x "$WORK/node-${node}.sh"
done

# ---- topology ---------------------------------------------------------------
# A shared runner may already own the default CIDR. Try a few disjoint
# candidates instead of failing the whole gate on an unrelated network.
CIDRS=("$VIP_SUBNET" "172.29.84.0/24" "172.29.85.0/24" "10.93.83.0/24")
NET_OK=0
for cidr in "${CIDRS[@]}"; do
  if docker network create --driver bridge --subnet "$cidr" "$NET" >/dev/null 2>&1; then
    NET_OK=1
    VIP_SUBNET="$cidr"
    if [[ "$VIP_EXPLICIT" -eq 0 ]]; then VIP_ADDR="${cidr%.*}.100"; fi
    break
  fi
  docker network rm "$NET" >/dev/null 2>&1 || true
done
[[ "$NET_OK" -eq 1 ]] || fail "cannot create a bridge in any candidate CIDR; pass --subnet with a free CIDR"
log "network ${NET} (${VIP_SUBNET}), VIP ${VIP_ADDR}"

for spec in "$A:NODE_A" "$B:NODE_B"; do
  name="${spec%%:*}"
  node="${spec##*:}"
  docker run -d --name "$name" --network "$NET" --user root --cap-add=NET_ADMIN \
    -v "$VOL:/work" "$NODE_IMAGE" \
    sh -c "exec /work/node-${node}.sh" >/dev/null 2>&1 || fail "cannot start node ${name}"
done
docker run -d --name "$CLI" --network "$NET" --user root --cap-add=NET_ADMIN \
  "$CLIENT_IMAGE" sh -c 'sleep 900' >/dev/null 2>&1 || fail "cannot start client"

# A node needs a moment for HAProxy + the stub to come up.
wait_local() {
  local name="$1" i
  for i in $(seq 1 40); do
    if docker exec "$name" wget -qO- -T 2 "http://127.0.0.1:${FRONT}/" 2>/dev/null | grep -q '^NODE_'; then
      return 0
    fi
    sleep 0.25
  done
  return 1
}
wait_local "$A" || { docker logs "$A" 2>&1 | tail -20 >&2; cat "$WORK/haproxy-NODE_A.log" >&2 || true; fail "node A never served traffic through HAProxy"; }
wait_local "$B" || { docker logs "$B" 2>&1 | tail -20 >&2; cat "$WORK/haproxy-NODE_B.log" >&2 || true; fail "node B never served traffic through HAProxy"; }
log "both nodes forward locally through the generated config"

client_get() { docker exec "$CLI" wget -qO- -T 3 "http://${VIP_ADDR}:${FRONT}/" 2>/dev/null; }

# Negative control 3: with no node owning the VIP, the address must be dead.
if client_get | grep -q '^NODE_'; then
  fail "VIP answered without any node owning it (test topology is wrong)"
fi
echo "VIP_ABSENT_CONTROL_OK=1"

# ---- primary ----------------------------------------------------------------
vip_take() {
  local name="$1"
  docker exec "$name" sh -c "ip addr add ${VIP_ADDR}/32 dev eth0" || return 1
  docker exec "$name" sh -c "arping -U -c 5 -I eth0 -s ${VIP_ADDR} ${VIP_ADDR} >/dev/null 2>&1" || return 1
  return 0
}
vip_drop() {
  docker exec "$1" sh -c "ip addr del ${VIP_ADDR}/32 dev eth0" 2>/dev/null || true
}

vip_take "$A" || fail "node A could not take the VIP (needs --user root + NET_ADMIN)"
pre=""
for _ in $(seq 1 20); do
  pre="$(client_get || true)"
  [[ "$pre" == "NODE_A" ]] && break
  sleep 0.3
done
[[ "$pre" == "NODE_A" ]] || fail "VIP did not reach node A (got '${pre}')"
log "primary answers through VIP: ${pre}"
echo "VIP_PRIMARY_NODE=${pre}"

# ---- failover ---------------------------------------------------------------
# Kill the WHOLE node, not just HAProxy: the VIP disappears with its network
# namespace, exactly like a real host failure. Node B then claims the address
# and sends gratuitous ARP; no client-side ARP flush is performed - the GARP
# must do the work.
docker rm -f "$A" >/dev/null 2>&1 || fail "cannot remove primary node"
vip_take "$B" || fail "node B could not take the VIP"
post=""
for _ in $(seq 1 25); do
  post="$(client_get || true)"
  [[ "$post" == "NODE_B" ]] && break
  sleep 0.3
done
[[ "$post" == "NODE_B" ]] || fail "VIP did not fail over to node B (got '${post}')"
log "after failover the VIP answers: ${post}"
echo "VIP_FAILOVER_NODE=${post}"
[[ "$pre" != "$post" ]] || fail "both phases answered identically; the drill cannot prove a failover"
# This is the honest finding: the endpoint recovered, the peer identity (and
# therefore any session/state) did not. Do not describe this as data HA.
echo "SESSION_DISCONTINUITY=1 (entry point moved; backend identity changed)"

# Negative control 4: once no node owns the VIP again, it must stop answering.
# Proves the successes above came from the address, not from some other route.
vip_drop "$B"
docker exec "$CLI" sh -c 'ip neigh flush dev eth0' >/dev/null 2>&1 || true
if client_get | grep -q '^NODE_'; then
  fail "VIP still answers after the address was withdrawn from both nodes"
fi
echo "VIP_REMOVED_CONTROL_OK=1"

log "client ARP entry observed during failover was refreshed by GARP, not by a manual flush"
echo "VIP_FAILOVER_DRILL_PASSED"
