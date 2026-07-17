package lua

import glua "github.com/yuin/gopher-lua"

// envAllowlist is the complete set of environment variable names
// scripts may read via rune.env. Everything else returns nil, exactly
// like an unset variable, so scripts get no signal to distinguish "not
// allowed" from "not set" - the allowlist can't be probed from Lua.
var envAllowlist = map[string]bool{
	"OPENCODE_API_KEY": true,
}

// registerEnvFuncs registers rune._env.* primitives.
// The public rune.env API is defined in Lua (83_env.lua). Reads go
// through Host so tests can control them without touching real
// process environment (see MockHost.EnvVars).
func (e *Engine) registerEnvFuncs() {
	envTable := e.L.NewTable()
	e.L.SetField(e.runeTable, "_env", envTable)

	// rune._env.get(name) -> string, or nil when unset or not
	// allowlisted.
	e.L.SetField(envTable, "get", e.L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)

		if !envAllowlist[name] {
			L.Push(glua.LNil)
			return 1
		}
		value, ok := e.host.Env(name)
		if !ok {
			L.Push(glua.LNil)
			return 1
		}
		L.Push(glua.LString(value))
		return 1
	}))
}
