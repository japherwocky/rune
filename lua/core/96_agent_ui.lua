-- Agent Observability (live mode) + start/stop control
-- Watches the agent core (87_agent.lua) through its agent_* hooks and
-- renders what it sees: reasoning, tool calls, and turn markers
-- echoed straight into the main scrollback (a separate reasoning pane
-- was tried first - see PLAN.md T7 - but that put reasoning and the
-- MUD output it was reacting to on two disconnected tracks, hard to
-- read in full and hard to correlate; moved inline instead), a
-- status-bar segment, and per-turn rune.log entries (PLAN.md T7). The
-- hook handlers below are pure observers - never call back into
-- rune.agent, never affect the turn loop, so a broken render here
-- can't derail a think (and per-hook quarantine means a failing
-- renderer just stops updating instead of spamming errors). The
-- "/agent start|stop" command is the one deliberate exception - a
-- direct, user-initiated call into rune.agent, not a hook reacting to
-- agent activity.
--
-- Registered unconditionally, like the core status bar in 95_ui.lua:
-- the "agent_status" bar exists but is invisible to a plain human
-- session unless placed into rune.ui.layout (the status bar registry
-- pattern - see 35_bars.lua), and every handler here is driven
-- entirely by hooks that T5 only ever fires from inside a turn, which
-- never runs unless something calls rune.agent.start(). Nothing to
-- opt into.
--
-- Add "agent_status" to your rune.ui.layout alongside "status" to see
-- the state/tokens/goal summary bar.

-- Optional cost estimate: set both fields to show a "$" figure next
-- to the token counts. Left nil by default rather than guessing at
-- per-model pricing (Zen's catalog rotates - see PLAN.md T4/T8) or
-- hardcoding a number that would silently go stale. Real budget
-- enforcement is T9's job; this is display-only.
rune.agent_ui = {}
rune.agent_ui.pricing = nil -- { input_per_million = 3, output_per_million = 15 }

local total_input_tokens = 0
local total_output_tokens = 0
local last_action = "idle"

local function log_line(text)
    rune.log.write("[Agent] " .. text)
end

rune.hooks.on("agent_turn_start", function()
    last_action = "thinking..."
    rune.echo(rune.style.dim("--- turn start ---"))
    log_line("turn start")
end, { name = "agent-ui-turn-start" })

local function count_usage(reply)
    if reply and reply.usage then
        total_input_tokens = total_input_tokens + (reply.usage.input_tokens or 0)
        total_output_tokens = total_output_tokens + (reply.usage.output_tokens or 0)
    end
end

rune.hooks.on("agent_reply", function(reply)
    count_usage(reply)
    if reply.text and reply.text ~= "" then
        rune.echo(rune.style.dim("[agent] ") .. reply.text)
        log_line("reply: " .. reply.text)
    end
end, { name = "agent-ui-reply" })

-- Reflection (89_memory.lua) runs its own LLM call outside the turn
-- loop, so it never fires agent_reply - both the token count and the
-- insights themselves have to be picked up here separately. Shown
-- distinctly from ordinary reasoning: a conclusion the bot just
-- committed to memory is a different kind of event from a plan for the
-- next command, and reads wrong unlabeled in the same inline stream.
rune.hooks.on("agent_reflection", function(insights, reply)
    count_usage(reply)
    if #insights == 0 then
        return
    end
    last_action = "reflected"
    for _, insight in ipairs(insights) do
        rune.echo(rune.style.magenta("[reflect] ") .. insight)
        log_line("reflection: " .. insight)
    end
end, { name = "agent-ui-reflection" })

-- search_log/read_log can return up to 500 lines (92_agent_log.lua) -
-- worth keeping in full in the durable log, but dumping all of it into
-- the live scrollback defeats the readability the inline-echo move
-- above was for: a screenful of the agent's own past history buries
-- the actual turn-by-turn thread a human is trying to follow. Shown
-- on screen as a count instead; the full result still reaches the log.
local LOG_READBACK_TOOLS = { search_log = true, read_log = true }

local function screen_result(name, result, is_error)
    if is_error or not LOG_READBACK_TOOLS[name] then
        return tostring(result)
    end
    local decoded = rune.json.decode(result)
    if type(decoded) == "table" then
        return #decoded .. " line(s) (see log)"
    end
    return "(results omitted, see log)"
end

rune.hooks.on("agent_tool_call", function(name, input, result, is_error)
    local encoded_input = rune.json.encode(input) or tostring(input)
    local full_line = "[tool] " .. name .. "(" .. encoded_input .. ") -> " .. tostring(result)
    local shown_line = "[tool] " .. name .. "(" .. encoded_input .. ") -> " ..
        screen_result(name, result, is_error)
    if is_error then
        last_action = "tool " .. name .. " failed"
        rune.echo(rune.style.red(shown_line))
    else
        last_action = "tool: " .. name
        rune.echo(rune.style.cyan(shown_line))
    end
    log_line(full_line .. (is_error and " (error)" or ""))
end, { name = "agent-ui-tool-call" })

rune.hooks.on("agent_turn_end", function(reply)
    last_action = "idle"
    rune.echo(rune.style.dim("--- turn end (" .. tostring(reply.stop_reason) .. ") ---"))
    log_line("turn end: " .. tostring(reply.stop_reason))
end, { name = "agent-ui-turn-end" })

rune.hooks.on("agent_error", function(err)
    last_action = "error"
    rune.echo(rune.style.red("[error] " .. err))
    log_line("error: " .. err)
end, { name = "agent-ui-error" })

-- rune.agent_ui.summary() -> the exact fields the bar renders, so
-- tests (and /agent) don't have to scrape rendered/styled strings.
function rune.agent_ui.summary()
    local cost = nil
    if rune.agent_ui.pricing then
        cost = (total_input_tokens / 1e6) * (rune.agent_ui.pricing.input_per_million or 0)
             + (total_output_tokens / 1e6) * (rune.agent_ui.pricing.output_per_million or 0)
    end
    return {
        input_tokens = total_input_tokens,
        output_tokens = total_output_tokens,
        cost = cost,
        last_action = last_action,
    }
end

-- Named "agent_status", not "agent": the layout resolver checks bars
-- before panes for a given name (ui/tui/layout.go's getWidget), so a
-- bar sharing the pane's name would permanently shadow it - no layout
-- could ever place the reasoning pane, only this one-line summary.
rune.ui.bar("agent_status", function(width)
    local s = rune.agent.status()
    if not s.active then
        return rune.style.gray("agent: stopped")
    end

    local summary = rune.agent_ui.summary()
    local state = s.thinking and rune.style.yellow("thinking") or rune.style.green("idle")
    local tokens = summary.input_tokens .. "/" .. summary.output_tokens .. " tok"
    if summary.cost then
        tokens = tokens .. string.format(" ($%.4f)", summary.cost)
    end
    local goal = s.goal and (" | " .. s.goal) or ""

    return "agent: " .. state .. " | " .. tokens .. " | " .. summary.last_action .. goal
end)

-- /agent [start [model] | stop] - status by default; start/stop
-- control added by explicit user request (T7 originally kept this
-- read-only "as a deliberate call" - see PLAN.md T7 - this is that
-- deliberate call, made later). Model resolution for "start":
-- explicit arg > RUNE_LLM_MODEL (.env, see 83_env.lua) > usage error.
local USAGE_AGENT = "[Usage] /agent [start [model] | stop]"

-- model/provider tag shared by the three places below that report
-- what's running: "started", "already running", and plain status.
-- Provider comes from rune.llm.provider() (86_llm.lua) - the same
-- RUNE_LLM_PROVIDER-or-"zen" resolution rune.llm.chat itself uses, not
-- re-derived here, so this can never drift out of sync with what a
-- turn actually talks to.
local function model_provider_tag(model)
    return "  (model: " .. tostring(model) .. ", provider: " .. rune.llm.provider() .. ")"
end

rune.command.add("agent", function(args)
    local sub, rest = args:match("^(%S*)%s*(.-)%s*$")

    if sub == "start" then
        if rune.agent.is_active() then
            rune.echo(rune.style.yellow("[Agent]") .. " already running" ..
                model_provider_tag(rune.agent.status().model))
            return
        end
        local model = rest ~= "" and rest or rune.env("RUNE_LLM_MODEL")
        if not model then
            rune.echo("[Usage] /agent start <model>  (or set RUNE_LLM_MODEL in .env)")
            return
        end
        rune.agent.start({ model = model })
        rune.echo(rune.style.green("[Agent]") .. " started" .. model_provider_tag(model))
        return
    end

    if sub == "stop" then
        if not rune.agent.is_active() then
            rune.echo(rune.style.gray("[Agent]") .. " already stopped")
            return
        end
        rune.agent.stop()
        rune.echo(rune.style.yellow("[Agent]") .. " stopped")
        return
    end

    if sub ~= "" then
        rune.echo(USAGE_AGENT)
        return
    end

    local s = rune.agent.status()
    local summary = rune.agent_ui.summary()
    if not s.active then
        rune.echo(rune.style.gray("[Agent]") .. " stopped")
        return
    end
    rune.echo(rune.style.green("[Agent]") .. " " .. (s.thinking and "thinking" or "idle") ..
        model_provider_tag(s.model))
    rune.echo("  goal: " .. (s.goal or rune.style.gray("(none yet)")))
    rune.echo("  tokens: " .. summary.input_tokens .. " in / " .. summary.output_tokens .. " out" ..
        (summary.cost and string.format("  (~$%.4f)", summary.cost) or ""))
    rune.echo("  last action: " .. summary.last_action)
end, "Show/control the agent: /agent [start [model] | stop] (reasoning streams inline as it plays)")
