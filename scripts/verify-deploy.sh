#!/usr/bin/env bash
# Validate the two deployment artefacts with the REAL vendor tools.
#
#   * `haproxy -c -f` on a config produced by `quack-proxy gen-proxy`
#     (plus an optional real forwarding smoke test with --forward)
#   * `systemd-analyze verify` on deploy/systemd/quack-proxy.service
#
# Before this script existed the generated HAProxy text was covered only by
# Go unit tests and the systemd unit only by eyeballing it (R-21 / #21).
# A negative control is included because `systemd-analyze verify` can return
# 0 even when it reports a parse error: we require the diagnostic TEXT, not
# merely a zero exit code, so a no-op validator cannot silently pass.
#
# Typical CI use (ubuntu-latest has the full systemd unit tree):
#   sudo apt-get install -y haproxy systemd
#   bash scripts/verify-deploy.sh --forward
#
# Offline/container use: --haproxy-bin, --systemd-analyze-bin and
# --extra-unit-path let you point at binaries extracted from .deb packages.
set -euo pipefail

WORK=""
PROXY_BIN=""
STUB_BIN=""
HAPROXY_BIN="haproxy"
SYSTEMD_ANALYZE_BIN="systemd-analyze"
EXTRA_UNIT_PATH=""
ONLY="all"
DO_FORWARD=0
FRONT=19600
BACK=19601
BODY="quack-forward-ok"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

usage() {
  cat <<'USAGE'
usage: verify-deploy.sh [options]
  --work DIR                  working dir (default: mktemp -d, auto-cleaned)
  --proxy-bin PATH            prebuilt quack-proxy binary (default: go build)
  --stub-bin PATH             prebuilt HTTP stub for --forward (default: go build)
  --haproxy-bin PATH          haproxy binary (default: haproxy)
  --systemd-analyze-bin PATH  systemd-analyze binary (default: systemd-analyze)
  --extra-unit-path DIR       extra unit search dir (e.g. an extracted .deb tree)
  --only haproxy|systemd|all  which validator to run (default: all)
  --forward                   also prove real traffic forwarding via HAProxy
  --front-port N / --back-port N
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --work) WORK="$2"; shift 2 ;;
    --proxy-bin) PROXY_BIN="$2"; shift 2 ;;
    --stub-bin) STUB_BIN="$2"; shift 2 ;;
    --haproxy-bin) HAPROXY_BIN="$2"; shift 2 ;;
    --systemd-analyze-bin) SYSTEMD_ANALYZE_BIN="$2"; shift 2 ;;
    --extra-unit-path) EXTRA_UNIT_PATH="$2"; shift 2 ;;
    --only) ONLY="$2"; shift 2 ;;
    --forward) DO_FORWARD=1; shift ;;
    --front-port) FRONT="$2"; shift 2 ;;
    --back-port) BACK="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "FAIL: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

case "$ONLY" in haproxy|systemd|all) ;; *) echo "FAIL: bad --only: $ONLY" >&2; exit 2 ;; esac

CLEAN=0
if [[ -z "$WORK" ]]; then WORK="$(mktemp -d)"; CLEAN=1; else mkdir -p "$WORK"; fi
HPID=""; SPID=""
cleanup() {
  set +e
  [[ -n "$HPID" ]] && kill "$HPID" 2>/dev/null
  [[ -n "$SPID" ]] && kill "$SPID" 2>/dev/null
  if [[ "$CLEAN" -eq 1 ]]; then rm -rf -- "$WORK"; fi
  return 0
}
trap cleanup EXIT

UNIT_SRC="$REPO/deploy/systemd/quack-proxy.service"
[[ -f "$UNIT_SRC" ]] || { echo "FAIL: missing $UNIT_SRC" >&2; exit 1; }

# ---- build / locate the CLI -------------------------------------------------
if [[ -z "$PROXY_BIN" ]]; then
  command -v go >/dev/null 2>&1 || { echo "FAIL: go not found; pass --proxy-bin" >&2; exit 1; }
  (cd "$REPO" && go build -o "$WORK/quack-proxy" ./cmd/quack-proxy)
  PROXY_BIN="$WORK/quack-proxy"
fi
[[ -x "$PROXY_BIN" ]] || { echo "FAIL: proxy binary not executable: $PROXY_BIN" >&2; exit 1; }

# ---- generate the real artefact --------------------------------------------
cat > "$WORK/config.yaml" <<YAML
global:
  duckdb_bin: duckdb
  pid_file: $WORK/quack-proxy.pid
  status_file: $WORK/status.json
listener:
  bind_host: 127.0.0.1
  health_path: /
shards:
  - name: only
    database: $WORK/only.db
    port: $BACK
    token: tok-verify
proxy:
  enabled: true
  output: $WORK/haproxy.cfg
  bind_port: $FRONT
YAML
"$PROXY_BIN" gen-proxy -c "$WORK/config.yaml" > "$WORK/gen.log" 2>&1 || {
  echo "FAIL: gen-proxy failed" >&2; cat "$WORK/gen.log" >&2; exit 1;
}
[[ -s "$WORK/haproxy.cfg" ]] || { echo "FAIL: generated config is empty" >&2; exit 1; }
echo "==> generated config ($(wc -c < "$WORK/haproxy.cfg") bytes)"

# ---- HAProxy ----------------------------------------------------------------
if [[ "$ONLY" == "all" || "$ONLY" == "haproxy" ]]; then
  command -v "$HAPROXY_BIN" >/dev/null 2>&1 || [[ -x "$HAPROXY_BIN" ]] || {
    echo "FAIL: haproxy binary not found: $HAPROXY_BIN" >&2; exit 1; }
  echo "==> $HAPROXY_BIN -c -f"
  "$HAPROXY_BIN" -c -f "$WORK/haproxy.cfg" > "$WORK/haproxy-check.log" 2>&1 || {
    echo "FAIL: haproxy -c rejected the generated config" >&2; cat "$WORK/haproxy-check.log" >&2; exit 1; }
  "$HAPROXY_BIN" -c -V -f "$WORK/haproxy.cfg" > "$WORK/haproxy-verbose.log" 2>&1 || {
    echo "FAIL: haproxy -c -V failed" >&2; cat "$WORK/haproxy-verbose.log" >&2; exit 1; }
  if grep -qi "Configuration file is valid" "$WORK/haproxy-verbose.log"; then
    echo "HAPROXY_CONFIG_VALID=1 (explicit confirmation)"
  else
    # Some HAProxy versions print nothing on success. Both -c and -c -V
    # returning 0 is the authoritative verdict; do not fail on wording.
    echo "HAPROXY_CONFIG_VALID=1 (rc=0; version printed no confirmation)"
  fi

  # Negative control: the vendor parser must REJECT a broken config. A stub
  # that always exits 0 would otherwise make this whole gate a no-op.
  { cat "$WORK/haproxy.cfg"; echo "definitely-not-a-valid-directive"; } > "$WORK/broken.cfg"
  if "$HAPROXY_BIN" -c -f "$WORK/broken.cfg" > "$WORK/broken.log" 2>&1; then
    echo "FAIL: haproxy accepted a deliberately broken config — validator is not real" >&2
    exit 1
  fi
  echo "HAPROXY_NEGATIVE_CONTROL_OK=1"

  if [[ "$DO_FORWARD" -eq 1 ]]; then
    command -v curl >/dev/null 2>&1 || { echo "FAIL: --forward needs curl" >&2; exit 1; }
    if [[ -n "$STUB_BIN" ]]; then
      [[ -x "$STUB_BIN" ]] || { echo "FAIL: stub not executable: $STUB_BIN" >&2; exit 1; }
      STUB="$STUB_BIN"
    else
      STUB="$WORK/stub"
    fi
    if [[ -z "$STUB_BIN" ]]; then
    cat > "$WORK/stub.go" <<'GO'
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: stub <port> <body>")
		os.Exit(2)
	}
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(os.Args[2]))
	})
	if err := http.ListenAndServe("127.0.0.1:"+os.Args[1], nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
GO
    (cd "$WORK" && go build -o stub stub.go) || { echo "FAIL: stub build" >&2; exit 1; }
    fi
    "$STUB" "$BACK" "$BODY" > "$WORK/stub.log" 2>&1 &
    SPID=$!
    sleep 0.4
    # -db keeps HAProxy in the foreground even though the template says daemon.
    "$HAPROXY_BIN" -db -f "$WORK/haproxy.cfg" > "$WORK/haproxy-run.log" 2>&1 &
    HPID=$!
    sleep 1.2
    got="$(curl -sS -m 3 "http://127.0.0.1:$FRONT/" 2>"$WORK/curl.err")" || {
      echo "FAIL: frontend did not answer" >&2; cat "$WORK/curl.err" "$WORK/haproxy-run.log" >&2; exit 1; }
    if [[ "$got" != "$BODY" ]]; then
      echo "FAIL: forwarded body = '$got', want '$BODY'" >&2; exit 1
    fi
    kill "$HPID" "$SPID" 2>/dev/null || true
    HPID=""; SPID=""
    echo "HAPROXY_FORWARD_OK=1 (frontend $FRONT -> backend $BACK)"
  fi
fi

# ---- systemd ----------------------------------------------------------------
if [[ "$ONLY" == "all" || "$ONLY" == "systemd" ]]; then
  [[ -x "$SYSTEMD_ANALYZE_BIN" ]] || command -v "$SYSTEMD_ANALYZE_BIN" >/dev/null 2>&1 || {
    echo "FAIL: systemd-analyze not found: $SYSTEMD_ANALYZE_BIN" >&2; exit 1; }

  # systemd refuses (or warns about) a world-writable/executable unit. A
  # Windows bind mount shows up as 0777, so always verify a clean 0644 copy.
  mkdir -p "$WORK/units" "$WORK/badunits"
  cp "$UNIT_SRC" "$WORK/units/quack-proxy.service"
  chmod 0644 "$WORK/units/quack-proxy.service"
  if [[ -n "$EXTRA_UNIT_PATH" ]]; then
    export SYSTEMD_UNIT_PATH="$WORK/units:$EXTRA_UNIT_PATH"
  fi

  # Make ExecStart and User= resolvable when we have the privileges, so the
  # verdict is about the UNIT and not about the sandbox.
  if [[ "$(id -u)" -eq 0 ]]; then
    if [[ ! -x /usr/local/bin/quack-proxy ]]; then
      mkdir -p /usr/local/bin
      printf '#!/bin/sh\nexit 0\n' > /usr/local/bin/quack-proxy
      chmod 0755 /usr/local/bin/quack-proxy
    fi
    getent passwd quack-proxy >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin quack-proxy 2>/dev/null || true
  fi

  echo "==> $SYSTEMD_ANALYZE_BIN verify"
  vrc=0
  "$SYSTEMD_ANALYZE_BIN" verify "$WORK/units/quack-proxy.service" > "$WORK/verify.log" 2>&1 || vrc=$?
  cat "$WORK/verify.log"
  if [[ "$vrc" -ne 0 ]]; then
    echo "FAIL: systemd-analyze verify rc=$vrc" >&2; exit 1
  fi
  if grep -qiE 'Failed to parse|Unknown lvalue|Unknown key|Unknown section|Invalid' "$WORK/verify.log"; then
    echo "FAIL: verify reported a unit syntax problem" >&2; exit 1
  fi
  echo "SYSTEMD_UNIT_VALID=1"

  # Negative control: an invalid KillMode MUST be diagnosed. verify returns 0
  # for some parse errors, so assert on the diagnostic text (see header).
  sed 's/KillMode=control-group/KillMode=TOTALLY_BOGUS/' "$UNIT_SRC" > "$WORK/badunits/bad.service"
  chmod 0644 "$WORK/badunits/bad.service"
  "$SYSTEMD_ANALYZE_BIN" verify "$WORK/badunits/bad.service" > "$WORK/bad.log" 2>&1 || true
  grep -qi "Failed to parse kill mode" "$WORK/bad.log" || {
    echo "FAIL: negative control was NOT diagnosed — verify is not really validating" >&2
    cat "$WORK/bad.log" >&2; exit 1; }
  echo "SYSTEMD_NEGATIVE_CONTROL_OK=1"
fi

echo "DEPLOY_VERIFY_PASSED (only=$ONLY forward=$DO_FORWARD)"
