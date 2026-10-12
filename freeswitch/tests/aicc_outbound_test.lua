-- SPDX-License-Identifier: Apache-2.0
--
-- Unit test for the aicc_did / export_vars stamping in scripts/aicc_outbound.lua.
-- It runs with a stock interpreter, no switch needed:
--
--   lua freeswitch/tests/aicc_outbound_test.lua
--
-- The script is loaded with a fake `session` and `freeswitch`; the database is
-- reported unreachable so only the stamping and the early return are exercised.
-- The real behaviour on a channel is verified on a switch (issue #160).

local script = arg and arg[0] and arg[0]:gsub("tests/[^/]*$", "scripts/aicc_outbound.lua")
  or "freeswitch/scripts/aicc_outbound.lua"

local failures = 0
local function check(name, got, want)
  if got ~= want then
    failures = failures + 1
    print(string.format("FAIL %s: got %s, want %s", name, tostring(got), tostring(want)))
  else
    print("ok   " .. name)
  end
end

local function run(vars)
  local set, logs, executed = {}, {}, {}
  _G.freeswitch = {
    consoleLog = function(level, message) logs[#logs + 1] = level .. " " .. message end,
    getGlobalVariable = function() return nil end, -- no DSN: the database is unreachable
    Dbh = function() error("must not be reached without a DSN") end,
  }
  _G.session = {
    getVariable = function(_, name) if set[name] ~= nil then return set[name] end return vars[name] end,
    setVariable = function(_, name, value) set[name] = value end,
    execute = function(_, app) executed[#executed + 1] = app end,
  }
  assert(loadfile(script))()
  return set, logs, executed
end

local set, _, executed = run({ trunk_clid = "95001" })
check("stamps aicc_did from trunk_clid", set.aicc_did, "95001")
check("absent export_vars becomes aicc_did", set.export_vars, "aicc_did")
check("no recording without a database", #executed, 0)

set = run({ effective_caller_id_number = "95002", export_vars = "aicc_call_id" })
check("falls back to effective_caller_id_number", set.aicc_did, "95002")
check("appends to export_vars", set.export_vars, "aicc_call_id,aicc_did")

set = run({ trunk_clid = "95001", export_vars = "aicc_call_id,aicc_did,aicc_language,aicc_call_type" })
check("does not duplicate the name", set.export_vars, "aicc_call_id,aicc_did,aicc_language,aicc_call_type")

set = run({ trunk_clid = "95001", export_vars = "aicc_didx,aicc_call_id" })
check("a name that merely contains it is not a match", set.export_vars, "aicc_didx,aicc_call_id,aicc_did")

set = run({ trunk_clid = "95001", export_vars = "", aicc_recording_started = "true" })
check("stamped before the already-recording early return", set.aicc_did, "95001")
check("exported before the already-recording early return", set.export_vars, "aicc_did")

local logs
set, logs = run({ export_vars = "aicc_call_id" })
check("empty DID sets no aicc_did", set.aicc_did, nil)
check("empty DID leaves export_vars alone", set.export_vars, nil)
check("empty DID logs at debug", logs[1] ~= nil and logs[1]:find("^debug") ~= nil, true)

set = run({ trunk_clid = "", effective_caller_id_number = "", aicc_recording_started = "true" })
check("empty DID on the early-return path sets nothing", set.aicc_did, nil)

if failures > 0 then
  print(failures .. " failure(s)")
  os.exit(1)
end
print("all passed")
