package lua

import "testing"

// TestJSONEncodeDecodeRoundTrip verifies rune.json round-trips scalars,
// arrays, objects, and nested structures, and that an empty table
// always encodes as {} (never []) - the documented convention shared
// with rune.store and rune.gmcp.
func TestJSONEncodeDecodeRoundTrip(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	script := `
		-- string
		assert(rune.json.decode(rune.json.encode("hello")) == "hello")

		-- number
		assert(rune.json.decode(rune.json.encode(42)) == 42)
		assert(rune.json.decode(rune.json.encode(3.5)) == 3.5)

		-- bool
		assert(rune.json.decode(rune.json.encode(true)) == true)
		assert(rune.json.decode(rune.json.encode(false)) == false)

		-- null / nil
		assert(rune.json.encode(nil) == "null")
		assert(rune.json.decode("null") == nil)

		-- array
		local arr = rune.json.decode(rune.json.encode({"a", "b", "c"}))
		assert(#arr == 3 and arr[1] == "a" and arr[3] == "c")

		-- object
		local obj = rune.json.decode(rune.json.encode({name = "arctic", hp = 100}))
		assert(obj.name == "arctic" and obj.hp == 100)

		-- nested
		local nested = rune.json.decode(rune.json.encode({
			room = { num = 3001, exits = {"north", "east"} },
		}))
		assert(nested.room.num == 3001)
		assert(#nested.room.exits == 2 and nested.room.exits[2] == "east")

		-- empty table: always round-trips through {} (object), never []
		assert(rune.json.encode({}) == "{}")
		assert(type(rune.json.decode("{}")) == "table")
		assert(type(rune.json.decode("[]")) == "table")
	`
	if err := engine.DoString("json_roundtrip", script); err != nil {
		t.Fatalf("json round-trip failed: %v", err)
	}
}

// TestJSONDecodeMalformed verifies malformed JSON text is a recoverable
// failure (nil, err), not a raise.
func TestJSONDecodeMalformed(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	script := `
		local v, err = rune.json.decode("{not valid json")
		assert(v == nil, "value should be nil on malformed JSON")
		assert(err ~= nil, "error should be set on malformed JSON")
	`
	if err := engine.DoString("json_malformed", script); err != nil {
		t.Fatalf("malformed JSON should return nil+err, not raise: %v", err)
	}
}

// TestJSONEncodeUnencodable verifies values luaToGo rejects (functions,
// cycles, mixed array/string keys) come back as nil, err from
// rune.json.encode too - it shares the same bridge as rune.store.set.
func TestJSONEncodeUnencodable(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	script := `
		local ok, err = rune.json.encode({ fn = function() end })
		assert(ok == nil and err ~= nil, "function value should be rejected")

		local cyc = {}
		cyc.self = cyc
		local ok2, err2 = rune.json.encode(cyc)
		assert(ok2 == nil and err2 ~= nil, "cycle should be rejected")

		local mixed = {"a", "b"}
		mixed.name = "oops"
		local ok3, err3 = rune.json.encode(mixed)
		assert(ok3 == nil and err3 ~= nil, "mixed array/string keys should be rejected")
	`
	if err := engine.DoString("json_unencodable", script); err != nil {
		t.Fatalf("unencodable value handling failed: %v", err)
	}
}

// TestJSONDecodeBadArgumentRaises verifies a non-coercible argument to
// decode raises - a programmer error, not a decode failure. Numbers are
// exempt: gopher-lua's CheckString coerces LNumber to a string like the
// rest of the VM (string.format, .. concatenation, ...), so
// rune.json.decode(123) is well-defined - it decodes the JSON text
// "123" - and is covered separately below rather than treated as bad
// input.
func TestJSONDecodeBadArgumentRaises(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	for _, code := range []string{
		`rune.json.decode(nil)`,
		`rune.json.decode({})`,
		`rune.json.decode(true)`,
		`rune.json.decode(function() end)`,
	} {
		if err := engine.DoString("test", code); err == nil {
			t.Errorf("expected error for %q", code)
		}
	}
}

// TestJSONDecodeNumberCoercion documents that a number argument is
// coerced to a string first, per gopher-lua's CheckString.
func TestJSONDecodeNumberCoercion(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("test", `assert(rune.json.decode(123) == 123)`); err != nil {
		t.Fatalf("number coercion failed: %v", err)
	}
}
