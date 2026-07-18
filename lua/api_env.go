package lua

import glua "github.com/yuin/gopher-lua"

// envAllowlist is the complete set of environment variable names
// scripts may read via rune.env. Everything else returns nil, exactly
// like an unset variable, so scripts get no signal to distinguish "not
// allowed" from "not set" - the allowlist can't be probed from Lua.
//
// RUNE_LLM_PROVIDER is not a secret (unlike the API keys below, which
// stay Go-only per LLMRequest's doc comment in host.go) - 86_llm.lua
// needs to read it to decide which wire-format JSON to build/parse
// (Anthropic-shaped vs. OpenAI-shaped), so it's safe and necessary to
// allowlist. RUNE_LLM_URL and RUNE_LLM_API_KEY never need to reach
// Lua - session/lua_llm.go reads those directly, same as
// OPENCODE_API_KEY.
//
// RUNE_LLM_MODEL is also not a secret - it's a fallback model id read
// by 96_agent_ui.lua's "/agent start" (no arg) so routine start/stop
// doesn't require a model name at the keyboard every time.
var envAllowlist = map[string]bool{
	"OPENCODE_API_KEY":  true,
	"RUNE_LLM_PROVIDER": true,
	"RUNE_LLM_MODEL":    true,
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
