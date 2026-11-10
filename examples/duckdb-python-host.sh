#!/bin/bash
# CLI-compat wrapper: quack-proxy invokes this like the duckdb CLI.
# Delegates to the python-hosted DuckDB 2.0.0 stable engine (see
# duckdb_serve_host.py's header for the version-matrix rationale).
# Stdin passes through untouched.
#
# Resolves the .py next to this script, so it works from any checkout;
# override the interpreter with $PYTHON if your venv lives elsewhere.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "${PYTHON:-python3}" "$SCRIPT_DIR/duckdb_serve_host.py" "$@"
