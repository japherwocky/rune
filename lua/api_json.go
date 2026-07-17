package lua

import (
	"encoding/json"

	glua "github.com/yuin/gopher-lua"
)

// registerJSONFuncs registers rune._json.* primitives.
// The public rune.json API is defined in Lua (82_json.lua). Both
// directions reuse the shared Lua<->Go value bridge (luaToGo/goToLua
// in api_store.go) that rune.store and rune.gmcp already build on, so
// all three agree on what's encodable and on the empty-table
// convention.
func (e *Engine) registerJSONFuncs() {
	jsonTable := e.L.NewTable()
	e.L.SetField(e.runeTable, "_json", jsonTable)

	// rune._json.encode(value) -> string, or nil + error message.
	e.L.SetField(jsonTable, "encode", e.L.NewFunction(func(L *glua.LState) int {
		value := L.Get(1)

		gv, err := luaToGo(value, make(map[*glua.LTable]bool), 0)
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		raw, err := json.Marshal(gv)
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(glua.LString(raw))
		return 1
	}))

	// rune._json.decode(text) -> value, or nil + error message. A
	// malformed text argument (wrong type) raises, per convention -
	// that's a programmer error, not a decode failure.
	e.L.SetField(jsonTable, "decode", e.L.NewFunction(func(L *glua.LState) int {
		text := L.CheckString(1)

		var decoded any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(goToLua(L, decoded))
		return 1
	}))
}
