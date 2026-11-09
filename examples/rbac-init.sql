INSTALL luajit FROM community;
LOAD luajit;
SELECT * FROM luajit_module(mode := 'compile', sql_name := 'rbac', source := $rbac$-- RBAC policy for quack-proxy shards (in-memory variant)
-- IMPORTANT: authn/authz run INSIDE connection setup. Calling _duckdb_query
-- there re-enters DuckDB during the handshake and deadlocks the server
-- (reproduced 2026-11-09: client ATTACH hangs forever). All state therefore
-- lives in Lua memory; only setup() may touch the database.
local esc = function(s) return (s:gsub("'", "''")) end

-- static credential store (edit here, or generate this file from your IdP)
local TOKENS = {
  ['viewer-tok']   = 'viewer',
  ['analyst-tok']  = 'analyst',
}
local ROLES = {
  viewer   = 'viewer_role',
  analyst  = 'analyst_role',
}
local GRANTS = {
  viewer_role   = { public_data = true },
  analyst_role  = { public_data = true, secret_data = true },
}
-- sid -> user, populated by authn, read by authz (process-lifetime)
local SESSIONS = {}

local function authn(p)
  local user = TOKENS[p.client_token or '']
  if not user then return false end
  SESSIONS[p.sid or ''] = user
  return true
end

local function authz(p)
  local q = string.upper(p.q or '')
  q = q:gsub('^%s+', ''):gsub('%s+$', '')
  if q == '' then return false end

  -- quack client ATTACH handshake shapes (DuckDB v2.x)
  local hs1 = q:find("^SELECT CATALOG_NAME, SCHEMA_NAME") and q:find("INFORMATION_SCHEMA%.SCHEMATA")
  local hs2 = q:find("^SELECT SCHEMA") and q:find(", SQL") and q:find("DUCKDB_TABLES") and q:find("DUCKDB_VIEWS") and q:find("UNION ALL")
  local hs3 = q:find("^WITH RECURSIVE SCHEMA_TREE") and q:find("DUCKDB_SCHEMAS%(%)") and q:find("CURRENT_DATABASE%(%)")
  if hs1 or hs2 or hs3 then return true end
  if q == "BEGIN TRANSACTION" or q == "ROLLBACK" or q == "COMMIT" then return true end

  if not q:find("^SELECT") then return false end
  local user = SESSIONS[p.sid or '']
  if not user then return false end
  local grants = GRANTS[ROLES[user] or '']
  if not grants then return false end
  -- deny if the query mentions any table the role cannot read
  for tbl in pairs(GRANTS.analyst_role) do end -- ensure both known tables iterate below
  local ALL_TABLES = { 'PUBLIC_DATA', 'SECRET_DATA' }
  for _, tn in ipairs(ALL_TABLES) do
    if q:find("%f[%w]" .. tn .. "%f[%W]") and not grants[string.lower(tn)] then
      return false
    end
  end
  return true
end

local function setup()
  return 'ok'
end

return function(p)
  if p.op == 'authn' then return authn(p) end
  if p.op == 'authz' then return authz(p) end
  if p.op == 'setup' then return setup() end
  return 'error: unknown op'
end
$rbac$);
CREATE MACRO rbac_authn(sid, ct, st) AS (luajit_s('rbac', {op:'authn', sid:sid, client_token:ct, server_token:st})::BOOLEAN);
CREATE MACRO rbac_authz(sid, query) AS (luajit_s('rbac', {op:'authz', sid:sid, q:query})::BOOLEAN);
SET GLOBAL quack_authentication_function = 'rbac_authn';
SET GLOBAL quack_authorization_function = 'rbac_authz';
