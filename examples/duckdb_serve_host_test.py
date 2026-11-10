#!/usr/bin/env python3
"""Unit tests for the statement splitter in duckdb_serve_host.py.

Pure logic — no duckdb import needed. Run with `python3 duckdb_serve_host_test.py`.

Contract: each emitted statement includes its terminating ';' (that is a
complete executable unit for con.execute); a fragment without a terminator
stays in the tail.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from duckdb_serve_host import _scan, split_statements  # noqa: E402


def test_plain_statements():
    assert split_statements("SELECT 1; SELECT 2") == ["SELECT 1;", "SELECT 2"]


def test_semicolon_inside_single_quote():
    got = split_statements("SELECT 'a;b'; SELECT 2")
    assert got == ["SELECT 'a;b';", "SELECT 2"], got


def test_semicolon_inside_dquote():
    got = split_statements('SELECT "a;b"; SELECT 2')
    assert got == ["SELECT \"a;b\";", "SELECT 2"], got


def test_escaped_single_quote():
    got = split_statements("SELECT 'it''s'; SELECT 2")
    assert got == ["SELECT 'it''s';", "SELECT 2"], got


def test_dollar_quoted_tag():
    got = split_statements("SELECT $lua$ x; y $lua$ AS f; SELECT 2")
    assert got == ["SELECT $lua$ x; y $lua$ AS f;", "SELECT 2"], got


def test_empty_dollar_tag():
    # $$ is a legal empty tag in DuckDB; the inner ';' must not split.
    got = split_statements("SELECT $$a;b$$ AS x; SELECT 2")
    assert got == ["SELECT $$a;b$$ AS x;", "SELECT 2"], got


def test_tag_with_underscore():
    got = split_statements("SELECT $rbac_tag$a;$rbac_tag$ AS x; SELECT 2")
    assert got == ["SELECT $rbac_tag$a;$rbac_tag$ AS x;", "SELECT 2"], got


def test_line_comment_with_semicolon():
    # A line comment runs to the newline; 'SELECT 2' is inside the comment,
    # so the whole line is ONE statement — the ';' must not split.
    got = split_statements("SELECT 1 -- note; SELECT 2")
    assert len(got) == 1, got
    assert got[0].startswith("SELECT 1 -- note"), got


def test_block_comment_with_semicolon():
    got = split_statements("/* note; */ SELECT 2")
    assert len(got) == 1 and got[0].endswith("SELECT 2"), got


def test_nested_block_comment():
    # DuckDB nests block comments; only the matching close ends it, so the
    # ';' inside the inner comment must not split.
    got = split_statements("/* a /* b */ ; */ SELECT 2; SELECT 3")
    assert len(got) == 2, got
    assert got[0].endswith("SELECT 2;") and got[1] == "SELECT 3", got


def test_multiline_lua_module_shape():
    # The real-world case: a luajit_module source with comments, a
    # semicolon, and a dollar tag, spanning lines, then quack_serve.
    src = (
        "SELECT * FROM luajit_module(mode := 'compile', sql_name := 'rbac', source := $rbac$\n"
        "-- RBAC policy; runs at boot\n"
        "local t = {'a;b'}\n"
        "/* /* nested */ still comment; */\n"
        "return function(p) return true end\n"
        "$rbac$);\n"
        "CALL quack_serve('quack:0.0.0.0:9491', token = 'tk');\n"
    )
    got = split_statements(src)
    # Exactly two statements: the big module SELECT and quack_serve.
    assert len(got) == 2, f"expected 2 statements, got {len(got)}:\n{got}"
    assert got[0].startswith("SELECT * FROM luajit_module")
    assert "return function(p) return true end" in got[0]
    assert got[0].endswith("$rbac$);"), got[0][-20:]
    assert got[1].startswith("CALL quack_serve") and got[1].endswith("token = 'tk');"), got[1]


def test_incomplete_tail_is_not_emitted():
    # A statement not yet terminated (no ';') stays in the tail.
    stmts, tail = _scan("SELECT 1; SELECT 2", yield_tail=False)
    assert stmts == ["SELECT 1;"], stmts
    assert tail.strip() == "SELECT 2", repr(tail)


def test_unterminated_dollar_quote_holds():
    # An unterminated dollar quote must not end a statement on ';' and must
    # stay in the tail (the real engine keeps waiting).
    stmts, tail = _scan("SELECT $$abc; def", yield_tail=False)
    assert stmts == [], stmts
    assert tail.strip().startswith("SELECT $$abc"), repr(tail)


def test_nothing_lost_verbatim():
    # Round-trip: joining the emitted statements + tail must retain the
    # payload — nothing is lost or reordered.
    src = "SELECT 'a'; SELECT $$b;c$$; SELECT /* d; */ 'e'"
    stmts, tail = _scan(src, yield_tail=False)
    reassembled = "\n".join(stmts) + (tail or "")
    assert "b;c" in reassembled, reassembled
    assert "d;" in reassembled, reassembled
    assert reassembled.count("SELECT") == 3, reassembled


if __name__ == "__main__":
    failures = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn()
                print(f"ok   {name}")
            except AssertionError as e:
                failures += 1
                print(f"FAIL {name}: {e}")
    if failures:
        sys.exit(f"{failures} test(s) failed")
    print("all tests passed")
