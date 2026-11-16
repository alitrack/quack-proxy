#!/usr/bin/env bash
# Prefetch the pinned DuckDB CLI + signed core `quack` extension into an
# offline cache for scripts/quack-e2e.sh (r10).
#
# Why this exists: the real-engine E2E used to fetch GitHub Releases and the
# DuckDB extension CDN from inside the test process on EVERY run. Both have
# returned transient 5xx / early-EOF, which is why quack-e2e stayed advisory
# on pull requests. The artifacts are version-pinned and content-verified, so
# they can be fetched ONCE on the CI runner and then reused strictly offline.
# A cache hit performs ZERO network requests; see QUACK_E2E_OFFLINE in the
# E2E script and the `quack-e2e` job in .github/workflows/ci.yml.
#
# Supply chain: this script NEVER uses -unsigned and NEVER touches the
# community extension repository. DuckDB verifies the core extension
# signatures on LOAD; the SHA256 pins below additionally detect a truncated
# or corrupted cache entry. All pins were verified against the real artifacts
# (DuckDB v2.0.0 Cyanoptera, linux_amd64) before being written here.
#
# httpfs is pinned too: without it, a disk-backed DuckDB v2.0.0 shard fails
# offline with "read-only crypto module loaded" (see the constant below).
#
# Usage:
#   bash scripts/prefetch-quack-e2e.sh --cache-dir /path/to/cache
#   QUACK_PREFETCH_OFFLINE=1 bash scripts/prefetch-quack-e2e.sh --cache-dir /path \
#       # assert the cache is already complete; NEVER touch the network
#
# Exit: 0 = cache complete and verified, non-zero = first failed check.
set -euo pipefail

DUCKDB_VERSION="v2.0.0"
CLI_FILE="duckdb-cli-linux-amd64.tar.gz"
CLI_URL="https://github.com/duckdb/duckdb/releases/download/${DUCKDB_VERSION}/${CLI_FILE}"
CLI_SHA256="c6507206baf7793c6013dbd82368469a45f20dc0e17a6f92f204602f652ed2bc"

EXT_GZ_FILE="quack.duckdb_extension.gz"
EXT_FILE="quack.duckdb_extension"
# v2.0.0 uses `linux_amd64` (the older `linux_amd64_gcc4` path returns 404).
EXT_URL="https://extensions.duckdb.org/${DUCKDB_VERSION}/linux_amd64/${EXT_GZ_FILE}"
EXT_GZ_SHA256="f9a5c0d2e0eef0f7546aba4579ab80d75d167cdf48830459faf67378ab2000fe"
EXT_SHA256="9c893bd543ec2c5253d0c47d5614f90a1bfa1791c735dc6bbcea4e8bd3811a97"

# DuckDB v2.0.0 writes encrypted database files with a READ-ONLY crypto
# module unless httpfs (which brings the full crypto backend) is loadable.
# With an isolated HOME, autoload cannot fetch it, so a disk-backed shard
# fails with "read-only crypto module loaded". r9's "offline" E2E actually
# still had network and let DuckDB autoload httpfs on demand; r10 pins it.
# The file only needs to be PRESENT in the extension dir - DuckDB autoloads
# it - so quack-proxy's boot SQL stays untouched.
HTTPFS_GZ_FILE="httpfs.duckdb_extension.gz"
HTTPFS_FILE="httpfs.duckdb_extension"
HTTPFS_URL="https://extensions.duckdb.org/${DUCKDB_VERSION}/linux_amd64/${HTTPFS_GZ_FILE}"
HTTPFS_GZ_SHA256="ee6c7d7e60a5da1b6f6bf6156b4298b55ddce0496a5bfe628bd6ab6d47eb8e3e"
HTTPFS_SHA256="77e6c429aa04f25da7962572f12b19113e0ce46080fa1365ad66017aadcdb68d"

# Where DuckDB looks under $HOME/.duckdb/extensions/. Verified by querying
# duckdb_extensions().install_path after an offline LOAD (r10).
EXT_RELPATH="${DUCKDB_VERSION}/linux_amd64"
MANIFEST="quack-e2e-manifest.txt"

CACHE_DIR="${QUACK_PREFETCH_CACHE_DIR:-}"
OFFLINE_ONLY="${QUACK_PREFETCH_OFFLINE:-0}"

usage() {
  awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "${BASH_SOURCE[0]}"
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cache-dir) CACHE_DIR="$2"; shift 2 ;;
    -h|--help)   usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 1 ;;
  esac
done

[[ -n "$CACHE_DIR" ]] || { echo "FAIL: --cache-dir is required" >&2; usage 1; }
mkdir -p "$CACHE_DIR"

sha_is() {
  local file="$1" want="$2"
  [[ -f "$file" ]] || return 1
  printf '%s  %s\n' "$want" "$file" | sha256sum -c - >/dev/null 2>&1
}

verify_cache() {
  sha_is "$CACHE_DIR/$CLI_FILE" "$CLI_SHA256" || return 1
  sha_is "$CACHE_DIR/$EXT_FILE" "$EXT_SHA256" || return 1
  sha_is "$CACHE_DIR/$HTTPFS_FILE" "$HTTPFS_SHA256" || return 1
  [[ -f "$CACHE_DIR/$MANIFEST" ]] || return 1
  grep -qx "extension_relpath=${EXT_RELPATH}" "$CACHE_DIR/$MANIFEST" || return 1
  grep -qx "cli_sha256=${CLI_SHA256}" "$CACHE_DIR/$MANIFEST" || return 1
  grep -qx "ext_sha256=${EXT_SHA256}" "$CACHE_DIR/$MANIFEST" || return 1
  grep -qx "httpfs_sha256=${HTTPFS_SHA256}" "$CACHE_DIR/$MANIFEST" || return 1
  return 0
}

if verify_cache; then
  echo "PREFETCH_CACHE_HIT: ${CACHE_DIR} (verified, no network used)"
  exit 0
fi

if [[ "$OFFLINE_ONLY" == "1" ]]; then
  echo "FAIL: QUACK_PREFETCH_OFFLINE=1 but the pinned cache is incomplete or corrupt" >&2
  echo "      expected ${CLI_FILE}, ${EXT_FILE}, ${HTTPFS_FILE} and ${MANIFEST} in ${CACHE_DIR}" >&2
  exit 1
fi

echo "==> cache miss; prefetching pinned ${DUCKDB_VERSION} artifacts into ${CACHE_DIR}"

fetch_with_retry() {
  local url="$1" dest="$2" want="$3" label="$4" attempt
  # Reuse an already-complete download instead of refetching a large asset.
  # The httpfs object in particular has repeatedly truncated near the end;
  # a verified local copy makes a retry cheap and deterministic.
  if sha_is "$dest" "$want"; then
    echo "==> ${label}: already present and verified (no download)"
    return 0
  fi
  for attempt in 1 2 3; do
    if curl -fsSL --retry 3 --retry-delay 3 --connect-timeout 20 -m 300 \
      -o "${dest}.part" "$url" && sha_is "${dest}.part" "$want"; then
      mv -f "${dest}.part" "$dest"
      return 0
    fi
    echo "==> ${label} fetch/verify attempt ${attempt} failed; retrying" >&2
    rm -f "${dest}.part" || true
    sleep 3
  done
  echo "FAIL: cannot fetch or verify ${label} from ${url}" >&2
  echo "      expected sha256 ${want}" >&2
  ls -l "${dest}.part" >&2 2>/dev/null || true
  return 1
}

fetch_with_retry "$CLI_URL" "$CACHE_DIR/$CLI_FILE" "$CLI_SHA256" "pinned DuckDB CLI"
fetch_with_retry "$EXT_URL" "$CACHE_DIR/$EXT_GZ_FILE" "$EXT_GZ_SHA256" "signed core quack extension"
fetch_with_retry "$HTTPFS_URL" "$CACHE_DIR/$HTTPFS_GZ_FILE" "$HTTPFS_GZ_SHA256" "signed core httpfs extension"

# Decompress to the exact filenames DuckDB expects, then verify the unpacked
# bytes too (a truncated gzip could otherwise slip past a compressed pin).
unpack_and_verify() {
  local gz="$1" out="$2" want="$3"
  if ! gunzip -c "$gz" > "${out}.part" 2>/dev/null; then
    echo "FAIL: cannot gunzip ${gz}" >&2
    return 1
  fi
  if ! sha_is "${out}.part" "$want"; then
    echo "FAIL: unpacked ${out} sha256 mismatch" >&2
    return 1
  fi
  mv -f "${out}.part" "$out"
}
unpack_and_verify "$CACHE_DIR/$EXT_GZ_FILE" "$CACHE_DIR/$EXT_FILE" "$EXT_SHA256"
unpack_and_verify "$CACHE_DIR/$HTTPFS_GZ_FILE" "$CACHE_DIR/$HTTPFS_FILE" "$HTTPFS_SHA256"

cat > "${CACHE_DIR}/${MANIFEST}.part" <<EOF
duckdb_version=${DUCKDB_VERSION}
cli_sha256=${CLI_SHA256}
ext_gz_sha256=${EXT_GZ_SHA256}
ext_sha256=${EXT_SHA256}
httpfs_gz_sha256=${HTTPFS_GZ_SHA256}
httpfs_sha256=${HTTPFS_SHA256}
extension_relpath=${EXT_RELPATH}
cli_url=${CLI_URL}
ext_url=${EXT_URL}
httpfs_url=${HTTPFS_URL}
EOF
mv -f "${CACHE_DIR}/${MANIFEST}.part" "$CACHE_DIR/$MANIFEST"

if ! verify_cache; then
  echo "FAIL: cache did not verify after prefetch" >&2
  exit 1
fi
echo "PREFETCH_VERIFIED: ${CACHE_DIR} (cli ${CLI_SHA256:0:12}… / quack ${EXT_SHA256:0:12}… / httpfs ${HTTPFS_SHA256:0:12}…)"
