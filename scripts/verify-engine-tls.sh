#!/usr/bin/env bash
# Engine TLS capability audit for quack-proxy (R-05, r12).
#
# This is an AUDIT OF THE ENGINE, NOT A FIX OF THE PRODUCT. It proves, using
# exactly the pinned DuckDB v2.0.0 CLI + signed core extensions that
# scripts/quack-e2e.sh already pins, that the Quack engine NATIVELY supports
# TLS, and that quack-proxy deliberately turns it off with a hardcoded
# `disable_ssl = true` in its boot SQL. The ledger used to lump "built-in TLS"
# into "needs a design decision", which reads like a capability limit. It is
# not: the remaining work is a PRODUCT design decision (certificate and
# fingerprint distribution, health checks over HTTPS, status.json contract,
# HAProxy config), not implementing TLS in the engine.
#
# What is proven here (all with the real engine, no stubs):
#   1. quack_serve exposes disable_ssl / ssl_cert_file / ssl_key_file.
#   2. quack_generate_keys() emits a self-signed cert + SHA256 fingerprint.
#   3. disable_ssl := true  -> quack_serve reports an http://  listen URL.
#      disable_ssl := false -> quack_serve reports an https:// listen URL.
#   4. A real TLSv1.3 handshake succeeds (openssl s_client, independent of
#      DuckDB), a plaintext HTTP request to that port is rejected, and an
#      HTTPS GET returns 200.
#   5. A Quack client pinned to the correct ssl_fingerprint gets the query
#      result (42). A wrong fingerprint, a missing fingerprint, and a
#      wrong token are all rejected -- these negative controls prove the
#      positive assertion is actually checking something.
#   6. quack-proxy's boot SQL still hardcodes disable_ssl = true, i.e. the
#      product does NOT serve TLS today. If TLS is ever enabled in the
#      product, this gate is EXPECTED to fail: update it and the docs
#      together, do not delete it (same contract as verify-plaintext.sh).
#
# Scope limits, stated plainly: this exercises the ENGINE directly, not the
# quack-proxy daemon. It does NOT mean quack-proxy can terminate TLS, does not
# change any product behaviour, and does not resolve the cleartext-on-LAN or
# token-at-rest risks. Those remain open under R-05. Turning this capability
# into a shipped feature requires an operator-facing design decision.
#
# Supply chain: cache mode is STRICTLY OFFLINE, with no curl, no INSTALL and
# no CDN fallback. Every artifact is checked against the same SHA256 pins as
# scripts/prefetch-quack-e2e.sh; DuckDB re-verifies the core extension
# signatures on LOAD. Nothing is downloaded here.
#
# Usage:
#   bash scripts/verify-engine-tls.sh --cache-dir ./engine-cache
#   QUACK_TLS_OFFLINE=1 bash scripts/verify-engine-tls.sh \
#       --cache-dir "$RUNNER_TEMP/quack-e2e-cache"
#   bash scripts/verify-engine-tls.sh --cache-dir ./engine-cache \
#       --control-variant           # negative control, MUST exit non-zero
#
# Requirements: Linux, bash, sha256sum, tar, gunzip, openssl, curl, python3.
# The cache dir must be produced by scripts/prefetch-quack-e2e.sh.
#
# Exit: 0 = every assertion held, non-zero = first failed assertion.
set -euo pipefail

# --- pins, kept in sync with prefetch-quack-e2e.sh / quack-e2e.sh (r10) ---
DUCKDB_VERSION="v2.0.0"
CLI_FILE="duckdb-cli-linux-amd64.tar.gz"
CLI_SHA256="c6507206baf7793c6013dbd82368469a45f20dc0e17a6f92f204602f652ed2bc"
EXT_FILE="quack.duckdb_extension"
EXT_SHA256="9c893bd543ec2c5253d0c47d5614f90a1bfa1791c735dc6bbcea4e8bd3811a97"
HTTPFS_FILE="httpfs.duckdb_extension"
HTTPFS_SHA256="77e6c429aa04f25da7962572f12b19113e0ce46080fa1365ad66017aadcdb68d"
EXT_RELPATH="${DUCKDB_VERSION}/linux_amd64"
CACHE_MANIFEST="quack-e2e-manifest.txt"

# Two independent endpoints (plaintext control + TLS) on distinct ports.
TLS_PORT="${QUACK_TLS_PORT:-19911}"
PLAIN_PORT="${QUACK_TLS_PLAIN_PORT:-19912}"
HOST="127.0.0.1"
TOKEN="engine-tls-audit-token"
CONTROL_VARIANT=0
CACHE_DIR="${QUACK_TLS_CACHE_DIR:-}"
OFFLINE="${QUACK_TLS_OFFLINE:-0}"
WORK=""
KEEP=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
  awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "${BASH_SOURCE[0]}"
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cache-dir)       CACHE_DIR="$2"; shift 2 ;;
    --work)            WORK="$2"; KEEP=1; shift 2 ;;
    --port)            TLS_PORT="$2"; shift 2 ;;
    --plain-port)      PLAIN_PORT="$2"; shift 2 ;;
    --control-variant) CONTROL_VARIANT=1; shift ;;
    --keep)            KEEP=1; shift ;;
    -h|--help)         usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 1 ;;
  esac
done

fail() { echo "FAIL: $*" >&2; exit 1; }
log()  { echo "==> $*"; }

for tool in sha256sum tar gunzip openssl curl python3; do
  command -v "$tool" >/dev/null 2>&1 || fail "required tool not found: ${tool}"
done

if [[ "$OFFLINE" == "1" && -z "$CACHE_DIR" ]]; then
  fail "QUACK_TLS_OFFLINE=1 requires --cache-dir (or QUACK_TLS_CACHE_DIR)"
fi
[[ -n "$CACHE_DIR" ]] || fail "--cache-dir is required (offline-only by design; no download path)"
[[ "$TLS_PORT" != "$PLAIN_PORT" ]] || fail "TLS and plaintext ports must differ"

if [[ -z "$WORK" ]]; then WORK="$(mktemp -d /tmp/quack-tls.XXXXXX)"; fi
mkdir -p "$WORK"
SERVER_PIDS=()
cleanup() {
  local rc=$?
  for pid in "${SERVER_PIDS[@]:-}"; do
    kill -TERM "$pid" 2>/dev/null || true
    # The pipeline is a process group: bash wrapper + duckdb + tail. Killing
    # only the wrapper leaves the DuckDB listener holding the port (R-21).
    pkill -TERM -P "$pid" 2>/dev/null || true
  done
  # Belt and braces: match this run's unique database paths.
  pkill -TERM -f "quack-tls-audit-${TLS_PORT}" 2>/dev/null || true
  pkill -TERM -f "quack-tls-audit-${PLAIN_PORT}" 2>/dev/null || true
  sleep 0.3
  pkill -KILL -f "quack-tls-audit-${TLS_PORT}" 2>/dev/null || true
  pkill -KILL -f "quack-tls-audit-${PLAIN_PORT}" 2>/dev/null || true
  if [[ "$KEEP" -eq 0 ]]; then rm -rf -- "$WORK"; fi
  exit "$rc"
}
trap cleanup EXIT

# --- 1. pinned offline cache (no network, ever) ---------------------------
sha_is() {
  local file="$1" want="$2"
  [[ -f "$file" ]] || return 1
  printf '%s  %s\n' "$want" "$file" | sha256sum -c - >/dev/null 2>&1
}

log "verifying pinned offline cache: ${CACHE_DIR}"
sha_is "$CACHE_DIR/$CLI_FILE" "$CLI_SHA256" \
  || fail "offline cache missing/corrupt: ${CLI_FILE}"
sha_is "$CACHE_DIR/$EXT_FILE" "$EXT_SHA256" \
  || fail "offline cache missing/corrupt: ${EXT_FILE}"
sha_is "$CACHE_DIR/$HTTPFS_FILE" "$HTTPFS_SHA256" \
  || fail "offline cache missing/corrupt: ${HTTPFS_FILE}"
grep -qx "extension_relpath=${EXT_RELPATH}" "$CACHE_DIR/$CACHE_MANIFEST" 2>/dev/null \
  || fail "offline cache manifest missing or has wrong extension_relpath"
echo "OFFLINE_CACHE_USED=1 (cli ${CLI_SHA256:0:12}… / quack ${EXT_SHA256:0:12}… / httpfs ${HTTPFS_SHA256:0:12}…)"
echo "${CLI_SHA256}  ${CACHE_DIR}/${CLI_FILE}" | sha256sum -c -
echo "${EXT_SHA256}  ${CACHE_DIR}/${EXT_FILE}" | sha256sum -c -
echo "${HTTPFS_SHA256}  ${CACHE_DIR}/${HTTPFS_FILE}" | sha256sum -c -

mkdir -p "$WORK/engine" "$WORK/home/.duckdb/extensions/${EXT_RELPATH}" "$WORK/keys"
tar xzf "$CACHE_DIR/$CLI_FILE" -C "$WORK/engine"
D="$WORK/engine/duckdb"
[[ -x "$D" ]] || fail "duckdb binary not executable: ${D}"
cp "$CACHE_DIR/$EXT_FILE" "$WORK/home/.duckdb/extensions/${EXT_RELPATH}/$EXT_FILE"
cp "$CACHE_DIR/$HTTPFS_FILE" "$WORK/home/.duckdb/extensions/${EXT_RELPATH}/$HTTPFS_FILE"

# Everything that touches DuckDB gets the isolated HOME so LOAD resolves the
# signed extensions with zero network. There is deliberately no curl/INSTALL
# branch in this script.
duck() { env HOME="$WORK/home" "$@"; }

ENGINE_VERSION="$(duck "$D" --version)"
echo "ENGINE_VERSION=${ENGINE_VERSION}"
[[ "$ENGINE_VERSION" == *"v2.0.0"* ]] || fail "unexpected engine version: ${ENGINE_VERSION}"
duck "$D" -c "LOAD quack;" >/dev/null 2>&1 \
  || fail "LOAD quack failed although the signed extension was pre-seeded"
echo "SIGNED_CORE_EXTENSION_LOADED=1"

# --- 2. capability facts, straight from the engine's function catalog ----
PARAMS="$(duck "$D" -csv -noheader -c "
LOAD quack;
SELECT parameters::VARCHAR FROM duckdb_functions() WHERE function_name='quack_serve';")"
echo "QUACK_SERVE_PARAMS=${PARAMS}"
for needed in disable_ssl ssl_cert_file ssl_key_file; do
  grep -q "$needed" <<<"$PARAMS" \
    || fail "quack_serve lacks the ${needed} parameter - engine TLS capability changed"
done
echo "ENGINE_TLS_PARAMS_PRESENT=1 (disable_ssl / ssl_cert_file / ssl_key_file)"

CLIENT_PARAMS="$(duck "$D" -csv -noheader -c "
LOAD quack;
SELECT parameters::VARCHAR FROM duckdb_functions() WHERE function_name='quack_query';")"
grep -q "ssl_fingerprint" <<<"$CLIENT_PARAMS" \
  || fail "quack_query lacks ssl_fingerprint - client pinning capability changed"
echo "CLIENT_FINGERPRINT_PARAM_PRESENT=1"

# quack_generate_keys() is the engine's own certificate generator. Use its
# output, then independently recompute the SHA256 fingerprint with openssl so
# the positive control does not trust the engine's self-reported value blindly.
KEYS="$(duck "$D" -csv -c "LOAD quack; SELECT * FROM quack_generate_keys(directory := '${WORK}/keys');")"
echo "KEYGEN_OUTPUT=${KEYS}"
CERT="${WORK}/keys/server.pem"
KEY="${WORK}/keys/private_key.pem"
[[ -s "$CERT" && -s "$KEY" ]] || fail "quack_generate_keys did not produce cert + key"
FP_HEX="$(openssl x509 -in "$CERT" -noout -fingerprint -sha256 | cut -d= -f2 | tr -d ':')"
[[ ${#FP_HEX} -eq 64 ]] || fail "unexpected SHA256 fingerprint: ${FP_HEX}"
echo "SELF_SIGNED_KEYS_GENERATED=1 (SHA256 ${FP_HEX:0:16}…)"
openssl rsa -in "$KEY" -check -noout >/dev/null 2>&1 \
  || fail "generated private key is not a valid RSA key"
echo "GENERATED_KEY_VALID=1"

# --- 3. start two real endpoints ------------------------------------------
# quack_serve blocks inside the DuckDB process. The product feeds boot SQL via
# `(cat file; tail -f /dev/null) | duckdb <db>`; we use the identical shape so
# the audit exercises the same engine path the daemon uses. nohup + a detached
# bash wrapper keeps the pipeline alive after this shell returns (otherwise
# SIGHUP silently reaps the listener).
start_server() {
  local name="$1" port="$2" sql="$3"
  local db="$WORK/quack-tls-audit-${port}.db"
  local sqlfile="$WORK/${name}.sql"
  printf '%s\n' "$sql" > "$sqlfile"
  # HOME must be set INSIDE the detached wrapper: an inherited HOME would
  # make DuckDB look for the signed extensions under /root/.duckdb and try
  # to INSTALL them from the network (which cache mode must never do).
  nohup bash -c "( cat '${sqlfile}'; tail -f /dev/null ) | env HOME='${WORK}/home' '${D}' '${db}'" \
    > "$WORK/${name}.log" 2>&1 &
  SERVER_PIDS+=("$!")
  local up=0 i
  for i in $(seq 1 80); do
    if python3 -c "
import socket,sys
s=socket.socket(); s.settimeout(0.5)
try: s.connect(('${HOST}',${port})); print('ok'); sys.exit(0)
except Exception: sys.exit(1)
s.close()" >/dev/null 2>&1; then up=1; break; fi
    sleep 0.25
  done
  [[ "$up" -eq 1 ]] || {
    echo "--- ${name} server log ---" >&2; cat "$WORK/${name}.log" >&2 || true
    fail "endpoint ${name} never bound port ${port}"
  }
  echo "${name^^}_ENDPOINT_UP=1"
}

TLS_SQL="LOAD quack;
LOAD httpfs;
CALL quack_serve('quack:${HOST}:${TLS_PORT}', token := '${TOKEN}',
  disable_ssl := $(if [[ "$CONTROL_VARIANT" -eq 1 ]]; then echo true; else echo false; fi),
  $(if [[ "$CONTROL_VARIANT" -eq 1 ]]; then echo ''; else echo "ssl_cert_file := '${CERT}',"; fi)
  $(if [[ "$CONTROL_VARIANT" -eq 1 ]]; then echo ''; else echo "ssl_key_file := '${KEY}',"; fi)
  allow_other_hostname := true);"
PLAIN_SQL="LOAD quack;
LOAD httpfs;
CALL quack_serve('quack:${HOST}:${PLAIN_PORT}', token := '${TOKEN}',
  disable_ssl := true, allow_other_hostname := true);"

start_server tls "$TLS_PORT" "$TLS_SQL"
start_server plain "$PLAIN_PORT" "$PLAIN_SQL"

# --- 4. the product's own boot SQL is still plaintext ---------------------
# Honesty guard: this audit must never be read as "quack-proxy has TLS".
BOOT_SQL="$REPO_ROOT/internal/supervisor/supervisor.go"
grep -q 'disable_ssl = true' "$BOOT_SQL" \
  || fail "product boot SQL no longer hardcodes disable_ssl=true; update R-05 docs, verify-plaintext.sh and this gate together"
if [[ -f "$REPO_ROOT/internal/config/config.go" ]]; then
  grep -q 'not implemented' "$REPO_ROOT/internal/config/config.go" \
    || true   # informational only; plaintext-audit owns that assertion
fi
echo "PRODUCT_DISABLES_ENGINE_TLS=1 (supervisor.go hardcodes disable_ssl = true)"

# --- 5. URL scheme is the first capability discriminator ------------------
TLS_URL="$(tr -d ' ' < "$WORK/tls.log" | grep -o 'https\?://[^│]*' | head -1 || true)"
PLAIN_URL="$(tr -d ' ' < "$WORK/plain.log" | grep -o 'https\?://[^│]*' | head -1 || true)"
echo "TLS_LISTEN_URL=${TLS_URL:-<none>}"
echo "PLAIN_LISTEN_URL=${PLAIN_URL:-<none>}"
[[ "$PLAIN_URL" == http://* ]] \
  || fail "plaintext control did not report an http:// URL (got '${PLAIN_URL}')"
[[ "$TLS_URL" == https://* ]] \
  || fail "TLS endpoint did not report an https:// URL (got '${TLS_URL}')"
echo "ENGINE_URL_SCHEME_SWITCH=1 (disable_ssl true->http, false->https)"

# --- 6. independent TLS handshake + plaintext rejection -------------------
HANDSHAKE="$(echo | openssl s_client -connect "${HOST}:${TLS_PORT}" 2>/dev/null || true)"
grep -q 'TLSv1' <<<"$HANDSHAKE" \
  || fail "no TLS handshake from openssl s_client on port ${TLS_PORT}"
grep -q 'Cipher is' <<<"$HANDSHAKE" \
  || fail "openssl s_client completed no cipher negotiation"
echo "TLS_HANDSHAKE_OK=1 ($(grep -m1 -o 'TLSv1\.[0-9]' <<<"$HANDSHAKE" || echo TLSv1))"

PLAIN_CODE="$(curl -sS -m 6 -o "$WORK/plain.body" -w '%{http_code}' \
  "http://${HOST}:${TLS_PORT}/" 2>/dev/null || true)"
[[ "$PLAIN_CODE" == "000" || "$PLAIN_CODE" == "" ]] \
  || fail "plaintext HTTP was accepted on the TLS port (code ${PLAIN_CODE})"
echo "PLAINTEXT_REJECTED_ON_TLS_PORT=1"

TLS_CODE="$(curl -skS -m 6 -o "$WORK/tls.body" -w '%{http_code}' \
  "https://${HOST}:${TLS_PORT}/" 2>/dev/null || true)"
[[ "$TLS_CODE" =~ ^2 ]] || fail "HTTPS GET failed on TLS port (code ${TLS_CODE})"
echo "HTTPS_GET_OK=1 (HTTP ${TLS_CODE})"

# --- 7. Quack client with the pinned fingerprint --------------------------
client_query() {
  local extra="$1"
  duck timeout 30 "$D" -csv -c "
LOAD quack;
SELECT * FROM quack_query('quack:${HOST}:${TLS_PORT}', 'SELECT 42 AS answer',
  token := '${TOKEN}'${extra});" 2>&1
}

GOOD="$(client_query ", ssl_fingerprint := '${FP_HEX}'" || true)"
grep -q '^42$' <<<"$GOOD" \
  || fail "client with the correct ssl_fingerprint did not get the query result: ${GOOD}"
echo "FINGERPRINT_PINNED_QUERY_OK=1 (SELECT 42 -> 42 over TLS)"

BAD_FP="$(client_query ", ssl_fingerprint := '0000000000000000000000000000000000000000000000000000000000000000'" || true)"
grep -qi 'does not match the pinned ssl_fingerprint' <<<"$BAD_FP" \
  || fail "a wrong fingerprint was NOT rejected - the fingerprint assertion is not checking: ${BAD_FP}"
echo "WRONG_FINGERPRINT_REJECTED=1"

NO_FP="$(client_query "" || true)"
[[ -z "$(grep -E '^42$' <<<"$NO_FP")" ]] \
  || fail "client with no fingerprint unexpectedly succeeded: ${NO_FP}"
echo "MISSING_FINGERPRINT_REJECTED=1"

BAD_TOKEN="$(duck timeout 30 "$D" -csv -c "
LOAD quack;
SELECT * FROM quack_query('quack:${HOST}:${TLS_PORT}', 'SELECT 42 AS answer',
  token := 'definitely-the-wrong-token', ssl_fingerprint := '${FP_HEX}');" 2>&1 || true)"
[[ -z "$(grep -E '^42$' <<<"$BAD_TOKEN")" ]] \
  || fail "a wrong token was accepted over TLS: ${BAD_TOKEN}"
echo "WRONG_TOKEN_REJECTED_OVER_TLS=1"

# The plaintext control must still answer: otherwise every rejection above
# could be explained by a dead endpoint rather than by TLS.
PLAIN_GOOD="$(duck timeout 30 "$D" -csv -c "
LOAD quack;
SELECT * FROM quack_query('quack:${HOST}:${PLAIN_PORT}', 'SELECT 42 AS answer',
  token := '${TOKEN}', disable_ssl := true);" 2>&1 || true)"
grep -q '^42$' <<<"$PLAIN_GOOD" \
  || fail "plaintext control endpoint did not answer (so the TLS negatives are meaningless): ${PLAIN_GOOD}"
echo "PLAINTEXT_CONTROL_ANSWERS=1 (negative controls are meaningful)"

# --- 8. negative control variant ------------------------------------------
# If --control-variant is passed, the TLS endpoint was started with
# disable_ssl := true. Reaching here means the URL-scheme assertion somehow
# accepted an http:// URL, so the whole gate must fail loudly. This proves the
# discriminator is live rather than a tautology.
if [[ "$CONTROL_VARIANT" -eq 1 ]]; then
  fail "control variant reached PASS: an http:// endpoint was accepted as TLS"
fi

echo
echo "ENGINE_TLS_CAPABILITY_PASSED"
echo "  Evidence about the PINNED ENGINE ONLY. quack-proxy still serves plaintext"
echo "  HTTP (disable_ssl=true hardcoded); R-05 remains OPEN. Enabling TLS in the"
echo "  product requires a design decision on certificates, fingerprint delivery,"
echo "  health checks, status.json and HAProxy - not an engine implementation."
echo "  See docs/REMEDIATION.md R-05 and docs/SECURITY.md."
