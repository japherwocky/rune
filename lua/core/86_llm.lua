-- LLM
-- Phase 1 transport: rune.http.post against OpenCode Zen's
-- Anthropic-Messages-API-shaped endpoint (see PLAN.md T4 and T2 for the
-- provider choice). T8 swaps the body of rune.llm.chat onto a Go
-- rune._llm primitive for streaming; this module's public signature is
-- designed to survive that swap unchanged.
--
-- Auth: verified live against https://opencode.ai/zen/v1/messages
-- (2026-07-17, see PLAN.md T4) - a bad "x-api-key" gets a distinct 401
-- AuthError, while a bad "Authorization: Bearer" is silently ignored
-- and the request falls through to the upstream provider. So:
-- x-api-key, not Authorization: Bearer, contrary to Zen's docs
-- elsewhere.
--
-- This module only builds/parses the request - it does not choose a
-- model on the caller's behalf. Zen's catalog rotates (including which
-- models are free/promotional), so req.model is required rather than
-- defaulted here; see T4 for the live /v1/models listing.

rune.llm = {}

local ZEN_URL = "https://opencode.ai/zen/v1/messages"
local ANTHROPIC_VERSION = "2023-06-01"

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

    local key = rune.env("OPENCODE_API_KEY")
    if not key then
        callback(nil, "rune.llm: OPENCODE_API_KEY is not set")
        return
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

    rune.http.post(ZEN_URL, body, {
        headers = {
            ["Content-Type"] = "application/json",
            ["x-api-key"] = key,
            ["anthropic-version"] = ANTHROPIC_VERSION,
        },
    }, function(resp, err)
        if err then
            callback(nil, "rune.llm: " .. err)
            return
        end
        if resp.status ~= 200 then
            callback(nil, error_message(resp.status, resp.body))
            return
        end

        local data, decode_err = rune.json.decode(resp.body)
        if not data then
            callback(nil, "rune.llm: malformed response JSON: " .. tostring(decode_err))
            return
        end

        local text, tool_uses = split_content(data.content)
        callback({
            content = data.content,
            text = text,
            tool_uses = tool_uses,
            stop_reason = data.stop_reason,
            usage = data.usage,
        }, nil)
    end)
end
