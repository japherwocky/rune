-- LLM
-- Go-backed transport (rune._llm, see PLAN.md T8): Go owns the
-- destination, auth header, and retry/backoff policy - the API key
-- never passes through Lua. This module builds/parses the request
-- body and owns the id -> callback map, the same split of
-- responsibilities as rune.http/80_http.lua: pending callbacks die
-- with the VM on /reload, and a late result for a stale id is
-- dropped. rune.llm.chat's public signature and reply shape are
-- unchanged since Phase 1 (T4).
--
-- Two wire formats, one public shape (PLAN.md T8b): Go picks the
-- backend from RUNE_LLM_PROVIDER (session/lua_llm.go) - "zen"
-- (default), OpenCode Zen's Anthropic-Messages-API-shaped endpoint, or
-- "openai", the OpenAI chat/completions shape spoken by llama.cpp's
-- llama-server and other local/self-hosted runners. This module reads
-- the *same* env var (allowlisted in api_env.go, unlike the API keys)
-- to decide which JSON shape to build/parse - the two sides agree
-- independently rather than one telling the other. Everything above
-- this file (87_agent.lua, 88_agent_tools.lua, 91_agent_policy.lua)
-- only ever sees the one canonical reply shape below
-- (content/text/tool_uses/stop_reason/usage, Anthropic-flavored) -
-- picking "openai" translates on the way out and normalizes on the
-- way back in, entirely inside this file.
--
-- This module only builds/parses the request - it does not choose a
-- model on the caller's behalf. Zen's catalog rotates (including which
-- models are free/promotional), so req.model is required rather than
-- defaulted here; see T4 for the live /v1/models listing. Against a
-- local runner, req.model is typically ignored server-side (it serves
-- whatever's loaded) - any non-empty placeholder satisfies the
-- validation below.

rune.llm = {}

local pending = {}
local next_id = 0

-- Pulls the human-readable message out of an error body shaped
-- {"error":{"message":...}}, optionally wrapped in an outer
-- {"type":"error",...} - both Zen/Anthropic and OpenAI-compatible
-- servers use this same nested shape for errors - falling back to the
-- raw body when it isn't JSON-shaped that way, so callers always get
-- their eyeball on something useful.
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
-- instead of re-walking content themselves. Used for both providers:
-- the openai path synthesizes this same block shape in
-- from_openai_response before ever reaching here.
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

-- ---- OpenAI chat/completions wire format (llama.cpp and friends) ----
--
-- The canonical shape used everywhere above this file is Anthropic's:
-- a top-level `system` string, `messages` where content is either a
-- plain string or an array of {type="text"|"tool_use"|"tool_result",
-- ...} blocks, and tools as {name, description, input_schema}. OpenAI
-- has no top-level system field, no multi-block message content, and
-- a different tool/tool_call shape - translated here, both directions,
-- so nothing above this file has to know a second wire format exists.

-- system becomes a leading {role="system"} message. A canonical
-- assistant message's content array becomes OpenAI's separate
-- content-string/tool_calls fields; a canonical user message carrying
-- tool_result blocks expands into one OpenAI role="tool" message per
-- block, since OpenAI has no multi-block content the way Anthropic
-- does - one incoming message can become several outgoing ones.
local function to_openai_messages(system, messages)
    local out = {}
    if system and system ~= "" then
        table.insert(out, { role = "system", content = system })
    end
    for _, m in ipairs(messages) do
        if type(m.content) == "string" then
            table.insert(out, { role = m.role, content = m.content })
        elseif m.role == "assistant" then
            local text, tool_uses = split_content(m.content)
            local tool_calls = nil
            if #tool_uses > 0 then
                tool_calls = {}
                for _, tu in ipairs(tool_uses) do
                    table.insert(tool_calls, {
                        id = tu.id,
                        type = "function",
                        ["function"] = { name = tu.name, arguments = rune.json.encode(tu.input) or "{}" },
                    })
                end
            end
            table.insert(out, { role = "assistant", content = text ~= "" and text or nil, tool_calls = tool_calls })
        else
            for _, block in ipairs(m.content or {}) do
                if block.type == "tool_result" then
                    -- OpenAI's role="tool" message has no is_error
                    -- field the way Anthropic's tool_result block does
                    -- - without folding it into the text, a model
                    -- talking over this wire would have no way to
                    -- tell a failed tool call from a successful one.
                    local text = tostring(block.content)
                    if block.is_error then
                        text = "Error: " .. text
                    end
                    table.insert(out, {
                        role = "tool",
                        tool_call_id = block.tool_use_id,
                        content = text,
                    })
                end
            end
        end
    end
    return out
end

-- {name, description, input_schema} -> {type="function", function=
-- {name, description, parameters}}. input_schema/parameters are both
-- raw JSON Schema tables, passed through unchanged - only the
-- envelope differs.
local function to_openai_tools(tools)
    if not tools then
        return nil
    end
    local out = {}
    for _, t in ipairs(tools) do
        table.insert(out, {
            type = "function",
            ["function"] = { name = t.name, description = t.description, parameters = t.input_schema },
        })
    end
    return out
end

-- finish_reason -> the one canonical stop_reason 87_agent.lua actually
-- branches on ("tool_use" continues the turn). "length" maps to
-- Anthropic's equivalent term since it's a direct, unambiguous
-- correspondence; anything else passes through as-is (e.g. plain
-- "stop") rather than being forced into an Anthropic term it isn't -
-- it still reaches T7's turn-end log untouched, which is more honest
-- than fabricating "end_turn" for a backend that never said that.
local function openai_stop_reason(finish_reason)
    if finish_reason == "tool_calls" then
        return "tool_use"
    end
    if finish_reason == "length" then
        return "max_tokens"
    end
    return finish_reason or "end_turn"
end

-- OpenAI's choices[1].message -> a synthesized Anthropic-shaped
-- content array, so split_content and the tool_result round-trip
-- (87_agent.lua echoes reply.content back verbatim on the next hop,
-- which to_openai_messages above then re-translates) behave
-- identically regardless of which wire the provider spoke.
-- function.arguments arrives as a JSON *string* in OpenAI's shape
-- (unlike Anthropic's already-decoded tool_use.input) - decoded here
-- so tool_uses[].input is always a table either way.
local function from_openai_response(data)
    local choice = data.choices and data.choices[1]
    local message = (choice and choice.message) or {}
    local content = {}
    if message.content and message.content ~= "" then
        table.insert(content, { type = "text", text = message.content })
    end
    for _, tc in ipairs(message.tool_calls or {}) do
        local fn = tc["function"] or {}
        local input = (fn.arguments and rune.json.decode(fn.arguments)) or {}
        table.insert(content, { type = "tool_use", id = tc.id, name = fn.name, input = input })
    end
    local usage = data.usage and {
        input_tokens = data.usage.prompt_tokens,
        output_tokens = data.usage.completion_tokens,
    } or nil
    return content, openai_stop_reason(choice and choice.finish_reason), usage
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

    local provider = rune.env("RUNE_LLM_PROVIDER") or "zen"

    local body_table
    if provider == "openai" then
        body_table = {
            model = req.model,
            messages = to_openai_messages(req.system, req.messages),
            tools = to_openai_tools(req.tools),
            max_tokens = req.max_tokens,
        }
    else
        body_table = {
            model = req.model,
            system = req.system,
            messages = req.messages,
            tools = req.tools,
            max_tokens = req.max_tokens,
        }
    end

    local body, encode_err = rune.json.encode(body_table)
    if not body then
        callback(nil, "rune.llm: failed to encode request: " .. tostring(encode_err))
        return
    end

    next_id = next_id + 1
    pending[next_id] = { callback = callback, provider = provider }
    rune._llm.request(next_id, { body = body })
end

-- INTERNAL: called by Go when a request completes. Exactly one of
-- response/err is set. Unknown ids - callback-less requests (there
-- are none today), or requests started before a /reload - are
-- dropped. A throwing callback propagates to the engine's error
-- report.
function rune.llm._deliver(id, resp, err)
    local entry = pending[id]
    if not entry then
        return
    end
    pending[id] = nil
    local cb = entry.callback

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

    local content, stop_reason, usage
    if entry.provider == "openai" then
        content, stop_reason, usage = from_openai_response(data)
    else
        content, stop_reason, usage = data.content, data.stop_reason, data.usage
    end

    local text, tool_uses = split_content(content)
    cb({
        content = content,
        text = text,
        tool_uses = tool_uses,
        stop_reason = stop_reason,
        usage = usage,
    }, nil)
end
