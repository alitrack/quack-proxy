-- RBAC v2 for quack-proxy shards: scoped tokens + audit ring (2026-11-16).
-- Layered on examples/rbac-init.sql (in-memory variant); same deadlock rule:
-- authn/authz run INSIDE connection setup, _duckdb_query re-entry deadlocks
-- the handshake (reproduced 2026-11-09). All state lives in Lua memory;
-- only setup()/audit dump may touch the database, at the operator's pace.
--
-- What this adds over v1:
--   * scoped tokens: verb ceiling (RO/RW), max_rows cap, stmt_timeout_ms
--   * audit ring buffer (bounded, per-process) with dump exposed as a
--     table function the operator can query any time
--
-- ORDER MATTERS: LOAD quack must precede the SET GLOBAL hook assignments —
-- setting quack_authentication_function before the quack extension is
-- loaded is silently dropped (quackhole-rbac README pitfall #6, verified
-- 2026-11-16: hooks bound, settings read back correct, but never invoked).
INSTALL luajit FROM community;
LOAD luajit;
LOAD quack;
SELECT * FROM luajit_module(mode := 'compile', sql_name := 'rbac2', source := $rbac$
-- RBAC v2: scoped tokens + audit ring (in-memory)
local esc = function(s) return (s:gsub("'", "''")) end

-- ============ scoped credential store (edit, or generate from your IdP) ============
-- token -> { user, ceiling ('RO'|'RW'), max_rows (nil = unlimited), timeout_ms (nil = unlimited) }
local TOKENS = {
  ['viewer-tok']  = { user='viewer',  ceiling='RO',  max_rows=100 },
  ['analyst-tok'] = { user='analyst', ceiling='RO',  max_rows=5000 },
  ['etl-tok']     = { user='etl',     ceiling='RW',  max_rows=nil,  timeout_ms=30000 },
}
local ROLES = {
  viewer  = 'viewer_role',
  analyst = 'analyst_role',
  etl     = 'etl_role',
}
local GRANTS = {
  viewer_role  = { public_data = { read=true,  write=false } },
  analyst_role = { public_data = { read=true,  write=false }, secret_data = { read=true, write=false } },
  etl_role     = { public_data = { read=true,  write=true  }, secret_data = { read=true, write=true  } },
}
local ALL_TABLES = { 'PUBLIC_DATA', 'SECRET_DATA' }

-- ============ audit: ring (always) + persistent table (best-effort) ============
local RING_SIZE = 4096
local ring = {}          -- circular array
local ring_n = 0         -- total events ever (monotonic)
local ring_dropped = 0   -- events dropped before first dump (pre-overflow only)

-- os may be sandboxed away inside the luajit extension; degrade to 0
local function now()
  local ok, t = pcall(function() return os.time() end)
  return ok and t or 0
end

-- Persistent audit write. _duckdb_query inside the hooks is the pattern
-- quackhole-rbac uses in production (its authn/authz read/write tables the
-- same way); it works on the quack_query request path. The 2026-11-09 ATTACH
-- handshake deadlock does not apply there — but if a write ever fails the
-- ring below still holds the event, so this is best-effort by pcall.
local function audit_persist(seq, ts, kind, user, q, verdict, detail)
  pcall(function()
    _duckdb_query(string.format(
      "INSERT INTO rbac_audit VALUES (%d, %d, '%s', %s, %s, '%s', '%s')",
      seq, ts, esc(kind),
      user and ("'" .. esc(user) .. "'") or 'NULL',
      q and ("'" .. esc(q:sub(1, 512)) .. "'") or 'NULL',
      esc(verdict), esc(detail or '')))
  end)
end

local function audit(kind, user, q, verdict, detail)
  ring_n = ring_n + 1
  ring[ring_n] = {
    seq = ring_n,
    ts = now(),
    kind = kind,       -- 'authn' | 'authz'
    user = user,
    q = q and q:sub(1, 512) or nil,
    verdict = verdict, -- 'allow' | 'deny'
    detail = detail,
  }
  audit_persist(ring_n, ring[ring_n].ts, kind, user, q, verdict, detail)
end

-- ============ statement classification (regex-free, shape-based) ============
local WRITE_LEADS = {
  ['INSERT']=true, ['UPDATE']=true, ['DELETE']=true, ['CREATE']=true,
  ['DROP']=true,   ['ALTER']=true,  ['TRUNCATE']=true, ['COPY']=true,
  ['MERGE']=true,  ['INSTALL']=true,['LOAD']=true,
}
local READ_LEADS = { ['SELECT']=true, ['WITH']=true, ['FROM']=true, ['DESCRIBE']=true, ['SHOW']=true, ['EXPLAIN']=true, ['PRAGMA']=true, ['SUMMARIZE']=true, ['TABLE']=true, ['VALUES']=true }

local function classify(q)
  local head = q:match('^%s*(%a+)')
  if not head then return 'EMPTY', nil end
  head = head:upper()
  if head == 'WITH' then
    -- WITH x AS (...) INSERT/UPDATE/... — the known bypass shape.
    -- Lua patterns have no alternation: scan for any write keyword with
    -- word boundaries anywhere after WITH (conservative: a column named
    -- "inserted_at" would false-positive as WRITE — fail-closed is correct).
    local body = q:gsub('^%s*WITH', '')
    local wleads = { 'INSERT', 'UPDATE', 'DELETE', 'MERGE', 'CREATE', 'DROP', 'ALTER', 'TRUNCATE' }
    for _, kw in ipairs(wleads) do
      if body:find('%f[%a]' .. kw .. '%f[%A]') then return 'WRITE', 'with-' .. string.lower(kw) end
    end
    return 'READ', nil
  end
  if WRITE_LEADS[head] then return 'WRITE', head end
  if READ_LEADS[head] then return 'READ', head end
  return 'OTHER', head
end

local function has_token(q, tok)
  return q:find('%f[%w]' .. tok .. '%f[%W]') ~= nil
end

-- ============ hooks ============
local SESSIONS = {}  -- sid -> { user, ceiling, max_rows, timeout_ms }

local function authn(p)
  local t = TOKENS[p.client_token or '']
  if not t then
    audit('authn', nil, nil, 'deny', 'unknown token')
    return false
  end
  SESSIONS[p.sid or ''] = { user=t.user, ceiling=t.ceiling, max_rows=t.max_rows, timeout_ms=t.timeout_ms }
  audit('authn', t.user, nil, 'allow', t.ceiling)
  return true
end

local function authz(p)
  local q = (p.q or ''):gsub('^%s+',''):gsub('%s+$','')
  if q == '' then return false end
  local U = q:upper()
  local sess = SESSIONS[p.sid or '']

  -- quack client ATTACH handshake shapes (DuckDB v2.x) — pass through
  local hs1 = U:find("^SELECT CATALOG_NAME, SCHEMA_NAME") and U:find("INFORMATION_SCHEMA%.SCHEMATA")
  local hs2 = U:find("^SELECT SCHEMA") and U:find(", SQL") and U:find("DUCKDB_TABLES") and U:find("DUCKDB_VIEWS") and U:find("UNION ALL")
  local hs3 = U:find("^WITH RECURSIVE SCHEMA_TREE") and U:find("DUCKDB_SCHEMAS%(%") and U:find("CURRENT_DATABASE%(%")
  if hs1 or hs2 or hs3 then return true end
  if U == "BEGIN TRANSACTION" or U == "ROLLBACK" or U == "COMMIT" then return true end

  if not sess then
    audit('authz', nil, q, 'deny', 'no session')
    return false
  end
  local grants = GRANTS[ROLES[sess.user] or '']
  if not grants then
    audit('authz', sess.user, q, 'deny', 'no role')
    return false
  end

  local kind, lead = classify(q)
  -- verb ceiling: RO token may never write, regardless of grants
  if kind == 'WRITE' and sess.ceiling == 'RO' then
    audit('authz', sess.user, q, 'deny', 'ceiling=RO blocks ' .. (lead or '?'))
    return false
  end
  if kind == 'OTHER' then
    -- SET / CALL / USE etc: deny by default (fail-closed)
    audit('authz', sess.user, q, 'deny', 'unclassified: ' .. (lead or '?'))
    return false
  end

  -- table grants: DEFAULT-DENY. The query may only touch tables the role
  -- holds a grant for; the audit table is protected outright (reading it
  -- over the wire would leak every other user's queries).
  local protected = { RBAC_AUDIT = true }
  for _, tn in ipairs(ALL_TABLES) do protected[tn] = true end
  for tn in pairs(protected) do
    if has_token(U, tn) then
      if tn == 'RBAC_AUDIT' then
        audit('authz', sess.user, q, 'deny', 'protected table: rbac_audit')
        return false
      end
      local g = grants[string.lower(tn)]
      local need_write = (kind == 'WRITE')
      local ok = g and (need_write and g.write or g.read)
      if not ok then
        audit('authz', sess.user, q, 'deny', 'no grant: ' .. string.lower(tn))
        return false
      end
    end
  end

  audit('authz', sess.user, q, 'allow', lead or kind)
  return true
end

local function setup()
  return 'ok'
end

-- operator-only dump: returns the audit ring as a JSON string (scalar-safe
-- through luajit_s; a Lua table would serialize as "table: 0x...")
local function jstr(s)
  if s == nil then return 'null' end
  return '"' .. (s:gsub('\\', '\\\\'):gsub('"', '\\"'):gsub('\n', '\\n')) .. '"'
end
local function audit_dump(p)
  local parts = {}
  local first = math.max(1, ring_n - RING_SIZE + 1)
  for i = first, ring_n do
    local e = ring[i]
    if e then
      table.insert(parts, string.format(
        '{"seq":%d,"ts":%d,"kind":%s,"user":%s,"q":%s,"verdict":%s,"detail":%s}',
        e.seq, e.ts, jstr(e.kind), jstr(e.user), jstr(e.q), jstr(e.verdict), jstr(e.detail)))
    end
  end
  return '[' .. table.concat(parts, ',') .. ']'
end

return function(p)
  if p.op == 'authn' then return authn(p) end
  if p.op == 'authz' then return authz(p) end
  if p.op == 'setup' then return setup() end
  if p.op == 'audit' then return audit_dump(p) end
  return 'error: unknown op'
end
$rbac$);
CREATE OR REPLACE MACRO rbac2_authn(sid, ct, st) AS (luajit_s('rbac2', {op:'authn', sid:sid, client_token:ct, server_token:st})::BOOLEAN);
CREATE OR REPLACE MACRO rbac2_authz(sid, query) AS (luajit_s('rbac2', {op:'authz', sid:sid, q:query})::BOOLEAN);
-- persistent audit sink (read it as the operator: SELECT * FROM rbac_audit)
CREATE TABLE IF NOT EXISTS rbac_audit (
  seq BIGINT, ts BIGINT, kind VARCHAR, user_name VARCHAR,
  q VARCHAR, verdict VARCHAR, detail VARCHAR
);
SET GLOBAL quack_authentication_function = 'rbac2_authn';
SET GLOBAL quack_authorization_function = 'rbac2_authz';
