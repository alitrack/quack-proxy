#!/usr/bin/env python3
"""RBAC v2 × MIMIC real-data E2E — WSL client -> Mac shard (19551).
Real clinical tables: viewer = terminology only, analyst = + clinical (RO), etl = RW.
Legacy RBAC meta tables (quack_tokens etc.) must be protected."""
import duckdb, sys

EP = "quack:10.10.10.81:19551"

def case(name, token, sql, expect):
    try:
        c = duckdb.connect()
        c.execute("LOAD quack")
        val = c.execute("FROM quack_query(?, ?, token := ?, disable_ssl := true)", [EP, sql, token]).fetchall()
        got, out = 'ok', str(val)[:60]
    except Exception as e:
        msg = str(e)
        got = 'deny' if ('Authorization' in msg or 'Authentication' in msg
                         or 'does not exist' in msg) else f"ERR:{msg[:60]}"
        out = ''
    verdict = "PASS" if got == expect else "FAIL"
    print(f"{verdict:4} {name:38} expect={expect:4} got={got:6} {out}")
    return verdict == "PASS"

r = []
# viewer: terminology only, RO
r.append(case("viewer read concept_std", "viewer-tok", "SELECT count(*) FROM concept_std", "ok"))
r.append(case("viewer read mimic_icd_map", "viewer-tok", "SELECT count(*) FROM mimic_icd_map", "ok"))
r.append(case("viewer read pri_dx (deny)", "viewer-tok", "SELECT count(*) FROM pri_dx", "deny"))
r.append(case("viewer write concept_std (deny)", "viewer-tok", "INSERT INTO concept_std VALUES (0,'x','x','x')", "deny"))
# analyst: + clinical, RO
r.append(case("analyst read pri_dx", "analyst-tok", "SELECT count(*) FROM pri_dx", "ok"))
r.append(case("analyst read p4sig", "analyst-tok", "SELECT count(*) FROM p4sig", "ok"))
r.append(case("analyst write pri_dx (deny)", "analyst-tok", "INSERT INTO pri_dx VALUES ('hack')", "deny"))
# etl: RW
r.append(case("etl read pri_dx", "etl-tok", "SELECT count(*) FROM pri_dx", "ok"))
# legacy RBAC meta protected (quack_tokens may hold REAL tokens from Nov experiments)
r.append(case("any read quack_tokens (deny)", "etl-tok", "SELECT * FROM quack_tokens", "deny"))
r.append(case("any read role_perms (deny)", "analyst-tok", "SELECT * FROM role_perms", "deny"))
r.append(case("any read rbac_audit (deny)", "etl-tok", "SELECT count(*) FROM rbac2m_audit", "deny"))
# unknown table default-deny
r.append(case("unknown table (deny)", "etl-tok", "SELECT * FROM pg_catalog.pg_tables", "deny"))
# real clinical query through the whole chain
r.append(case("analyst clinical join", "analyst-tok",
    "SELECT count(*) FROM pri_dx p JOIN mimic_icd_map m ON p.icd_code = m.icd_code", "ok"))

print()
print(f"MIMIC E2E: {sum(r)}/{len(r)} PASS")
sys.exit(0 if all(r) else 1)
