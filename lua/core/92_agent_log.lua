-- Agent Logging
-- Everything the log needs to be the bot's memory rather than just a
-- game transcript (PLAN.md T12), layered *on top of* 60_log.lua rather
-- than inside it. That file is upstream's; this one is the fork's, so
-- pulling upstream never conflicts over logging policy. Its header
-- documents exactly this seam ("register your own hooks against
-- rune._log.write for a different policy"), and everything below goes
-- through either that primitive or rune.log's public API.
--
-- Three additions, each off by default so a plain human /log session is
-- byte-for-byte what upstream produces:
--
--   - Timestamps (rune.log.timestamps(true)): wraps rune._log.write
--     itself, so *every* line gets the same "[HH:MM:SS] " prefix - game
--     output and agent reasoning alike. Wrapping the one primitive both
--     60_log.lua's hooks and rune.log.write already funnel through is
--     what makes this work without editing either. Correlating "which
--     server line was the bot reacting to" was the whole reason the
--     agent's reasoning got logged in the first place (T7), and
--     interleaved-but-unstamped lines only half-answer it.
--   - Agent chrome: 60_log.lua deliberately drops rune.echo output as
--     "client chrome" - right for /help spam, wrong for a bot, whose
--     echoed commands *are* the transcript. The gap was never the LLM's
--     own turns (96_agent_ui.lua already logs those): it was reflex
--     sends, which fire from a trigger with no LLM hop and so produce
--     no agent_tool_call, and policy notices (denied/rate-limited/
--     oscillation/budget), which only ever reached the screen.
--   - Headless auto-start: with no terminal there is no scrollback to
--     lose, so an unattended run opens a log unprompted.
--
-- Read-back (rune.log.read/search) lives here too rather than in
-- 60_log.lua for the same fork-boundary reason. It is what lets the
-- agent search its own history for context beyond the 200-line rolling
-- window rune.perception.transcript() carries (84_perception.lua) -
-- and it stays inside the fairness principle (PLAN.md §4) by
-- construction: the log holds only what already reached the screen plus
-- the bot's own reasoning, so reading it back is memory, never new
-- perception.

local green, red, dim = rune.style.green, rune.style.red, rune.style.gray

-- Caps on one read-back. Bounded because rune._log.read/search scan the
-- file synchronously on the session goroutine under the 5s watchdog,
-- and because anything read here is usually on its way into a prompt.
local DEFAULT_LINES = 50
local MAX_LINES = 500

-- Timestamps -----------------------------------------------------------

-- The Go primitive, captured before wrapping. Rebound from scratch on
-- every /reload (the VM is rebuilt and registerLogFuncs runs again), so
-- the wrapper can never stack on a previous wrapper.
local raw_write = rune._log.write
local stamping = false

rune._log.write = function(text)
    if stamping then
        raw_write(os.date("[%H:%M:%S] ") .. text)
    else
        raw_write(text)
    end
end

-- rune.log.timestamps(on) - prefix every logged line with the wall
-- clock. Returns the resulting setting; call with no argument to read
-- it. Applies to lines written from here on, not retroactively.
function rune.log.timestamps(on)
    if on ~= nil then
        stamping = not not on
    end
    return stamping
end

-- Read-back ------------------------------------------------------------

local function clamp(n, default)
    if n == nil then
        return default
    end
    if type(n) ~= "number" or n ~= n or n < 1 then
        error("line count must be a positive number, got " .. tostring(n), 3)
    end
    return math.min(math.floor(n), MAX_LINES)
end

-- rune.log.read(n) -> array of the last n log lines (oldest first),
-- or nil + error message when no log is open.
function rune.log.read(n)
    return rune._log.read(clamp(n, DEFAULT_LINES))
end

-- rune.log.search(pattern, n) -> array of up to the n most recent log
-- lines matching a regex (oldest first), or nil + error message when no
-- log is open or the pattern is invalid. Same regex engine as triggers.
function rune.log.search(pattern, n)
    if type(pattern) ~= "string" or pattern == "" then
        error("rune.log.search: pattern must be a non-empty string", 2)
    end
    return rune._log.search(pattern, clamp(n, DEFAULT_LINES))
end

-- Agent chrome ---------------------------------------------------------

-- Pure observers, exactly like 96_agent_ui.lua's: they only write to
-- the log, never call back into the agent. rune.log.write is a no-op
-- while no log is open, so these cost nothing in a session that never
-- started one.

-- Reflex sends and tool-driven sends both pass through
-- rune.agent_policy.send, which fires this for each one actually put on
-- the wire. The LLM's own tool calls are logged separately (and more
-- richly, with their arguments) by 96_agent_ui.lua; this is what makes
-- a reflex - which runs with no LLM turn anywhere near it - visible at
-- all.
rune.hooks.on("agent_send", function(cmd)
    rune.log.write("[agent] " .. cmd)
end, { name = "agent-log-send" })

rune.hooks.on("agent_policy", function(kind, message)
    rune.log.write("[agent-policy] " .. kind .. ": " .. message)
end, { name = "agent-log-policy" })

-- Headless auto-start --------------------------------------------------

-- rune.headless is set by Go before core scripts load (engine.go's
-- SetHeadless). A terminal session is left alone: starting a file
-- write nobody asked for would be exactly the unwanted side effect
-- 84_perception.lua's enable() and 96_agent_ui.lua's pane placement
-- both already avoid.
if rune.headless then
    rune.log.timestamps(true)
    local path, err = rune.log.start()
    if path then
        rune.echo(green("[Log]") .. " headless session logging to " .. path)
    else
        rune.echo(red("[Log]") .. " could not start headless log: " .. tostring(err))
    end
end

-- /loglines - read back or search the active log by hand, the same
-- view the agent's own read_log/search_log tools get.
rune.command.add("loglines", function(args)
    local sub, rest = args:match("^(%S*)%s*(.-)%s*$")

    local lines, err
    if sub == "search" then
        if rest == "" then
            rune.echo("[Usage] /loglines [n] | /loglines search <pattern>")
            return
        end
        lines, err = rune.log.search(rest, DEFAULT_LINES)
    else
        lines, err = rune.log.read(tonumber(sub) or 20)
    end

    if not lines then
        rune.echo(red("[Log]") .. " " .. tostring(err))
        return
    end
    if #lines == 0 then
        rune.echo(dim("[Log] no matching lines"))
        return
    end
    for _, line in ipairs(lines) do
        rune.echo(dim(line))
    end
end, "Read back the active log (/loglines [n], /loglines search <pattern>)")
