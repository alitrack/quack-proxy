#!/bin/bash
# CLI-compat wrapper: quack-proxy invokes this like the duckdb CLI.
# Delegates to the python-hosted DuckDB 2.0.0 stable engine (see .py file
# header for the version-matrix rationale). Stdin passes through untouched.
exec /home/lhy/watch-poc/.venv/bin/python3 duckdb_serve_host.py "$@"
