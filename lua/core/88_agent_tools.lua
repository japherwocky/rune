-- Agent Tools
-- The concrete tools T5's turn loop can dispatch to (PLAN.md T6) - the
-- centerpiece being create_trigger, which lets the agent write reflex
-- automation (Lua triggers) for repetitive situations instead of
-- re-invoking the LLM every round. Registered onto T5's flat dispatch
-- seam (rune.agent.register_tool, see 87_agent.lua), but backed by a
-- real rune.registry.new{kind="tool"} underneath so tool calls get
-- the same name/source/quarantine machinery as every other subsystem
-- (hooks, timers, triggers, ...): a tool that keeps throwing is
-- disabled individually rather than derailing every future turn.
--
-- Group discipline: create_trigger asks the model for a short label
-- ("combat", "nav") and structurally prepends "agent-" to it itself -
-- the model cannot forget the prefix or collide with a human's own
-- trigger groups by omitting it, because the prefix isn't something
-- the model ever gets to spell out. remove_group and list_automation
-- use the same "agent-<label>" -> "agent-" convention to prune/inspect
-- only what the agent itself installed.

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

-- Validates a model-supplied group label and returns the namespaced
-- form. Restricting the charset keeps "agent-<label>" predictable for
-- remove_group/list_automation's own matching.
local function agent_group(label)
    if type(label) ~= "string" or not label:match("^[%w_-]+$") then
        error('group must be a non-empty label using only letters, digits, "-" and "_", got ' .. tostring(label))
    end
    return "agent-" .. label
end

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

register("create_trigger",
    "Install a reflex: when a server output line matches pattern, send command " ..
    "automatically, without waiting for another turn. Use this only for a " ..
    "situation that will keep recurring many times before it's done (combat " ..
    "rounds, a flee-and-chase sequence) - it runs at machine speed and costs " ..
    "nothing until it stops matching anything useful. Do NOT use this for a " ..
    "one-off action like walking a route or a single conversation; just send " ..
    "the command(s) directly instead.", {
    type = "object",
    properties = {
        pattern = { type = "string", description = "A regular expression matched against each server output line." },
        command = { type = "string", description = "The command to send on a match. %1, %2, ... substitute the pattern's capture groups. Separate multiple commands with ';'." },
        group = { type = "string", description = "Short label for this reflex, e.g. 'combat' or 'nav' (stored as agent-<group>). remove_group('combat') later clears everything tagged with it in one call." },
        once = { type = "boolean", description = "Remove this trigger after it fires once. Default false." },
        gag = { type = "boolean", description = "Hide the matching line from the transcript/screen. Default false." },
    },
    required = { "pattern", "command", "group" },
}, function(input)
    if type(input) ~= "table" then
        error("create_trigger: input must be an object")
    end
    if type(input.pattern) ~= "string" or input.pattern == "" then
        error("create_trigger: input.pattern must be a non-empty string")
    end
    if type(input.command) ~= "string" or input.command == "" then
        error("create_trigger: input.command must be a non-empty string")
    end
    local group = agent_group(input.group)
    local command = input.command
    -- A function action, not the plain string rune.trigger.regex would
    -- otherwise send directly - PLAN.md T9's governance (rate limit,
    -- denylist, oscillation) lives at rune.agent_policy.send, and a
    -- reflex is the one send path with no LLM round-trip anywhere near
    -- it to slow it down, so it needs that gate more than any other
    -- caller. rune.substitute_captures is the exact same substitution
    -- rune.trigger.process would have done for a string action, so
    -- %1/%2 behavior is unchanged - only where the substituted command
    -- goes is different. A "denied" (not "rate_limited") failure is
    -- re-raised so a reflex that keeps trying something denylisted
    -- quarantines like any other malfunctioning trigger, feeding T9's
    -- quarantine -> re-plan wake (91_agent_policy.lua); a rate-limited
    -- firing is dropped silently instead, since throttling a fast (but
    -- otherwise fine) reflex during a burst must not itself escalate
    -- into disabling it.
    rune.trigger.regex(input.pattern, function(matches)
        local cmd = rune.substitute_captures(command, matches)
        local ok, err, reason = rune.agent_policy.send(cmd)
        if not ok and reason == "denied" then
            error(err)
        end
    end, {
        group = group,
        once = input.once or false,
        gag = input.gag or false,
    })
    return "created trigger in group " .. group
end)

register("remove_group",
    "Remove every trigger previously installed under the given group label " ..
    "(the same short label passed to create_trigger) - use this when a " ..
    "fight ends, a zone changes, or a reflex is no longer wanted.", {
    type = "object",
    properties = {
        group = { type = "string", description = "The short label, e.g. 'combat' (matches agent-<group>)." },
    },
    required = { "group" },
}, function(input)
    if type(input) ~= "table" then
        error("remove_group: input must be an object")
    end
    local group = agent_group(input.group)
    local removed = rune.trigger.remove_group(group)
    return removed .. " trigger(s) removed from group " .. group
end)

register("list_automation",
    "List every trigger the agent has installed (only its own agent-* groups, " ..
    "not the human player's own triggers).",
    { type = "object", properties = {} },
    function()
    local result = {}
    for _, t in ipairs(rune.trigger.list()) do
        if t.group and t.group:match("^agent%-") then
            table.insert(result, { kind = "trigger", pattern = t.match, command = t.value, group = t.group, enabled = t.enabled })
        end
    end
    return result
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
