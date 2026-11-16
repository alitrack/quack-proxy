#!/usr/bin/env python3
"""E2E matrix for RBAC v2 (scoped tokens + audit ring) — 2026-11-16.
Verified client path: quack_query() one-shot (ATTACH path hits the
DuckLake-style nested-catalog quack issue; quackhole-rbac's own matrix
uses quack_query too).
"""
import duckdb, sys

EP = "quack:127.0.0.1:19501"

def case(name, token, sql, expect):
    try:
        c = duckdb.connect()
        c.execute("LOAD quack")
        val = c.execute(
            "FROM quack_query(?, ?, token := ?)", [EP, sql, token]).fetchall()
        got, out = 'ok', str(val)[:50]
    except Exception as e:
        msg = str(e)
        got = 'deny' if ('Authorization' in msg or 'Authentication' in msg) else f"ERR:{msg[:60]}"
        out = ''
    verdict = "PASS" if got == expect else "FAIL"
    print(f"{verdict:4} {name:32} expect={expect:4} got={got:6} {out}")
    return verdict == "PASS"

results = []
# viewer: RO ceiling, public only
results.append(case("viewer read public",        "viewer-tok", "SELECT * FROM public_data", "ok"))
results.append(case("viewer read secret (deny)", "viewer-tok", "SELECT * FROM secret_data", "deny"))
results.append(case("viewer write (ceiling RO)", "viewer-tok", "INSERT INTO public_data VALUES (9,'x')", "deny"))
results.append(case("viewer CTE-write bypass",   "viewer-tok", "WITH x AS (SELECT 1) INSERT INTO public_data VALUES (9,'x')", "deny"))
results.append(case("viewer tables-as-write",    "viewer-tok", "CREATE TABLE hack(a INT)", "deny"))
# analyst: RO ceiling, both tables readable
results.append(case("analyst read secret",       "analyst-tok", "SELECT * FROM secret_data", "ok"))
results.append(case("analyst write (ceiling RO)", "analyst-tok", "INSERT INTO secret_data VALUES (9,'x')", "deny"))
# etl: RW ceiling
results.append(case("etl write public",          "etl-tok", "INSERT INTO public_data VALUES (9,'x')", "ok"))
results.append(case("etl read secret",           "etl-tok", "SELECT * FROM secret_data", "ok"))
# bad token
results.append(case("unknown token",             "bogus-tok", "SELECT * FROM public_data", "deny"))
# unclassified (fail-closed)
results.append(case("SET blocked (fail-closed)", "viewer-tok", "SET GLOBAL memory_limit='1GB'", "deny"))

print()
print(f"E2E: {sum(results)}/{len(results)} PASS")
sys.exit(0 if all(results) else 1)
