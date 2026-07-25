-- Agent Governance
-- Safety nets around the agent core (87_agent.lua) and its tools
-- (88_agent_tools.lua) - PLAN.md T9. A tool-calling agent can spam the
-- MUD, drain a budget, or get stuck in a loop just as easily as it can
-- play well. Four independent mechanisms:
--
--   - Rate limit: caps agent-attributed outgoing sends (send_command
--     and speak - NOT human input, see "Why not wrap rune.send" below)
--     to config.max_commands_per_second (default 3), fixed 1-second
--     window via os.time(). On by default - a safety net, not an
--     opt-in. "Covers channel output too" (PLAN.md) falls out for
--     free: speak() funnels through the same send() as send_command.
--   - Irreversible-command gate: a denylist of regex patterns checked
--     against the full outgoing command text; defaults to a bare
--     "quit". Deny-only, not deny-or-confirm - an interactive confirm
--     needs a UI affordance neither run mode has yet (T10/T11); denying
--     is the strictly safer half and fully satisfies "contained".
--   - Oscillation: the last oscillation_window (default 4)
--     agent-attributed sends, if they're all the *same command* sent
--     while perception.snapshot() stayed byte-identical every time,
--     means nothing is changing in response to repeating it - wakes the
--     LLM and clears the window. Detects same-command (period-1)
--     repetition specifically, not arbitrary-period cycles ("A, B, A,
--     B, ...") - the dominant real failure mode (the model repeating
--     one tool call) and far simpler to test.
--   - Budget: accumulates reply.usage into a running $ total and stops
--     the agent once config.budget_usd is hit. Off by default (nil,
--     like 96_agent_ui.lua's own pricing slot) - there is no safe
--     universal default for "how much may this bot spend", and
--     fabricating one from guessed pricing would be worse than not
--     enforcing at all (same reasoning T7 already documented). Tracks
--     its own tokens/pricing independently of 96_agent_ui.lua's display
--     accumulator - governance must keep working even if observability
--     were ever stripped out. A deployer wanting both the bar's display
--     AND a real cap sets rune.agent_ui.pricing and
--     rune.agent_policy.configure{pricing=...} separately; they are
--     intentionally not linked.
--
-- Why not wrap rune.send: rune.send/rune.send_raw are shared with
-- ordinary human input (a human typing "quit" at the prompt must never
-- be blocked by the agent's own denylist, and a human's typing speed
-- must never be capped by the agent's rate limit) - governance can only
-- live at the agent-attribution boundary, i.e. the specific call sites
-- that originate from the agent's own tools, not the shared primitive
-- every keystroke eventually reaches. rune.agent_policy.send is that
-- boundary; 88_agent_tools.lua calls it instead of rune.send directly
-- from send_command and speak.
--
-- Caveat: rune.send itself still expands ";"-chains and "#N" repeats
-- *after* this choke point, so one governed call can still put more
-- than one line on the wire. Not a regression, just not fully closed by
-- the limiter - it counts governed calls, not wire lines.
--
-- Despite the plan's "[Lua + small Go]" label, no new Go primitive
-- turned out to be necessary: os.time() (already available to every
-- script, see 60_log.lua's os.date use) covers the rate limiter's
-- clock.

rune.agent_policy = {}

local config = {
    budget_usd = nil,
    pricing = nil,               -- { input_per_million, output_per_million }
    max_commands_per_second = 3,
    oscillation_window = 4,
}

local denylist = { "^quit$" }

-- rune.agent_policy.configure(opts) - merge any of budget_usd, pricing,
-- max_commands_per_second, oscillation_window onto the current config.
-- Safe to call any time, before or after rune.agent.start(); every
-- field is read lazily, not snapshotted.
function rune.agent_policy.configure(opts)
    opts = opts or {}
    if opts.budget_usd ~= nil then config.budget_usd = opts.budget_usd end
    if opts.pricing ~= nil then config.pricing = opts.pricing end
    if opts.max_commands_per_second ~= nil then config.max_commands_per_second = opts.max_commands_per_second end
    if opts.oscillation_window ~= nil then config.oscillation_window = opts.oscillation_window end
end

-- rune.agent_policy.deny(pattern) - add a Go-regexp pattern to the
-- outgoing command denylist (matched against the full command text via
-- rune.regex.match, same engine/caching as triggers/aliases). Raises on
-- an invalid pattern, same convention as rune.trigger.regex.
function rune.agent_policy.deny(pattern)
    local ok, err = rune.regex.validate(pattern)
    if not ok then
        error("invalid denylist pattern '" .. tostring(pattern) .. "': " .. tostring(err), 2)
    end
    table.insert(denylist, pattern)
end

local function notify(kind, message)
    rune.echo(rune.style.yellow("[agent-policy]") .. " " .. message)
    rune.hooks.call("agent_policy", kind, message)
end

local function is_denied(cmd)
    for _, pattern in ipairs(denylist) do
        if rune.regex.match(pattern, cmd) then
            return true
        end
    end
    return false
end

-- Fixed 1-second window, not a true sliding window (a burst can
-- straddle a boundary) - good-citizen throttling doesn't need
-- leaky-bucket precision. rate_limit_notified_this_window caps the
-- echo/hook to once per window even under a sustained flood (every
-- individual denial is still reflected in send()'s own return value,
-- just not separately echoed) - the same "report once" instinct as
-- rune.regex.match's entry.reported for a bad pattern.
local window_second = nil
local sent_in_window = 0
local rate_limit_notified_this_window = false

local function rate_limit_ok()
    local now = os.time()
    if now ~= window_second then
        window_second = now
        sent_in_window = 0
        rate_limit_notified_this_window = false
    end
    if sent_in_window >= config.max_commands_per_second then
        return false
    end
    sent_in_window = sent_in_window + 1
    return true
end

-- Ring of the last (up to) oscillation_window commands this module has
-- actually sent, each paired with a JSON encoding of perception state
-- at that moment - cheap string equality stands in for a real diff,
-- the same technique 87_agent.lua already uses to embed the same
-- snapshot into a prompt.
local recent = {}

-- Returns true if the ring is now full of oscillation_window entries
-- that are all this exact command with this exact state. Only called
-- right after actually sending cmd, so `state` is the state *entering*
-- this repeat, not a re-sample after some delay - a simple, good-enough
-- proxy for "sending this again had no effect", not a rigorous diff.
local function record_and_check_oscillation(cmd)
    local state = rune.json.encode(rune.perception.snapshot()) or ""
    table.insert(recent, { cmd = cmd, state = state })
    while #recent > config.oscillation_window do
        table.remove(recent, 1)
    end
    if #recent < config.oscillation_window then
        return false
    end
    for _, entry in ipairs(recent) do
        if entry.cmd ~= cmd or entry.state ~= state then
            return false
        end
    end
    return true
end

-- rune.agent_policy.send(cmd) - the one path send_command and speak
-- (88_agent_tools.lua) use instead of rune.send directly (see the
-- header for why rune.send itself is never wrapped). Applies the
-- denylist, then the rate limit, then - once actually sent - echoes
-- the command into the main game window (rune.send itself never
-- echoes anything, human-typed or not, so without this the only
-- record of what the agent sent would be the is_error-free tool_result
-- the model itself sees, never the human's own screen) and tracks
-- oscillation.
--
-- Returns true on success, or nil + a reason string + a short category
-- ("denied" or "rate_limited") on refusal - send_command and speak
-- both raise on either, which is fine: reaching 3 consecutive
-- rate-limited *tool* calls in a row would require the model itself to
-- retry blindly 3 times despite each one saying so, which is closer to
-- oscillation than bad luck.
function rune.agent_policy.send(cmd)
    if is_denied(cmd) then
        local msg = 'blocked "' .. cmd .. '" (denylisted)'
        notify("denied_command", msg)
        return nil, msg, "denied"
    end

    if not rate_limit_ok() then
        local msg = 'rate limited: dropped "' .. cmd .. '"'
        if not rate_limit_notified_this_window then
            rate_limit_notified_this_window = true
            notify("rate_limited", msg)
        end
        return nil, msg, "rate_limited"
    end

    -- Echoed into the main game window so the command lands inline in
    -- the same transcript as the server's reaction to it. The hook
    -- carries the same fact to observers that need it in a durable
    -- form rather than on screen: 60_log.lua drops rune.echo output as
    -- client chrome, so 92_agent_log.lua listens here to get every
    -- governed send into the session log too.
    rune.echo(rune.style.gray("[agent] ") .. cmd)
    rune.hooks.call("agent_send", cmd)
    rune.send(cmd)

    if record_and_check_oscillation(cmd) then
        recent = {}
        local msg = 'stuck: "' .. cmd .. '" repeated ' .. config.oscillation_window ..
            'x with no change in perception - waking to re-plan'
        notify("oscillation", msg)
        rune.agent.wake("oscillation")
    end

    return true
end

-- Budget: see the header for why this is a second, independent
-- accumulator rather than reading 96_agent_ui.lua's.
local total_input_tokens = 0
local total_output_tokens = 0

local function accumulated_cost()
    if not config.pricing then
        return nil
    end
    return (total_input_tokens / 1e6) * (config.pricing.input_per_million or 0)
         + (total_output_tokens / 1e6) * (config.pricing.output_per_million or 0)
end

-- Fires on every hop (including intermediate tool_use ones), same as
-- 96_agent_ui.lua's own accumulator, so a many-hop turn is checked as
-- it goes rather than only once at the end. This can stop the agent
-- mid-turn: on_reply (87_agent.lua) does not re-check `active` between
-- firing this hook and continuing a tool_use turn, so one more
-- in-flight hop can still go out after the cap is hit, before the very
-- next on_reply's existing "not active" guard drops it - a firm
-- backstop, not a laser-precise cutoff, and an accepted tradeoff rather
-- than a reason to touch 87_agent.lua's already-shipped turn loop.
rune.hooks.on("agent_reply", function(reply)
    if not reply.usage then
        return
    end
    total_input_tokens = total_input_tokens + (reply.usage.input_tokens or 0)
    total_output_tokens = total_output_tokens + (reply.usage.output_tokens or 0)

    if not config.budget_usd then
        return
    end
    local cost = accumulated_cost()
    if cost and cost >= config.budget_usd then
        notify("budget_paused", string.format(
            "budget cap reached ($%.4f >= $%.4f) - stopping the agent", cost, config.budget_usd))
        rune.agent.stop()
    end
end, { name = "agent-policy-budget" })

-- A read-only snapshot for tests and /policy.
function rune.agent_policy.status()
    local denylist_copy = {}
    for i, pattern in ipairs(denylist) do
        denylist_copy[i] = pattern
    end
    return {
        budget_usd = config.budget_usd,
        input_tokens = total_input_tokens,
        output_tokens = total_output_tokens,
        cost = accumulated_cost(),
        max_commands_per_second = config.max_commands_per_second,
        sent_in_window = sent_in_window,
        oscillation_window = config.oscillation_window,
        denylist = denylist_copy,
    }
end

rune.command.add("policy", function(args)
    local s = rune.agent_policy.status()
    rune.echo(rune.style.green("[Policy]"))
    if s.budget_usd then
        rune.echo(string.format("  budget: $%.4f / $%.4f", s.cost or 0, s.budget_usd))
    else
        rune.echo("  budget: " .. rune.style.gray("(no cap configured)"))
    end
    rune.echo("  rate limit: " .. s.sent_in_window .. "/" .. s.max_commands_per_second .. " this second")
    rune.echo("  oscillation window: " .. s.oscillation_window)
    rune.echo("  denylist: " .. table.concat(s.denylist, ", "))
end, "Show agent governance status (budget/rate-limit/denylist)")
