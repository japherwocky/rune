-- JSON
-- Thin wrapper over rune._json (api_json.go). Both directions go
-- through the same Lua<->Go value bridge as rune.store and rune.gmcp:
-- tables with keys 1..n encode as JSON arrays, tables with string keys
-- encode as JSON objects, and an empty table always encodes as {}
-- (Lua has no way to distinguish an empty array from an empty object).
-- Symmetrically, decoding [] and decoding {} both yield the same empty
-- Lua table - code that needs to tell those apart cannot do so through
-- this bridge.

rune.json = {}

-- rune.json.encode(value) -> string, or nil + error message.
-- value may be nil, a boolean, number, string, or a JSON-able table.
-- Mixed-key tables, holes, functions, userdata, and reference cycles
-- are not encodable and return nil + error message.
function rune.json.encode(value)
    return rune._json.encode(value)
end

-- rune.json.decode(text) -> value, or nil + error message on
-- malformed JSON.
function rune.json.decode(text)
    return rune._json.decode(text)
end
