#!/usr/bin/env python3
"""DuckDB 2.0.0 stable serve host for quack-proxy shards.

Why this exists (verified 2026-11-09):
- duckdb CLI v2.0.0-alpha39998 has BROKEN quack token auth (rejects even
  the correct server token)
- duckdb CLI v2.0.0-alpha48730 has working auth but NO luajit community
  extension build (404)
- python duckdb 2.0.0 stable has BOTH working — it is the only viable
  shard host engine until a stable CLI ships.

CLI-compatible surface for quack-proxy's supervisor:
    duckdb_stable_serve.py [-unsigned] DATABASE   (SQL arrives on stdin)

Incremental & statement-aware: stdin never hits EOF (the supervisor holds
it open with `tail -f /dev/null`), so statements are executed AS THEY
COMPLETE while streaming; the splitter understands $tag$ dollar-quoting,
single/double quotes, '--' and /* */ comments — a multi-line
luajit_module(...) source passes through verbatim.
"""
import sys
import time


def _scan(text, yield_tail):
    """State-machine scan. Returns (statements, incomplete_tail).

    Tracks the DuckDB lexer's rules (verified against duckdb 2.0.0,
    2026-11-10): '$' + any alnum run (including the empty tag '$$') opens a
    dollar-quote; block comments NEST (depth counted, '/*' inside a comment
    increments); a ';' ends a statement only outside quotes, dollar-quotes
    and comments.
    """
    stmts = []
    buf = []
    dollar_tag = None
    in_squote = in_dquote = False
    in_line_comment = False
    block_depth = 0
    i, n = 0, len(text)
    while i < n:
        ch = text[i]
        nxt = text[i + 1] if i + 1 < n else ""
        if in_line_comment:
            buf.append(ch)
            if ch == "\n":
                in_line_comment = False
            i += 1
            continue
        if block_depth > 0:
            buf.append(ch)
            if ch == "/" and nxt == "*":  # nested open
                buf.append(nxt)
                block_depth += 1
                i += 2
                continue
            if ch == "*" and nxt == "/":
                buf.append(nxt)
                block_depth -= 1
                i += 2
                continue
            i += 1
            continue
        if dollar_tag is None and not in_squote and not in_dquote:
            if ch == "-" and nxt == "-":
                in_line_comment = True
                buf.append(ch)
                i += 1
                continue
            if ch == "/" and nxt == "*":
                block_depth = 1
                buf.append(ch)
                i += 1
                continue
            if ch == "'":
                in_squote = True
            elif ch == '"':
                in_dquote = True
            elif ch == "$":
                j = i + 1
                while j < n and (text[j].isalnum() or text[j] == "_"):
                    j += 1
                # j == i + 1 covers the empty tag $$
                if j < n and text[j] == "$" and j >= i + 1:
                    dollar_tag = text[i : j + 1]
                    buf.append(dollar_tag)
                    i = j + 1
                    continue
        elif dollar_tag is not None:
            if text.startswith(dollar_tag, i):
                buf.append(dollar_tag)
                i += len(dollar_tag)
                dollar_tag = None
                continue
        elif in_squote:
            if ch == "'":
                if nxt == "'":  # escaped ''
                    buf.append("''")
                    i += 2
                    continue
                in_squote = False
        elif in_dquote:
            if ch == '"':
                in_dquote = False
        buf.append(ch)
        if (
            ch == ";"
            and dollar_tag is None
            and not in_squote
            and not in_dquote
            and not in_line_comment
            and block_depth == 0
        ):
            stmt = "".join(buf).strip()
            if stmt:
                stmts.append(stmt)
            buf = []
        i += 1
    return stmts, "".join(buf)


def split_statements(text):
    """One-shot split for tests: complete statements + trailing fragment."""
    stmts, tail = _scan(text, yield_tail=True)
    out = list(stmts)
    if tail.strip():
        out.append(tail.strip())
    return out


def main() -> None:
    import duckdb  # lazy: the splitter above is testable without the engine

    args = [a for a in sys.argv[1:] if a != "-unsigned"]
    db = args[0] if args else ":memory:"
    con = duckdb.connect(db)
    pending = ""
    done = 0
    # readline loop (NOT `for line in sys.stdin:` — the iterator blocked
    # indefinitely under the supervisor's tail-held pipe in testing)
    while True:
        line = sys.stdin.readline()
        if not line:  # EOF — not expected with supervisor; hold for quack
            break
        pending += line
        stmts, pending = _scan(pending, yield_tail=False)
        for stmt in stmts:
            done += 1
            try:
                con.execute(stmt)
            except Exception as e:  # noqa: BLE001 — boot must survive log noise
                print(f"boot-sql error: {e}", file=sys.stderr, flush=True)
    # stdin EOF (not expected with supervisor) — hold process for quack
    while True:
        time.sleep(3600)


if __name__ == "__main__":
    main()
