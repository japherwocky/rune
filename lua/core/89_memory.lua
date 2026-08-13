-- Memory
-- A durable, retrievable memory stream for the agent (PLAN.md T13),
-- adapted from Park et al.'s Generative Agents (arXiv:2304.03442).
-- Sits between perception (84_perception.lua, what is true *now*) and
-- the log (92_agent_log.lua, everything that ever scrolled past): this
-- file holds the small number of things worth *remembering*, scored and
-- retrieved on demand.
--
-- What it replaces: before this, the only state crossing a turn
-- boundary was `agent_goal` - the model's own last reply text, replayed
-- verbatim into the next observation. That one slot was carrying the
-- whole cross-turn memory load, which is why 87_agent.lua's goal
-- heading has to disclaim it as "your own words ... not confirmed
-- fact". Facts live here now; the goal can go back to being a goal.
--
-- Four deliberate departures from the paper, each because a MUD is not
-- a sandbox town:
--
--   - **Creation is salience-gated, not per-observation.** Their agents
--     perceived a handful of events per tick; a ROM combat round emits
--     lines faster than any of them could be worth storing. Automatic
--     records come only from the GMCP deltas below - first sight of a
--     room or a mob, a level, a close call, an inbound tell - plus
--     whatever the model chooses to write with `remember`.
--   - **Importance is a static table, not an LLM call.** The paper
--     rates every memory's poignancy 1-10 with its own model call. Here
--     that would be one extra request per memory, on a hot path, that
--     can fail mid-turn. Scoring by kind is free, deterministic, and
--     good enough to rank against; the model may still set its own on a
--     `remember` call, where it is already paying for the turn anyway.
--   - **Recency decays per minute, not per hour.** The paper's 0.995^h
--     ran over *sandbox* hours, where a simulated day passed in minutes
--     of real time. Applied to real hours the same constant barely
--     decays at all (0.89 after a full day). Per minute puts the
--     half-life near two hours, which is the scale a play session
--     actually runs on.
--   - **Relevance is keyword+tag overlap, behind a swappable seam.**
--     Theirs is embedding cosine similarity; no embedding provider
--     exists in this stack (Zen's catalog is chat models). Tags carry
--     most of the weight on purpose: "what do I know about room 3054"
--     is an exact-match question and a join answers it better than a
--     similarity search would. rune.memory.set_relevance swaps the
--     whole function when embeddings do show up - no caller changes,
--     the same boundary that let a second LLM provider land inside
--     86_llm.lua without touching the agent core.
--
-- Reflection is kept as-is and is the point of the whole exercise:
-- periodically, ask the model what its recent memories *mean* and store
-- the answers as memories in their own right. That is what turns three
-- separate "killed by the cityguard" observations into "cityguards are
-- fatal at this level" - knowledge a rolling transcript structurally
-- cannot produce. It runs outside the turn loop with its own
-- single-flight flag: 87_agent.lua's `thinking` guard does not cover
-- this call, and two concurrent requests is exactly what that guard
-- exists to prevent.
--
-- What is *not* here, deliberately: the paper's recursive planning tree
-- (daily chunks -> hourly -> 5-15 minute actions). It presumes a world
-- you control and a clock that matters; a MUD is interrupt-driven and
-- an aggro mob invalidates the plan every thirty seconds. PLAN.md §1
-- already settled this domain's answer - the LLM sets intent, the
-- scripting layer executes tactics.
--
-- Inert until rune.memory.enable(), which rune.agent.start calls -
-- exactly like 84_perception.lua, and for the same reason: a core file
-- is loaded in every session, and subscribing to GMCP or writing the
-- durable store are side effects a human who never started a bot did
-- not ask for.
--
-- Fairness (PLAN.md §4) holds by construction: every automatic record
-- is built from GMCP a human's client already receives, and everything
-- else is the agent's own words. Nothing here observes anything new.

rune.memory = {}

local STORE_KEY = "agent_memory"

-- The stream is capped, and that cap is the watchdog guarantee: recall
-- scores every record, so bounding the stream bounds the work at
-- O(MAX_RECORDS) no matter how long the bot has been running. Nothing
-- here scales with session length. If this ever needs to be much
-- larger, scoring moves to Go as a primitive the way T12's log search
-- did, rather than growing this loop.
local MAX_RECORDS = 400
local RECALL_DEFAULT = 6
local RECALL_MAX = 20

-- 0.995^minutes: ~0.55 at two hours, ~0.02 at eight. See the header for
-- why this is per minute where the paper's is per hour.
local DECAY_PER_MINUTE = 0.995

-- Reflection fires when importance accumulated since the last one
-- crosses this. The paper used 150, but its agents recorded every
-- observation; ours records far fewer and denser ones, so the same
-- number would mean reflecting roughly never.
local REFLECT_THRESHOLD = 50
local REFLECT_SAMPLE = 40
local REFLECT_INSIGHTS = 5
local REFLECT_MAX_TOKENS = 512

-- A "close call" is worth remembering; every combat tick under the
-- threshold is not. Armed again only after recovering past
-- HP_RECOVERED_RATIO, so one bad fight produces one memory.
local LOW_HP_RATIO = 0.15
local HP_RECOVERED_RATIO = 0.5

local IMPORTANCE = {
    reflection = 8,
    level      = 7,
    close_call = 6,
    tell       = 5,
    note       = 4,
    enemy      = 3,
    room       = 2,
}
local DEFAULT_IMPORTANCE = 4

-- Weights on the three retrieval factors, all 1.0 as in the paper.
-- Exposed as a plain table so a deployer can lean the bot toward, say,
-- relevance over recency without editing this file.
rune.memory.weights = { recency = 1.0, importance = 1.0, relevance = 1.0 }

-- ...except when someone asked an actual question. With equal weights a
-- perfect relevance hit is worth exactly as much as being the most
-- important memory, so a high-importance non-match ties with the record
-- that literally contains the search term - and the tiebreak (newer
-- wins) then puts the wrong one on top. That balance is right for the
-- automatic retrieval feeding each turn's observation, which is asking
-- "what should be on your mind", but wrong for the `recall` tool and
-- /memory search, which are asking "what do you know about X".
-- Explicit-query paths pass these instead.
rune.memory.query_weights = { recency = 0.5, importance = 0.5, relevance = 2.0 }

-- Records ---------------------------------------------------------------

-- Persisted as { next_id, pending, records }, not a bare array: ids must
-- survive eviction (a reflection's `refs` point at them) and the
-- reflection counter must survive a restart, or a long-running bot
-- reflects on a clock that resets every time it reconnects.
local loaded = false
local records = {}
local next_id = 1
local pending_importance = 0

local function load()
    if loaded then return end
    loaded = true
    local data = rune.store.get(STORE_KEY)
    if type(data) == "table" and type(data.records) == "table" then
        records = data.records
        next_id = tonumber(data.next_id) or (#records + 1)
        pending_importance = tonumber(data.pending) or 0
    end
end

local function save()
    rune.store.set(STORE_KEY, {
        next_id = next_id,
        pending = pending_importance,
        records = records,
    })
end

-- Reflections are the compressed form of the observations they cite, so
-- dropping them to keep their own sources would be exactly backwards.
-- Falls back to the oldest record of any kind if the stream somehow
-- holds nothing but reflections - a cap that cannot be enforced is not
-- a cap.
local function evict()
    while #records > MAX_RECORDS do
        local victim = 1
        for i, rec in ipairs(records) do
            if rec.kind ~= "reflection" then
                victim = i
                break
            end
        end
        table.remove(records, victim)
    end
end

local function clamp_importance(n)
    n = tonumber(n)
    if not n or n ~= n then return nil end
    return math.max(1, math.min(10, math.floor(n)))
end

-- rune.memory.record(kind, text, opts) -> record
-- opts: { importance? (1-10, defaults from the kind), tags? (array of
-- "room:3054"/"mob:kobold"/"player:bob" strings), refs? (record ids a
-- reflection was drawn from) }. Writes through to rune.store
-- immediately: that is a synchronous file write, which is affordable
-- only because creation is salience-gated - the same tradeoff
-- 84_perception.lua's record_room already makes.
function rune.memory.record(kind, text, opts)
    if type(kind) ~= "string" or kind == "" then
        error("rune.memory.record: kind must be a non-empty string", 2)
    end
    if type(text) ~= "string" or text == "" then
        error("rune.memory.record: text must be a non-empty string", 2)
    end
    opts = opts or {}
    load()

    local tags = {}
    if type(opts.tags) == "table" then
        for _, tag in ipairs(opts.tags) do
            if type(tag) == "string" and tag ~= "" then
                table.insert(tags, tag)
            end
        end
    end

    local rec = {
        id = next_id,
        t = os.time(),
        kind = kind,
        text = text,
        importance = clamp_importance(opts.importance) or IMPORTANCE[kind] or DEFAULT_IMPORTANCE,
        tags = tags,
        refs = opts.refs,
    }
    next_id = next_id + 1
    table.insert(records, rec)
    evict()
    pending_importance = pending_importance + rec.importance
    save()

    rune.hooks.call("agent_memory", rec)
    return rec
end

-- rune.memory.all() -> a copy of the stream, oldest first.
function rune.memory.all()
    load()
    local out = {}
    for i, rec in ipairs(records) do
        out[i] = rec
    end
    return out
end

-- rune.memory.forget_all() - drop everything, including reflections.
function rune.memory.forget_all()
    load()
    records = {}
    next_id = 1
    pending_importance = 0
    save()
end

-- Retrieval -------------------------------------------------------------

local STOPWORDS = {
    ["the"] = true, ["and"] = true, ["for"] = true, ["with"] = true,
    ["you"] = true, ["your"] = true, ["that"] = true, ["this"] = true,
    ["was"] = true, ["are"] = true, ["from"] = true, ["have"] = true,
    ["has"] = true, ["into"] = true, ["its"] = true, ["not"] = true,
}

local function word_set(s)
    local out = {}
    for w in tostring(s or ""):lower():gmatch("[%a][%w'_]*") do
        if #w >= 3 and not STOPWORDS[w] then
            out[w] = true
        end
    end
    return out
end

-- The default relevance function: tag overlap first, word overlap as
-- the tiebreak. Replaceable wholesale via set_relevance - see the
-- header. Signature: fn(record, query) -> 0..1, where query is
-- { text = string|nil, tags = array }.
local function default_relevance(rec, query)
    local score = 0

    local qtags = query.tags
    if qtags and #qtags > 0 and rec.tags and #rec.tags > 0 then
        local wanted = {}
        for _, tag in ipairs(qtags) do wanted[tag] = true end
        local hits = 0
        for _, tag in ipairs(rec.tags) do
            if wanted[tag] then hits = hits + 1 end
        end
        if hits > 0 then
            score = score + 0.7 * math.min(1, hits / #qtags)
        end
    end

    if query.text and query.text ~= "" then
        local qwords = query.text_words or word_set(query.text)
        local total, hits = 0, 0
        local rwords = word_set(rec.text)
        for w in pairs(qwords) do
            total = total + 1
            if rwords[w] then hits = hits + 1 end
        end
        if total > 0 then
            score = score + 0.3 * (hits / total)
        end
    end

    return math.min(1, score)
end

local relevance_fn = default_relevance

-- rune.memory.set_relevance(fn) - swap the relevance factor. Pass nil to
-- restore the default. This is the seam an embedding-backed similarity
-- drops into later without any caller knowing.
function rune.memory.set_relevance(fn)
    if fn ~= nil and type(fn) ~= "function" then
        error("rune.memory.set_relevance: fn must be a function or nil", 2)
    end
    relevance_fn = fn or default_relevance
end

-- Min-max to [0,1], as the paper specifies. A constant vector maps to
-- all-1 rather than 0/0: the factor simply carries no information, and
-- adding the same constant to every score leaves the ranking untouched.
local function normalize(scored, field)
    local min, max
    for _, s in ipairs(scored) do
        local v = s[field]
        if not min or v < min then min = v end
        if not max or v > max then max = v end
    end
    local span = (max or 0) - (min or 0)
    for _, s in ipairs(scored) do
        s[field] = span > 0 and ((s[field] - min) / span) or 1
    end
end

-- rune.memory.context_tags() -> tags describing the situation right
-- now, from perception. This is what makes retrieval a join rather than
-- a similarity search: the agent already knows which room it is in and
-- what it is fighting, so "what do I know about *this*" is answerable
-- exactly.
function rune.memory.context_tags()
    local tags = {}
    if not rune.perception.is_enabled() then
        return tags
    end
    local snap = rune.perception.snapshot()
    if snap.room then
        if snap.room.num then table.insert(tags, "room:" .. tostring(snap.room.num)) end
        if snap.room.area then table.insert(tags, "area:" .. tostring(snap.room.area)) end
    end
    if snap.status and snap.status.enemy then
        table.insert(tags, "mob:" .. tostring(snap.status.enemy):lower())
    end
    return tags
end

-- rune.memory.recall(opts) -> array of records, best first.
-- opts: { limit? (default 6, max 20), text? (free-text query), tags?
-- (defaults to context_tags()), weights? (defaults to
-- rune.memory.weights; see query_weights for the explicit-question
-- case) }.
--
--   score = w.recency*recency + w.importance*importance + w.relevance*relevance
--
-- with each factor min-max normalized across the candidates first.
function rune.memory.recall(opts)
    opts = opts or {}
    load()
    if #records == 0 then
        return {}
    end

    local limit = tonumber(opts.limit) or RECALL_DEFAULT
    limit = math.max(1, math.min(math.floor(limit), RECALL_MAX))

    local query = {
        text = opts.text,
        tags = opts.tags or rune.memory.context_tags(),
    }
    if query.text then
        query.text_words = word_set(query.text)
    end

    local now = os.time()
    local scored = {}
    for _, rec in ipairs(records) do
        local minutes = math.max(0, (now - (tonumber(rec.t) or now)) / 60)
        local ok, relevance = pcall(relevance_fn, rec, query)
        table.insert(scored, {
            rec = rec,
            recency = DECAY_PER_MINUTE ^ minutes,
            importance = (tonumber(rec.importance) or DEFAULT_IMPORTANCE) / 10,
            relevance = (ok and tonumber(relevance)) or 0,
        })
    end

    normalize(scored, "recency")
    normalize(scored, "importance")
    normalize(scored, "relevance")

    local w = opts.weights or rune.memory.weights or {}
    for _, s in ipairs(scored) do
        s.score = (w.recency or 1) * s.recency
                + (w.importance or 1) * s.importance
                + (w.relevance or 1) * s.relevance
    end

    -- Ties break toward the newer record: same score, more recent wins.
    table.sort(scored, function(a, b)
        if a.score == b.score then
            return (a.rec.id or 0) > (b.rec.id or 0)
        end
        return a.score > b.score
    end)

    local out = {}
    for i = 1, math.min(limit, #scored) do
        out[i] = scored[i].rec
    end
    return out
end

local function ago(t)
    local seconds = os.time() - (tonumber(t) or os.time())
    if seconds < 90 then return "just now" end
    local minutes = math.floor(seconds / 60)
    if minutes < 90 then return minutes .. "m ago" end
    return math.floor(minutes / 60) .. "h ago"
end

-- rune.memory.format(recs) -> one "- [when] text" line per record, the
-- shape both the prompt section and /memory render.
function rune.memory.format(recs)
    local lines = {}
    for _, rec in ipairs(recs or {}) do
        local mark = rec.kind == "reflection" and "*" or "-"
        table.insert(lines, mark .. " [" .. ago(rec.t) .. "] " .. tostring(rec.text))
    end
    return lines
end

-- Reflection ------------------------------------------------------------

local REFLECT_SYSTEM =
    "You are the memory of an agent playing a MUD. Given its recent memories, " ..
    "state what they add up to: durable, reusable conclusions that will still be " ..
    "true and useful later. Prefer specifics that change what it should do - which " ..
    "enemies are dangerous, which routes lead where, which tactics worked, who is " ..
    "friendly. Write one conclusion per line, no numbering, no preamble, nothing " ..
    "else. If the memories support no real conclusion, write nothing at all."

local function parse_insights(text)
    local out = {}
    for line in tostring(text or ""):gmatch("[^\n]+") do
        local cleaned = line:gsub("^%s*[%-%*%d%.%)%s]+", ""):gsub("%s+$", "")
        if #cleaned >= 8 then
            table.insert(out, cleaned)
            if #out >= REFLECT_INSIGHTS then break end
        end
    end
    return out
end

local reflecting = false

-- rune.memory.reflect() - one reflection pass. Its own single-flight
-- flag (87_agent.lua's `thinking` does not cover this call), and it
-- deliberately resets the counter when the request goes *out*, not when
-- it comes back: a reflection that errors must not leave the threshold
-- still tripped and retry on every subsequent turn end. Returns true if
-- a request was actually sent.
function rune.memory.reflect()
    load()
    if reflecting or #records == 0 then
        return false
    end
    local status = rune.agent.status()
    if not status.active or not status.model then
        return false
    end

    local lines, refs = {}, {}
    for i = math.max(1, #records - REFLECT_SAMPLE + 1), #records do
        table.insert(lines, "- " .. tostring(records[i].text))
        table.insert(refs, records[i].id)
    end

    reflecting = true
    pending_importance = 0
    save()

    rune.llm.chat({
        model = status.model,
        system = REFLECT_SYSTEM,
        messages = { { role = "user", content =
            "Recent memories:\n" .. table.concat(lines, "\n") ..
            "\n\nWhat " .. REFLECT_INSIGHTS .. " or fewer high-level conclusions do these support?" } },
        max_tokens = REFLECT_MAX_TOKENS,
    }, function(reply, err)
        reflecting = false
        if err then
            rune.hooks.call("agent_error", "reflection: " .. tostring(err))
            return
        end
        local insights = parse_insights(reply and reply.text)
        for _, insight in ipairs(insights) do
            rune.memory.record("reflection", insight, { refs = refs })
        end
        -- The reflections just recorded added their own importance back
        -- onto the counter; clear it so reflecting can never feed itself.
        pending_importance = 0
        save()
        rune.hooks.call("agent_reflection", insights, reply)
    end)

    return true
end

-- Automatic capture ------------------------------------------------------

-- Session-local "already recorded" sets, seeded from the stream on
-- enable(). Kept separately from the stream so eviction cannot cause a
-- room the bot has known all session to be rediscovered and written
-- again.
local known_rooms = {}
local known_mobs = {}
local last_level = nil
local hp_low_armed = true

local function seed_known()
    for _, rec in ipairs(records) do
        for _, tag in ipairs(rec.tags or {}) do
            if tag:sub(1, 5) == "room:" then
                known_rooms[tag:sub(6)] = true
            elseif tag:sub(1, 4) == "mob:" then
                known_mobs[tag:sub(5)] = true
            end
        end
    end
end

local function on_room(data)
    if not data or not data.num then return end
    local key = tostring(data.num)
    if known_rooms[key] then return end
    known_rooms[key] = true

    local tags = { "room:" .. key }
    if data.area then table.insert(tags, "area:" .. tostring(data.area)) end
    rune.memory.record("room", "Found " .. tostring(data.name or "an unnamed room") ..
        (data.area and (" in " .. tostring(data.area)) or "") .. " (room " .. key .. ").",
        { tags = tags })
end

local function on_status(data)
    if not data then return end

    local level = tonumber(data.level)
    if level and last_level and level > last_level then
        rune.memory.record("level", "Reached level " .. level .. ".", { tags = rune.memory.context_tags() })
    end
    if level then last_level = level end

    if data.enemy then
        local mob = tostring(data.enemy):lower()
        if not known_mobs[mob] then
            known_mobs[mob] = true
            local tags = rune.memory.context_tags()
            table.insert(tags, "mob:" .. mob)
            rune.memory.record("enemy", "First fight with " .. tostring(data.enemy) .. ".", { tags = tags })
        end
    end
end

local function on_vitals(data)
    if not data or not data.hp or not data.maxhp or data.maxhp <= 0 then return end
    local ratio = data.hp / data.maxhp
    if ratio >= HP_RECOVERED_RATIO then
        hp_low_armed = true
        return
    end
    if ratio < LOW_HP_RATIO and hp_low_armed then
        hp_low_armed = false
        local snap = rune.perception.snapshot()
        rune.memory.record("close_call",
            "Nearly died (" .. data.hp .. "/" .. data.maxhp .. " hp)" ..
            (snap.status and snap.status.enemy and (" fighting " .. tostring(snap.status.enemy)) or "") ..
            ((snap.room and snap.room.name) and (" in " .. tostring(snap.room.name)) or "") .. ".",
            { tags = rune.memory.context_tags() })
    end
end

-- Tells only. Gossip and the other broadcast channels are the MUD's
-- background noise; a message addressed to this character specifically
-- is the one that tends to matter later.
local function on_comm(data)
    if not data or data.chan ~= "tell" then return end
    local who = data.player and tostring(data.player) or nil
    rune.memory.record("tell",
        (who and (who .. " told me: ") or "Was told: ") .. tostring(data.msg or ""),
        { tags = who and { "player:" .. who:lower() } or nil })
end

local function on_turn_end()
    if pending_importance >= REFLECT_THRESHOLD then
        rune.memory.reflect()
    end
end

-- Lifecycle --------------------------------------------------------------

local active = false
local handles = {}

-- rune.memory.enable() - wire the automatic capture and the reflection
-- trigger. Called by rune.agent.start; inert otherwise, exactly like
-- rune.perception.enable(). Idempotent. Does not subscribe to GMCP
-- itself - perception already did, and rune.gmcp.on is a separate
-- concern from the subscription.
function rune.memory.enable()
    if active then return end
    active = true
    load()
    seed_known()

    handles = {
        rune.gmcp.on("Room.Info", on_room, { name = "memory-room" }),
        rune.gmcp.on("Char.Status", on_status, { name = "memory-status" }),
        rune.gmcp.on("Char.Vitals", on_vitals, { name = "memory-vitals" }),
        rune.gmcp.on("Comm.Channel", on_comm, { name = "memory-comm" }),
        rune.hooks.on("agent_turn_end", on_turn_end, { name = "memory-reflect" }),
    }
end

-- rune.memory.disable() - unwinds enable(). The stream itself is
-- durable and is deliberately left alone: this stops recording, it does
-- not forget (see forget_all for that).
function rune.memory.disable()
    if not active then return end
    active = false
    for _, handle in ipairs(handles) do
        handle:remove()
    end
    handles = {}
end

function rune.memory.is_enabled()
    return active
end

-- rune.memory.status() - a read-only snapshot for tests and /memory.
function rune.memory.status()
    load()
    local reflections = 0
    for _, rec in ipairs(records) do
        if rec.kind == "reflection" then reflections = reflections + 1 end
    end
    return {
        active = active,
        count = #records,
        reflections = reflections,
        pending_importance = pending_importance,
        reflect_threshold = REFLECT_THRESHOLD,
        reflecting = reflecting,
        max_records = MAX_RECORDS,
    }
end

-- Tools ------------------------------------------------------------------

-- Registered through 88_agent_tools.lua's helper so these get the same
-- name/source/quarantine machinery as send_command and the rest.
local register = rune.agent_tools.register

register("remember",
    "Save something worth remembering later - a fact, a lesson, a name, a route, " ..
    "who is friendly. Use it the moment you learn something that would still matter " ..
    "an hour from now; your recent-output window is short and this is not.", {
    type = "object",
    properties = {
        text = { type = "string", description = "The fact, in one sentence, phrased so it makes sense on its own later." },
        importance = { type = "number", description = "1-10: how much it matters. 1 mundane, 5 useful, 9 critical. Default 4." },
        tags = {
            type = "array",
            items = { type = "string" },
            description = "Optional labels to find it by later, e.g. \"room:3054\", \"mob:cityguard\", \"player:bob\".",
        },
    },
    required = { "text" },
}, function(input)
    if type(input) ~= "table" or type(input.text) ~= "string" or input.text == "" then
        error("remember: input.text must be a non-empty string")
    end
    local tags = input.tags
    if tags == nil then
        tags = rune.memory.context_tags()
    end
    local rec = rune.memory.record("note", input.text, { importance = input.importance, tags = tags })
    return "remembered (#" .. rec.id .. ")"
end)

register("recall",
    "Search your own memory for what you have learned - facts you saved, places and " ..
    "enemies you have met, and the conclusions you drew from them. Ranked by how " ..
    "recent, important, and relevant each memory is. Unlike search_log this returns " ..
    "what you learned, not the raw lines you saw.", {
    type = "object",
    properties = {
        query = { type = "string", description = "What you want to remember about, in a few words." },
        limit = { type = "number", description = "How many memories to return. Default 6, max 20." },
    },
}, function(input)
    local query = type(input) == "table" and input.query or nil
    local limit = type(input) == "table" and input.limit or nil
    local recs = rune.memory.recall({
        text = query,
        limit = limit,
        -- An explicit question: rank by what it asked about, not by
        -- what happens to be important right now.
        weights = query and rune.memory.query_weights or nil,
    })
    if #recs == 0 then
        return "no memories yet"
    end
    return table.concat(rune.memory.format(recs), "\n")
end)

-- /memory - the same view the agent's own tools get.
rune.command.add("memory", function(args)
    local sub, rest = args:match("^(%S*)%s*(.-)%s*$")
    local green, dim, yellow = rune.style.green, rune.style.gray, rune.style.yellow

    if sub == "reflect" then
        if rune.memory.reflect() then
            rune.echo(green("[Memory]") .. " reflecting...")
        else
            rune.echo(dim("[Memory] nothing to reflect on (or already reflecting, or agent stopped)"))
        end
        return
    end

    if sub == "forget" then
        rune.memory.forget_all()
        rune.echo(yellow("[Memory]") .. " forgot everything")
        return
    end

    local s = rune.memory.status()
    local recs
    if sub == "search" and rest ~= "" then
        recs = rune.memory.recall({ text = rest, limit = RECALL_MAX, weights = rune.memory.query_weights })
    else
        recs = rune.memory.recall({ limit = tonumber(sub) or 10 })
    end

    rune.echo(green("[Memory]") .. dim(" " .. s.count .. " records, " .. s.reflections ..
        " reflections, " .. s.pending_importance .. "/" .. s.reflect_threshold .. " to next reflection"))
    if #recs == 0 then
        rune.echo(dim("  (nothing remembered yet)"))
        return
    end
    for _, line in ipairs(rune.memory.format(recs)) do
        rune.echo(dim("  " .. line))
    end
end, "Show/search agent memory (/memory [n] | search <text> | reflect | forget)")
