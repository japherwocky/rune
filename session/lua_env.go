package session

import "os"

// Env implements lua.Host. It is a dumb accessor - the allowlist
// restricting which names scripts may read lives at the Lua boundary
// (lua/api_env.go), not here.
func (s *Session) Env(name string) (string, bool) {
	return os.LookupEnv(name)
}
