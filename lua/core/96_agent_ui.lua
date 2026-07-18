-- Agent Observability (live mode)
-- Watches the agent core (87_agent.lua) through its agent_* hooks and
-- renders what it sees: a reasoning pane, a status-bar segment, and
-- per-turn rune.log entries (PLAN.md T7). Pure observer - never calls
-- back into rune.agent, never affects the turn loop, so a broken
-- render here can't derail a think (and per-hook quarantine means a
-- failing renderer just stops updating instead of spamming errors).
--
-- Registered unconditionally, like the core status bar in 95_ui.lua:
-- the "agent" pane/bar exist but are invisible to a plain human
-- session unless placed into rune.ui.layout (pane) or already are
-- (the status bar registry pattern - see 35_bars.lua), and every
-- handler here is driven entirely by hooks that T5 only ever fires
-- from inside a turn, which never runs unless something calls
-- rune.agent.start(). Nothing to opt into.
--
-- Add {name = "agent", height = N} to your rune.ui.layout to see the
-- reasoning pane; the bar shows up once "agent" is added to a layout
-- row the same way "status" is.

local PANE = "agent"

rune.pane.create(PANE)

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

local function pane_line(text)
    rune.pane.write(PANE, text)
end

local function log_line(text)
    rune.log.write("[Agent] " .. text)
end

rune.hooks.on("agent_turn_start", function()
    last_action = "thinking..."
    rune.pane.show(PANE)
    pane_line(rune.style.dim("--- turn start ---"))
    log_line("turn start")
end, { name = "agent-ui-turn-start" })

rune.hooks.on("agent_reply", function(reply)
    if reply.usage then
        total_input_tokens = total_input_tokens + (reply.usage.input_tokens or 0)
        total_output_tokens = total_output_tokens + (reply.usage.output_tokens or 0)
    end
    if reply.text and reply.text ~= "" then
        pane_line(reply.text)
        log_line("reply: " .. reply.text)
    end
end, { name = "agent-ui-reply" })

rune.hooks.on("agent_tool_call", function(name, input, result, is_error)
    local encoded_input = rune.json.encode(input) or tostring(input)
    local line = "[tool] " .. name .. "(" .. encoded_input .. ") -> " .. tostring(result)
    if is_error then
        last_action = "tool " .. name .. " failed"
        pane_line(rune.style.red(line))
    else
        last_action = "tool: " .. name
        pane_line(rune.style.cyan(line))
    end
    log_line(line .. (is_error and " (error)" or ""))
end, { name = "agent-ui-tool-call" })

rune.hooks.on("agent_turn_end", function(reply)
    last_action = "idle"
    pane_line(rune.style.dim("--- turn end (" .. tostring(reply.stop_reason) .. ") ---"))
    log_line("turn end: " .. tostring(reply.stop_reason))
end, { name = "agent-ui-turn-end" })

rune.hooks.on("agent_error", function(err)
    last_action = "error"
    pane_line(rune.style.red("[error] " .. err))
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

rune.ui.bar("agent", function(width)
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

-- /agent - read-only status (start/stop is a Lua API call, see T5;
-- this module only ever watches)
rune.command.add("agent", function(args)
    local s = rune.agent.status()
    local summary = rune.agent_ui.summary()
    if not s.active then
        rune.echo(rune.style.gray("[Agent]") .. " stopped")
        return
    end
    rune.echo(rune.style.green("[Agent]") .. " " .. (s.thinking and "thinking" or "idle"))
    rune.echo("  goal: " .. (s.goal or rune.style.gray("(none yet)")))
    rune.echo("  tokens: " .. summary.input_tokens .. " in / " .. summary.output_tokens .. " out" ..
        (summary.cost and string.format("  (~$%.4f)", summary.cost) or ""))
    rune.echo("  last action: " .. summary.last_action)
end, "Show agent status (reasoning pane: add {name='agent'} to your layout)")
