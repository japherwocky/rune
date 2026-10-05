-- Agent Core
-- The state machine: idle -> observing -> waiting_llm -> acting ->
-- observing (PLAN.md T5). Single agent per process; collaboration
-- between agents happens over in-game channels (speak, a T6 tool),
-- never shared memory - see PLAN.md's design principles.
--
-- Cadence combines three wake sources, exactly as spec'd:
--   - salient GMCP events (combat start, low hp, a channel message)
--     just set the wake flag;
--   - the "prompt" hook is the natural "my turn" signal - it wakes
--     and attempts a think immediately;
--   - a debounce timer polls every DEBOUNCE_SECONDS and catches
--     anything the other two missed.
-- Single-flight: `thinking` guards against two outstanding rune.llm
-- calls. A wake that arrives mid-think just sets the flag again;
-- finish_turn() rechecks it the moment the current think returns
-- ("events during a think coalesce into think again on return").
-- Rate-limiting a busy agent is explicitly deferred to T9's
-- governance layer, not this module.
--
-- Tools are dispatched through a flat name -> fn map this module
-- owns (register_tool/unregister_tool). T5 has no opinion on what
-- tools exist - it ships empty, so tests can register fakes and T6
-- can later register the real ones (send_command, speak, ...)
-- without this file changing. T6's own registry-backed quarantine
-- wraps its tool functions before registering them here; this module
-- only needs a name and a plain Lua function.

rune.agent = {}

local DEBOUNCE_SECONDS = 2
local DEFAULT_MAX_TOKENS = 1024
local LOW_HP_RATIO = 0.3
local GOAL_KEY = "agent_goal"

-- The prohibitions in the last paragraph came first, and on their own
-- they were not enough: a model given only rules about what not to write
-- still narrated its way through the world instead of playing it. What
-- was missing was the domain - that a MUD answers commands with room
-- descriptions, that an exits line is a list of commands, that acting at
-- all means calling send_command. Teach that first, then forbid. A
-- prompt that is all prohibition says what to avoid without ever saying
-- what the job is.
local DEFAULT_SYSTEM = table.concat({
    "You are playing a text MUD (a multiplayer text adventure) through the " ..
        "Rune client. You are a character inside the game world, and the only " ..
        "way to do anything at all is to call the send_command tool with a " ..
        "command the game understands.",

    "How the game talks to you: when you arrive somewhere, or send `look`, it " ..
        "sends a description of the room you are standing in, usually followed " ..
        "by a line like `[Exits: north east down]`. Those exits are the " ..
        "directions you can move, and each one is itself a command. Game " ..
        "commands are short: `look`, `north`, `get sword`, `kill rat`, " ..
        "`inventory`, `say hello`.",

    "Each turn you are given your mission, what you have learned, whatever " ..
        "status data the game provides, and the recent game text. The game " ..
        "text is the ground truth for where you are and what is happening - " ..
        "read it before you act, and expect the room description to be in " ..
        "there rather than in a tidy field.",

    "Only the game text and tool results are real. Nothing you write yourself " ..
        "happens, and nothing you write reaches anyone: do not write room " ..
        "descriptions, a \"Command:\" label, dialogue for other characters, or " ..
        "anything else shaped like game output. If you are not calling a tool, " ..
        "write one short sentence of plan and stop there.",
}, "\n\n")

-- name -> {name, description, input_schema, fn}
local tools = {}

-- rune.agent.register_tool(name, description, input_schema, fn)
-- input_schema is passed through verbatim as the tool's JSON-schema
-- `input_schema` in requests (see rune.llm.chat / T4) - this module
-- does not interpret it. A second registration under the same name
-- replaces the first.
function rune.agent.register_tool(name, description, input_schema, fn)
    if type(name) ~= "string" or name == "" then
        error("rune.agent.register_tool: name must be a non-empty string", 2)
    end
    if type(fn) ~= "function" then
        error("rune.agent.register_tool: fn must be a function", 2)
    end
    tools[name] = { name = name, description = description, input_schema = input_schema, fn = fn }
end

function rune.agent.unregister_tool(name)
    tools[name] = nil
end

local function tool_defs()
    local defs = {}
    for _, t in pairs(tools) do
        table.insert(defs, { name = t.name, description = t.description, input_schema = t.input_schema })
    end
    return defs
end

-- Runs one tool_use block. Never throws: a missing tool or a tool
-- that errors becomes a tool_result the model can see and react to
-- (is_error = true) rather than aborting the whole turn.
local function dispatch_tool(name, input)
    local t = tools[name]
    if not t then
        return "error: unknown tool \"" .. tostring(name) .. "\"", true
    end
    local ok, result = pcall(t.fn, input)
    if not ok then
        return "error: tool \"" .. name .. "\" failed: " .. tostring(result), true
    end
    if result == nil then
        return "ok", false
    end
    if type(result) == "string" then
        return result, false
    end
    local encoded = rune.json.encode(result)
    return encoded or tostring(result), false
end

local function goal()
    return rune.store.get(GOAL_KEY)
end

local function set_goal(text)
    rune.store.set(GOAL_KEY, text)
end

-- Builds the single user message a turn starts from: current goal +
-- retrieved memories + a fresh perception snapshot + recent
-- transcript. Deliberately not a growing chat history across turns
-- (that would blow out context over a long session) - what persists is
-- the goal, in the model's own words, and the memory stream
-- (89_memory.lua), which is retrieved fresh and capped every turn
-- rather than accumulated in the message list.
--
-- The heading spells out "your own words, not confirmed fact" rather
-- than a bare "## Goal": reply.text is saved and replayed here
-- verbatim (set_goal below), so if a turn's text reads as narrative
-- rather than a plan, an unqualified heading would hand it back next
-- turn looking like established ground truth - inviting the model to
-- treat its own invention as something that already happened and
-- continue it, rather than as its own prior, possibly-wrong words.
--
-- The memory section (T13) is the one part that *is* established fact
-- rather than the model's own words: 89_memory.lua's records are built
-- from GMCP, from tool results, and from conclusions the model drew
-- deliberately, not from whatever it happened to narrate last turn.
-- Capped hard at MEMORY_RECALL records - the whole point of building a
-- fresh observation each turn is that context does not grow without
-- bound, and an unbounded recall would give that back.
local MEMORY_RECALL = 6

-- Operator-set and fixed for the run, unlike goal() below, which is only
-- ever whatever the model last said. Declared up here because
-- build_observation reads it; set by rune.agent.start/set_mission.
local mission

local function memory_section()
    local recs = rune.memory.recall({ limit = MEMORY_RECALL })
    if #recs == 0 then
        return nil
    end
    return "## What you've learned (your memory - these did happen)\n" ..
        table.concat(rune.memory.format(recs), "\n")
end

local function is_blank(t)
    return type(t) ~= "table" or next(t) == nil
end

local function build_observation()
    local snap = rune.perception.snapshot()
    local transcript = rune.perception.transcript()
    local sections = {}

    -- The mission is listed first and outranks the plan: it is the one
    -- part of the observation the model did not write and cannot drift.
    if mission and mission ~= "" then
        table.insert(sections,
            "## Your mission (set by your operator - this does not change)\n" .. mission)
    end

    table.insert(sections,
        "## Your plan (your own words from the end of your last turn - " ..
            "not confirmed fact)\n" .. (goal() or "(none yet - decide what to do)"))

    local memories = memory_section()
    if memories then
        table.insert(sections, memories)
    end

    -- Vitals/Status/Room come from GMCP, and plenty of MUDs send none of
    -- it at all. An empty "## Room\n{}" is actively worse than no section:
    -- it answers "where am I" with "nowhere", under the exact heading the
    -- model needs, while the real room description sits unlabelled in the
    -- game text below. Omit what the game did not send, and when it sent
    -- nothing, say where to look instead.
    local structured = false
    for _, part in ipairs({
        { "Vitals", snap.vitals },
        { "Status", snap.status },
        { "Room", snap.room },
    }) do
        if not is_blank(part[2]) then
            structured = true
            table.insert(sections, "## " .. part[1] .. "\n" .. (rune.json.encode(part[2]) or "{}"))
        end
    end
    if not structured then
        table.insert(sections,
            "## Status data\nThis game sends no structured data, so the game " ..
            "text below is your only source for where you are, what is here, " ..
            "and which exits exist. Read it as a room description, not a log.")
    end

    table.insert(sections, "## Recent game output\n" .. table.concat(transcript, "\n"))
    return table.concat(sections, "\n\n")
end

local active = false
local thinking = false
local wake_pending = false
local handles = {}

local model, system, max_tokens

-- Forward declarations: start_turn and on_reply call each other
-- (tool_use continues the turn via another request), and finish_turn
-- calls start_turn again on a coalesced wake.
local start_turn, on_reply, finish_turn

-- A turn ends when the model stops asking for tools. A model that never
-- stops (observed: 25 tool calls in one turn without a single end_turn)
-- holds the turn open forever, and because agent_turn_end never fires,
-- everything hanging off the turn boundary - T9's budget accounting and
-- oscillation check, the goal update - never runs either. Cap the hops
-- so a runaway turn still closes and still gets governed.
local MAX_TOOL_HOPS = 8

local function request(messages, cb)
    local defs = tool_defs()
    rune.llm.chat({
        model = model,
        system = system,
        messages = messages,
        tools = #defs > 0 and defs or nil,
        max_tokens = max_tokens,
    }, cb)
end

on_reply = function(messages, reply, err, hop)
    hop = hop or 1
    if not active then
        -- Stopped mid-turn: the HTTP request already went out and
        -- can't be recalled, but its result must not act on a stopped
        -- agent's behalf (no continuation, no goal update).
        thinking = false
        return
    end

    if err then
        rune.echo(rune.style.red("[agent]") .. " " .. err)
        rune.hooks.call("agent_error", err)
        finish_turn()
        return
    end

    -- Fires for every hop of the turn, including intermediate
    -- tool_use replies (which often carry reasoning text alongside
    -- the tool call) - T7's pane/log watch this for live reasoning.
    rune.hooks.call("agent_reply", reply)

    if reply.stop_reason == "tool_use" and #reply.tool_uses > 0 then
        if hop >= MAX_TOOL_HOPS then
            -- Close the turn here rather than continuing. The tool calls
            -- in this reply are deliberately not dispatched: the model
            -- has had its hops, and running them would be one more
            -- action with nothing left to interpret the result.
            local notice = "tool-hop cap reached (" .. MAX_TOOL_HOPS ..
                ") - ending the turn"
            rune.echo(rune.style.gray("[agent]") .. " " .. notice)
            rune.hooks.call("agent_policy", "hop_cap", notice)
            rune.hooks.call("agent_turn_end", reply)
            finish_turn()
            return
        end
        table.insert(messages, { role = "assistant", content = reply.content })

        local results = {}
        for _, tu in ipairs(reply.tool_uses) do
            local content, is_error = dispatch_tool(tu.name, tu.input)
            rune.hooks.call("agent_tool_call", tu.name, tu.input, content, is_error)
            table.insert(results, {
                type = "tool_result",
                tool_use_id = tu.id,
                content = content,
                is_error = is_error or nil,
            })
        end
        table.insert(messages, { role = "user", content = results })

        request(messages, function(reply2, err2)
            on_reply(messages, reply2, err2, hop + 1)
        end)
        return
    end

    if reply.text and reply.text ~= "" then
        set_goal(reply.text)
    end
    rune.hooks.call("agent_turn_end", reply)
    finish_turn()
end

start_turn = function()
    thinking = true
    wake_pending = false
    rune.hooks.call("agent_turn_start")
    local messages = { { role = "user", content = build_observation() } }
    request(messages, function(reply, err)
        on_reply(messages, reply, err, 1)
    end)
end

finish_turn = function()
    thinking = false
    if active and wake_pending then
        wake_pending = false
        start_turn()
    end
end

local function maybe_think()
    if not active or thinking or not wake_pending then
        return
    end
    start_turn()
end

-- rune.agent.wake(reason) - request a think. Safe to call from
-- anywhere (a trigger noticing something the agent should react to,
-- a test): sets the wake flag and, if nothing is in flight, starts a
-- think immediately. A no-op while stopped.
function rune.agent.wake(reason)
    if not active then
        return
    end
    wake_pending = true
    maybe_think()
end

-- rune.agent.start(opts) - opts: { model (required), system?,
-- max_tokens? }. Enables rune.perception (see T3 - it is inert until
-- something asks for it) and wires the cadence. Idempotent.
function rune.agent.start(opts)
    if active then
        return
    end
    opts = opts or {}
    if type(opts.model) ~= "string" or opts.model == "" then
        error("rune.agent.start: opts.model must be a non-empty string", 2)
    end

    active = true
    model = opts.model
    system = opts.system or DEFAULT_SYSTEM
    max_tokens = opts.max_tokens or DEFAULT_MAX_TOKENS
    if opts.goal ~= nil then
        if type(opts.goal) ~= "string" then
            error("rune.agent.start: opts.goal must be a string", 2)
        end
        mission = opts.goal
    end

    rune.perception.enable()
    rune.memory.enable()

    local was_fighting = false

    handles = {
        -- The natural "my turn" signal - never gags/rewrites, so it
        -- must return nil either way.
        rune.hooks.on("prompt", function()
            wake_pending = true
            maybe_think()
        end, { name = "agent-wake-prompt", priority = 150 }),

        -- Salient GMCP deltas (see botmud#20's confirmed package
        -- shapes): only set the flag, never think immediately - the
        -- prompt hook or the debounce timer does that. This keeps a
        -- burst of vitals ticks from each independently trying to
        -- start a think.
        rune.gmcp.on("Char.Vitals", function(data)
            if data and data.hp and data.maxhp and data.maxhp > 0
                and (data.hp / data.maxhp) < LOW_HP_RATIO then
                wake_pending = true
            end
        end, { name = "agent-wake-low-hp" }),

        rune.gmcp.on("Char.Status", function(data)
            local fighting = data ~= nil and data.position == "fighting"
            if fighting and not was_fighting then
                wake_pending = true
            end
            was_fighting = fighting
        end, { name = "agent-wake-combat-start" }),

        rune.gmcp.on("Comm.Channel", function()
            wake_pending = true
        end, { name = "agent-wake-channel" }),

        rune.timer.every(DEBOUNCE_SECONDS, function()
            maybe_think()
        end, { name = "agent-debounce" }),
    }
end

-- rune.agent.stop() - unwinds start(): removes hooks/timer, disables
-- perception. An LLM call already in flight is left to arrive (Go
-- can't recall an in-progress HTTP request) but on_reply drops it
-- (see the `not active` guard above) rather than acting on it.
-- Idempotent.
function rune.agent.stop()
    if not active then
        return
    end
    active = false
    for _, handle in ipairs(handles) do
        handle:remove()
    end
    handles = {}
    rune.perception.disable()
    -- Stops recording; the stream itself is durable and survives, so a
    -- restarted agent still knows what it learned (rune.memory.disable).
    rune.memory.disable()
end

function rune.agent.is_active()
    return active
end

-- A read-only snapshot for tests and T7's observability pane.
function rune.agent.status()
    return {
        active = active,
        thinking = thinking,
        wake_pending = wake_pending,
        mission = mission,
        goal = goal(),
        model = model,
    }
end

-- rune.agent.set_mission(text) - set or clear the operator's mission
-- mid-run. Separate from the plan the model writes for itself each turn
-- (set_goal above): the mission is the part of the observation the agent
-- cannot author, so it is the only durable steering there is. Takes
-- effect on the next turn's observation. Pass nil or "" to clear.
function rune.agent.set_mission(text)
    if text ~= nil and type(text) ~= "string" then
        error("rune.agent.set_mission: text must be a string or nil", 2)
    end
    mission = (text ~= "" and text) or nil
end

-- rune.agent.mission() -> the current mission, or nil.
function rune.agent.mission()
    return mission
end
