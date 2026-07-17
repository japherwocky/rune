package lua

import "testing"

// TestEnvAllowlistedReturnsValue verifies rune.env returns the value
// of an allowlisted, set variable.
func TestEnvAllowlistedReturnsValue(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	host.EnvVars = map[string]string{"OPENCODE_API_KEY": "sk-test-123"}

	if err := engine.DoString("test", `assert(rune.env("OPENCODE_API_KEY") == "sk-test-123")`); err != nil {
		t.Fatal(err)
	}
}

// TestEnvAllowlistedButUnset verifies an allowlisted name that isn't
// actually set in the environment returns nil, not an error.
func TestEnvAllowlistedButUnset(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("test", `assert(rune.env("OPENCODE_API_KEY") == nil)`); err != nil {
		t.Fatal(err)
	}
}

// TestEnvNotAllowlistedReturnsNil verifies scripts cannot read
// arbitrary environment variables, even ones the host does have set -
// the allowlist is enforced regardless of what Host.Env would return.
func TestEnvNotAllowlistedReturnsNil(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	host.EnvVars = map[string]string{
		"PATH":              "/usr/bin",
		"HOME":              "/home/user",
		"SECRET":            "leaked-if-this-fails",
		"OPENCODE_API_KEYS": "not-the-real-name",
	}

	script := `
		assert(rune.env("PATH") == nil)
		assert(rune.env("HOME") == nil)
		assert(rune.env("SECRET") == nil)
		assert(rune.env("OPENCODE_API_KEYS") == nil)
	`
	if err := engine.DoString("test", script); err != nil {
		t.Fatal(err)
	}
}

// TestEnvBadArgumentRaises verifies a non-coercible argument raises -
// a programmer error, not a missing-variable case.
func TestEnvBadArgumentRaises(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	for _, code := range []string{
		`rune.env(nil)`,
		`rune.env({})`,
		`rune.env(true)`,
	} {
		if err := engine.DoString("test", code); err == nil {
			t.Errorf("expected error for %q", code)
		}
	}
}
