-- Perception
-- The agent's sensors: a compact, always-current view of what a human
-- player with a capable GMCP client would see on screen right now (see
-- PLAN.md T3 and the fairness principle in botmud#20). Built only from
-- rune.gmcp packages and the "output" hook - nothing here reads or
-- infers anything a human player couldn't already see.
--
-- Named rune.perception, not rune.world: 65_worlds.lua already owns
-- rune.world for MUD server bookmarks (/world add, /connect).
--
-- Each GMCP package here (Char.Vitals, Char.Status, Room.Info) is a
-- full snapshot of its slice of state, not a delta - the spec emits
-- every relevant field on every update and simply omits fields that no
-- longer apply (e.g. Char.Status drops "enemy"/"enemy_condition" once
-- combat ends). So ingestion replaces the corresponding local, it never
-- merges - a merge would leave stale fields (a "phantom" enemy) behind
-- forever.

rune.perception = {}

local TRANSCRIPT_CAP = 200
local CHANNEL_CAP = 20

local vitals = {}
local status = {}
local room = {}
local transcript = {}
local channels = {}

-- The room the player was last known to be in, and the movement
-- command (if any) that was the most recent input. Together these let
-- a Room.Info naming a *different* room be recorded as a learned
-- dir -> destination edge - the map is only ever learned by walking
-- (see the fairness principle), never inferred from exits alone.
-- last_dir is set (or cleared) on every single input, so a stale
-- direction from an earlier blocked/failed move can never be
-- attributed to a later, unrelated room change (e.g. recall/teleport).
local last_room_num = nil
local last_dir = nil

-- ROM's fixed movement vocabulary (botmud is a ROM 2.4b6 derivative).
-- Both the abbreviation and the full word map to the canonical name so
-- either form is recognized regardless of what the player/agent types.
local DIRECTIONS = {
    n = "north", north = "north",
    s = "south", south = "south",
    e = "east", east = "east",
    w = "west", west = "west",
    u = "up", up = "up",
    d = "down", down = "down",
    ne = "northeast", northeast = "northeast",
    nw = "northwest", northwest = "northwest",
    se = "southeast", southeast = "southeast",
    sw = "southwest", southwest = "southwest",
}

local function shallow_copy(t)
    local out = {}
    for k, v in pairs(t) do
        out[k] = v
    end
    return out
end

local function array_copy(t)
    local out = {}
    for i, v in ipairs(t) do
        out[i] = v
    end
    return out
end

local function ring_append(list, item, cap)
    table.insert(list, item)
    while #list > cap do
        table.remove(list, 1)
    end
end

-- rune.perception.snapshot() -> { vitals, status, room, channels }
-- A fresh compact copy: safe for a caller to hold onto or mutate, never
-- aliases the model's internal tables.
function rune.perception.snapshot()
    local room_copy = shallow_copy(room)
    if room.exits then
        room_copy.exits = array_copy(room.exits)
    end
    return {
        vitals = shallow_copy(vitals),
        status = shallow_copy(status),
        room = room_copy,
        channels = array_copy(channels),
    }
end

-- rune.perception.transcript() -> array of recent cleaned output lines,
-- oldest first, capped at TRANSCRIPT_CAP. The rolling context T5's
-- agent core feeds the LLM alongside snapshot().
function rune.perception.transcript()
    return array_copy(transcript)
end

-- The durable, learned map: { rooms = {[tostring(num)]={name,area,
-- terrain}}, edges = {[tostring(num)]={[dir]=dest_num}} }. Keys are
-- strings (not room numbers) because rune.store's JSON bridge rejects
-- tables with sparse/non-sequential numeric keys - see api_store.go.
local function load_map()
    return rune.store.get("perception_map") or { rooms = {}, edges = {} }
end

function rune.perception.map()
    return load_map()
end

-- Persists the current room and, when the previous room is known and a
-- movement command immediately preceded this update, the edge that led
-- here. Only called from the Room.Info handler (room entry is rare
-- compared to combat's chatty Char.Vitals), so this never sits in a
-- hot path despite rune.store.set being a synchronous file write.
local function record_room(data)
    local map = load_map()
    map.rooms[tostring(data.num)] = {
        name = data.name,
        area = data.area,
        terrain = data.terrain,
    }
    if last_room_num and last_dir and last_room_num ~= data.num then
        local from = tostring(last_room_num)
        map.edges[from] = map.edges[from] or {}
        map.edges[from][last_dir] = data.num
    end
    rune.store.set("perception_map", map)

    last_room_num = data.num
    last_dir = nil
end

local function on_vitals(data)
    if data then vitals = data end
end

local function on_status(data)
    if data then status = data end
end

local function on_room(data)
    if not data then return end
    room = data
    record_room(data)
end

local function on_comm(data)
    if data then ring_append(channels, data, CHANNEL_CAP) end
end

local function on_output(line)
    ring_append(transcript, line:clean(), TRANSCRIPT_CAP)
end

local function on_input(text)
    last_dir = DIRECTIONS[text:lower():match("^%s*(.-)%s*$")]
end

local function on_disconnected()
    vitals = {}
    status = {}
    room = {}
    transcript = {}
    channels = {}
    last_room_num = nil
    last_dir = nil
end

local active = false
local handles = {}

-- rune.perception.enable() wires the GMCP subscriptions and hooks that
-- drive the model. Inert until called: this is a core file (loaded for
-- every Rune session, bot or not), and subscribing to GMCP packages or
-- writing to the durable store are observable side effects a plain
-- human user never asked for. The agent core (T5) calls this at
-- startup; nothing calls it automatically. Idempotent.
function rune.perception.enable()
    if active then return end
    active = true

    rune.gmcp.subscribe("Char", 1)
    rune.gmcp.subscribe("Room", 1)
    rune.gmcp.subscribe("Comm", 1)

    handles = {
        rune.gmcp.on("Char.Vitals", on_vitals, { name = "perception-vitals" }),
        rune.gmcp.on("Char.Status", on_status, { name = "perception-status" }),
        rune.gmcp.on("Room.Info", on_room, { name = "perception-room" }),
        rune.gmcp.on("Comm.Channel", on_comm, { name = "perception-comm" }),
        -- Priority 200: after triggers (75_send.lua), so a gagged line -
        -- hidden from the screen a human would see - stays out of the
        -- transcript too. Never gags or rewrites itself.
        rune.hooks.on("output", on_output, { name = "perception-transcript", priority = 200 }),
        -- Default priority (50) - must run before the core input
        -- handler (100), which always consumes, or this would never
        -- see anything.
        rune.hooks.on("input", on_input, { name = "perception-direction-tracker" }),
        rune.hooks.on("disconnected", on_disconnected, { name = "perception-reset", priority = 100 }),
    }
end

-- rune.perception.disable() unwinds enable(): unsubscribes and removes
-- every hook. Volatile state (vitals/status/room/transcript/channels)
-- is left as-is rather than cleared - a later enable() can resume with
-- stale-but-not-wrong data until fresh GMCP arrives. Idempotent.
function rune.perception.disable()
    if not active then return end
    active = false

    rune.gmcp.unsubscribe("Char")
    rune.gmcp.unsubscribe("Room")
    rune.gmcp.unsubscribe("Comm")
    for _, handle in ipairs(handles) do
        handle:remove()
    end
    handles = {}
end

function rune.perception.is_enabled()
    return active
end
