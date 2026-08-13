-- Agent Tools
-- The concrete tools T5's turn loop can dispatch to (PLAN.md T6).
-- Registered onto T5's flat dispatch seam (rune.agent.register_tool,
-- see 87_agent.lua), but backed by a real rune.registry.new{kind="tool"}
-- underneath so tool calls get the same name/source/quarantine
-- machinery as every other subsystem (hooks, timers, triggers, ...): a
-- tool that keeps throwing is disabled individually rather than
-- derailing every future turn.
--
-- 2026-07-25: dropped create_trigger/remove_group/list_automation
-- (reflex-programming via installed Lua triggers) - real use showed
-- both the agent and the human watching it found the two-layer
-- behavior (some actions direct, some running unattended as
-- self-installed triggers) more confusing than the machine-speed
-- reflex was worth. See PLAN.md T6's addendum. Governance
-- (91_agent_policy.lua) and observability (96_agent_ui.lua) still
-- apply fully to what remains - only the reflex layer is gone.

local green, yellow, cyan, dim =
    rune.style.green, rune.style.yellow, rune.style.cyan, rune.style.gray

local registry = rune.registry.new{ kind = "tool" }

-- Wraps fn in the registry's quarantine machinery and plugs it into
-- T5's dispatch seam. A tool disabled by quarantine (or by hand, via
-- registry:disable(name)) raises instead of running - dispatch_tool
-- (87_agent.lua) turns that into an is_error tool_result, so the
-- model finds out its tool stopped working instead of silently
-- getting nothing.
local function register(name, description, input_schema, fn)
    local data = { name = name, source = rune.caller_source(1) }
    registry:add(data, { name = name })

    rune.agent.register_tool(name, description, input_schema, function(input)
        if not registry:active(data) then
            error('tool "' .. name .. '" is disabled (quarantined or turned off)')
        end
        local label = 'Tool "' .. name .. '"' .. (data.source and (" @" .. data.source) or "")
        local ok, result, errmsg = rune.guarded_call(label, data, fn, input)
        if not ok then
            -- guarded_call already echoed this and tracked the
            -- failure; re-raise the same message so dispatch_tool
            -- reports it to the model too - it needs the specific
            -- reason to have any chance of correcting its next call.
            error(errmsg or ('tool "' .. name .. '" failed'))
        end
        return result
    end)
end

-- Exported so tools defined in later core files (89_memory.lua) get the
-- same quarantine machinery instead of reaching past it to
-- rune.agent.register_tool directly. Deliberately just this one
-- function: the registry itself stays private, so nothing outside can
-- enable/disable entries behind /tools' back.
rune.agent_tools = { register = register }

register("send_command", "Send a raw command to the MUD, exactly as a player would type it.", {
    type = "object",
    properties = {
        cmd = { type = "string", description = "The command text, e.g. 'kill kobold', 'north', '#3 kill rat'." },
    },
    required = { "cmd" },
}, function(input)
    if type(input) ~= "table" or type(input.cmd) ~= "string" or input.cmd == "" then
        error("send_command: input.cmd must be a non-empty string")
    end
    local ok, err = rune.agent_policy.send(input.cmd)
    if not ok then
        error(err)
    end
    return "sent"
end)

register("speak", "Talk to other players over say (room), tell (private), or gossip (global).", {
    type = "object",
    properties = {
        channel = { type = "string", enum = { "say", "tell", "gossip" }, description = "Which channel to speak on." },
        target = { type = "string", description = "Player name to tell. Required when channel is 'tell'." },
        message = { type = "string", description = "The message text." },
    },
    required = { "channel", "message" },
}, function(input)
    if type(input) ~= "table" or type(input.message) ~= "string" or input.message == "" then
        error("speak: input.message must be a non-empty string")
    end
    local cmd
    if input.channel == "say" then
        cmd = "say " .. input.message
    elseif input.channel == "gossip" then
        cmd = "gossip " .. input.message
    elseif input.channel == "tell" then
        if type(input.target) ~= "string" or input.target == "" then
            error("speak: input.target is required when channel is 'tell'")
        end
        cmd = "tell " .. input.target .. " " .. input.message
    else
        error("speak: input.channel must be 'say', 'tell', or 'gossip', got " .. tostring(input.channel))
    end
    local ok, err = rune.agent_policy.send(cmd)
    if not ok then
        error(err)
    end
    return "sent"
end)

-- The log read-back tools (92_agent_log.lua). Deliberately memory, not
-- perception: the session log holds only what already reached the
-- screen plus the agent's own reasoning, so nothing here can surface
-- something a human player at the same terminal wouldn't have seen
-- (PLAN.md §4).

register("search_log",
    "Search this session's log for lines matching a regular expression - your " ..
    "own memory of everything seen and done so far, reaching much further back " ..
    "than the recent output in each observation. Use it to recall something " ..
    "specific: a name, a direction, a quest hint, what happened last time you " ..
    "tried something.", {
    type = "object",
    properties = {
        pattern = { type = "string", description = "A regular expression matched against each log line." },
        max_results = { type = "number", description = "How many matches to return (most recent kept). Default 50, max 500." },
    },
    required = { "pattern" },
}, function(input)
    if type(input) ~= "table" or type(input.pattern) ~= "string" or input.pattern == "" then
        error("search_log: input.pattern must be a non-empty string")
    end
    local lines, err = rune.log.search(input.pattern, input.max_results)
    if not lines then
        error("search_log: " .. tostring(err))
    end
    return lines
end)

register("read_log",
    "Read the most recent lines of this session's log - game output, your own " ..
    "commands, and your own reasoning, interleaved in the order they happened. " ..
    "Use it to look further back than the recent output you were given.", {
    type = "object",
    properties = {
        lines = { type = "number", description = "How many trailing lines to return. Default 50, max 500." },
    },
}, function(input)
    local n = type(input) == "table" and input.lines or nil
    local lines, err = rune.log.read(n)
    if not lines then
        error("read_log: " .. tostring(err))
    end
    return lines
end)

-- /tools - list registered agent tools and their quarantine status
rune.command.add("tools", function(args)
    local items = registry:items()
    rune.echo(green("[Tools]") .. dim(" (" .. #items .. " total)"))
    for _, data in ipairs(items) do
        local status = data.enabled and green("[on] ") or rune.style.red("[off]")
        local src_str = data.source and ("  " .. dim("@" .. data.source)) or ""
        rune.echo(string.format("  %s %s%s", status, yellow(data.name), src_str))
    end
end, "List agent tools and their quarantine status")
