-- Env
-- Thin wrapper over rune._env (api_env.go). Only an allowlisted set of
-- environment variable names is readable from scripts - currently just
-- OPENCODE_API_KEY, for the LLM client (see PLAN.md T2). Anything else
-- returns nil, the same as an unset variable.

-- rune.env(name) -> string, or nil.
function rune.env(name)
    return rune._env.get(name)
end
