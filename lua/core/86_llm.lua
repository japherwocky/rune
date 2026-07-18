-- LLM
-- Go-backed transport (rune._llm, see PLAN.md T8): Go owns the
-- destination, auth header, and retry/backoff policy for OpenCode Zen
-- (session/lua_llm.go) - the API key never passes through Lua. This
-- module only builds/parses the Anthropic-Messages-API-shaped JSON
-- body and owns the id -> callback map, the same split of
-- responsibilities as rune.http/80_http.lua: pending callbacks die
-- with the VM on /reload, and a late result for a stale id is
-- dropped. rune.llm.chat's public signature and reply shape are
-- unchanged from the Phase 1 (T4) rune.http.post transport this
-- replaced.
--
-- This module only builds/parses the request - it does not choose a
-- model on the caller's behalf. Zen's catalog rotates (including which
-- models are free/promotional), so req.model is required rather than
-- defaulted here; see T4 for the live /v1/models listing.

rune.llm = {}

local pending = {}
local next_id = 0

-- Pulls the human-readable message out of a Zen/Anthropic-shaped error
-- body ({"error":{"message":...}}, optionally wrapped in an outer
-- {"type":"error",...}); falls back to the raw body when it isn't
-- JSON-shaped that way, so callers always get their eyeball on
-- something useful.
local function error_message(status, body)
    local data = rune.json.decode(body)
    if type(data) == "table" and type(data.error) == "table" and data.error.message then
        return "rune.llm: HTTP " .. status .. ": " .. data.error.message
    end
    return "rune.llm: HTTP " .. status .. ": " .. body
end

-- Splits an Anthropic-shaped content array into a convenience { text,
-- tool_uses } pair. The raw array is preserved on the reply too
-- (reply.content) because a follow-up turn with tool_result blocks
-- must echo the assistant's content verbatim - callers that only
-- care about text or tool calls can use the convenience fields
-- instead of re-walking content themselves.
local function split_content(content)
    local text_parts = {}
    local tool_uses = {}
    for _, block in ipairs(content or {}) do
        if block.type == "text" then
            table.insert(text_parts, block.text)
        elseif block.type == "tool_use" then
            table.insert(tool_uses, { id = block.id, name = block.name, input = block.input })
        end
    end
    return table.concat(text_parts), tool_uses
end

-- rune.llm.chat(req, callback)
-- req: { model, messages, system?, tools?, max_tokens }. model,
-- messages, and max_tokens are required (the upstream API rejects a
-- request missing any of them); system and tools pass through as-is
-- when present, omitted when not.
-- callback(reply, err) - exactly one is set:
--   reply = { content, text, tool_uses, stop_reason, usage =
--     { input_tokens, output_tokens } }
--   err = a string: bad env (no API key), HTTP transport failure,
--     non-200 response, or a malformed response body.
function rune.llm.chat(req, callback)
    if type(req) ~= "table" then
        error("rune.llm.chat: req must be a table", 2)
    end
    if type(req.model) ~= "string" or req.model == "" then
        error("rune.llm.chat: req.model must be a non-empty string", 2)
    end
    if type(req.messages) ~= "table" then
        error("rune.llm.chat: req.messages must be a table", 2)
    end
    if type(req.max_tokens) ~= "number" then
        error("rune.llm.chat: req.max_tokens must be a number", 2)
    end
    if type(callback) ~= "function" then
        error("rune.llm.chat: callback must be a function", 2)
    end

    local body, encode_err = rune.json.encode({
        model = req.model,
        system = req.system,
        messages = req.messages,
        tools = req.tools,
        max_tokens = req.max_tokens,
    })
    if not body then
        callback(nil, "rune.llm: failed to encode request: " .. tostring(encode_err))
        return
    end

    next_id = next_id + 1
    pending[next_id] = callback
    rune._llm.request(next_id, { body = body })
end

-- INTERNAL: called by Go when a request completes. Exactly one of
-- response/err is set. Unknown ids - callback-less requests (there
-- are none today), or requests started before a /reload - are
-- dropped. A throwing callback propagates to the engine's error
-- report.
function rune.llm._deliver(id, resp, err)
    local cb = pending[id]
    if not cb then
        return
    end
    pending[id] = nil

    if err then
        cb(nil, "rune.llm: " .. err)
        return
    end
    if resp.status ~= 200 then
        cb(nil, error_message(resp.status, resp.body))
        return
    end

    local data, decode_err = rune.json.decode(resp.body)
    if not data then
        cb(nil, "rune.llm: malformed response JSON: " .. tostring(decode_err))
        return
    end

    local text, tool_uses = split_content(data.content)
    cb({
        content = data.content,
        text = text,
        tool_uses = tool_uses,
        stop_reason = data.stop_reason,
        usage = data.usage,
    }, nil)
end
