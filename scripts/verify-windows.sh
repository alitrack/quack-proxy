#!/usr/bin/env bash
# R-18 / r9: prove on a REAL Windows host that the shipped platform policy is
# honest. Before r9 the Windows artifact looked usable (`version` worked) but
# `start` died with a misleading "duckdb_bin ... not executable" (POSIX mode
# bits don't exist on Windows), or silently failed inside a missing bash.
#
# Now `start` must refuse up front with an actionable message. This script is
# a REQUIRED gate on windows-latest and is also runnable locally in Git Bash:
#
#   bash scripts/verify-windows.sh                       # builds the exe first
#   bash scripts/verify-windows.sh --proxy-bin /c/tmp/qp.exe --work /c/tmp/w
#
# It intentionally passes a NONEXISTENT config path: seeing the platform error
# instead of "failed to load config" proves the guard runs before config load.
set -euo pipefail

PROXY_BIN=""
WORK=""
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() { echo "FAIL: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --proxy-bin) PROXY_BIN="$2"; shift 2 ;;
    --work)      WORK="$2"; shift 2 ;;
    -h|--help)   sed -n '2,16p' "$0"; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*|Windows_NT) : ;;
  *) fail "this drill must run on a real Windows host (uname=$(uname -s)); use the windows-latest CI job" ;;
esac

CLEAN_WORK=0
if [[ -z "$WORK" ]]; then
  WORK="$(mktemp -d)"
  CLEAN_WORK=1
else
  mkdir -p "$WORK"
fi
cleanup() {
  if [[ "$CLEAN_WORK" -eq 1 ]]; then rm -rf "$WORK"; fi
}
trap cleanup EXIT

if [[ -z "$PROXY_BIN" ]]; then
  command -v go >/dev/null 2>&1 || fail "go is required when --proxy-bin is not supplied"
  PROXY_BIN="$WORK/quack-proxy.exe"
  echo "==> cross-building Windows artifact"
  # go is a native Windows program: give it a Windows path for -o, while
  # bash keeps using the MSYS path when executing the result.
  BUILD_OUT="$PROXY_BIN"
  if command -v cygpath >/dev/null 2>&1; then
    BUILD_OUT="$(cygpath -w "$PROXY_BIN")"
  fi
  (cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
    go build -trimpath -o "$BUILD_OUT" ./cmd/quack-proxy)
elif command -v cygpath >/dev/null 2>&1; then
  # Callers may pass a native Windows path (e.g. $RUNNER_TEMP on Actions).
  PROXY_BIN="$(cygpath -u "$PROXY_BIN" 2>/dev/null || printf '%s' "$PROXY_BIN")"
fi

[[ -f "$PROXY_BIN" ]] || fail "Windows binary not found: $PROXY_BIN"
# POSIX mode bits are meaningless here; only require that the file exists.
echo "==> WINDOWS_BINARY=$PROXY_BIN"

echo "==> control: version still works"
VERSION_OUT="$("$PROXY_BIN" version 2>&1)"
echo "    $VERSION_OUT"
[[ "$VERSION_OUT" == quack-proxy* ]] || fail "version did not report the product name"

echo "==> assertion: start refuses with the real reason (config path does not exist)"
set +e
START_OUT="$("$PROXY_BIN" start -c "$WORK/definitely-missing.yaml" 2>&1)"
START_RC=$?
set -e
echo "$START_OUT" | head -8
[[ "$START_RC" -ne 0 ]] || fail "start unexpectedly succeeded on Windows"
grep -q "not a supported runtime platform" <<<"$START_OUT" \
  || fail "start did not state that Windows is unsupported"
grep -q "R-18" <<<"$START_OUT" \
  || fail "start message does not point at R-18"
grep -q "WSL2" <<<"$START_OUT" \
  || fail "start message does not offer a usable alternative"

echo "==> negative controls"
if grep -q "failed to load config" <<<"$START_OUT"; then
  fail "platform guard did not run before config load"
fi
if grep -q "not found on PATH or not executable" <<<"$START_OUT"; then
  fail "r9 regression: the old misleading duckdb_bin error is back"
fi
if grep -q "all shards started" <<<"$START_OUT"; then
  fail "start actually launched shards on Windows"
fi
[[ ! -e "$WORK/quack-proxy.pid" ]] || fail "start left a pid file behind"

echo "WINDOWS_RUNTIME_GUARD_PASSED"
