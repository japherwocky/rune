package lua

// T12 tests (92_agent_log.lua): the fork's logging layer on top of
// upstream's 60_log.lua - timestamps, agent chrome capture, read-back,
// and the headless auto-start. Lua-against-MockHost throughout; the
// real file-backed read/search is covered at the Session layer
// (session/lua_log_test.go), so these pin the Lua policy rather than
// re-testing the Go scan.

import (
	"sort"
	"strings"
	"testing"

	"github.com/mmcdole/rune/text"
)

// setupHeadlessTest boots a VM the way Session.boot does for a
// --headless run: SetHeadless before core scripts load, so
// 92_agent_log.lua sees rune.headless at load time. setupTest leaves
// it unset (falsy), which is the ordinary terminal case.
func setupHeadlessTest(t *testing.T) (*Engine, *MockHost, func()) {
	t.Helper()

	host := NewMockHost()
	engine := NewEngine(host)
	if err := engine.Init(); err != nil {
		t.Fatal("Failed to initialize engine:", err)
	}
	engine.SetConfigDir(t.TempDir())
	engine.SetHeadless(true)

	entries, err := CoreScripts.ReadDir("core")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	for _, file := range files {
		content, err := CoreScripts.ReadFile("core/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.DoString(file, string(content)); err != nil {
			t.Fatalf("Failed to execute %s: %v", file, err)
		}
	}
	return engine, host, func() { engine.Close() }
}

func startLog(t *testing.T, engine *Engine) {
	t.Helper()
	if err := engine.DoString("start-log", `assert(rune.log.start("/tmp/test.log"))`); err != nil {
		t.Fatal(err)
	}
}

// --- timestamps ---

// The wrapper goes on rune._log.write itself, so it must catch lines
// written by upstream's own hooks too, not just rune.log.write callers.
func TestTimestampsPrefixEveryLoggedLine(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startLog(t, engine)

	// Everything already written (rune.log.start's own "--- Log started
	// ---" stamp) stays as it was: enabling applies from here on, never
	// retroactively.
	before := len(host.LogWrites)
	if err := engine.DoString("stamp-on", `rune.log.timestamps(true)`); err != nil {
		t.Fatal(err)
	}
	if err := engine.DoString("write", `rune.log.write("a direct write")`); err != nil {
		t.Fatal(err)
	}
	engine.OnOutput(text.NewLine("a server line"))

	writes := host.LogWrites[before:]
	if len(writes) < 2 {
		t.Fatalf("expected both lines logged, got %q", writes)
	}
	for _, line := range writes {
		// "[HH:MM:SS] " - checked structurally rather than against a
		// literal clock reading.
		if !strings.HasPrefix(line, "[") || len(line) < 11 || line[9:11] != "] " {
			t.Errorf("line not timestamped: %q", line)
		}
	}
}

// Off by default is what keeps a plain human /log byte-identical to
// upstream's output.
func TestTimestampsOffByDefault(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startLog(t, engine)

	if err := engine.DoString("check-default", `assert(rune.log.timestamps() == false)`); err != nil {
		t.Fatal(err)
	}
	if err := engine.DoString("write", `rune.log.write("plain")`); err != nil {
		t.Fatal(err)
	}

	writes := host.LogWrites
	if len(writes) == 0 || writes[len(writes)-1] != "plain" {
		t.Fatalf("expected an unmodified line, got %q", writes)
	}
}

func TestTimestampsCanBeTurnedBackOff(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startLog(t, engine)

	if err := engine.DoString("toggle", `
		rune.log.timestamps(true)
		rune.log.timestamps(false)
		rune.log.write("plain again")
	`); err != nil {
		t.Fatal(err)
	}

	writes := host.LogWrites
	if writes[len(writes)-1] != "plain again" {
		t.Fatalf("expected timestamps off, got %q", writes[len(writes)-1])
	}
}

// --- agent chrome ---

// A reflex send produces no agent_tool_call (no LLM turn is involved),
// so without this hook it would reach the screen and nothing else.
func TestAgentSendIsLogged(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startLog(t, engine)

	if err := engine.DoString("send", `assert(rune.agent_policy.send("kill kobold"))`); err != nil {
		t.Fatal(err)
	}

	if !containsLine(host.LogWrites, "[agent] kill kobold") {
		t.Errorf("agent command missing from log: %q", host.LogWrites)
	}
}

func TestAgentPolicyNoticeIsLogged(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startLog(t, engine)

	// "quit" is denylisted by default (91_agent_policy.lua).
	if err := engine.DoString("denied", `
		local ok = rune.agent_policy.send("quit")
		assert(ok == nil, "quit should have been denied")
	`); err != nil {
		t.Fatal(err)
	}

	if !containsLine(host.LogWrites, "denied_command") {
		t.Errorf("policy notice missing from log: %q", host.LogWrites)
	}
	// A denied command must not also be logged as if it had been sent.
	if containsLine(host.LogWrites, "[agent] quit") {
		t.Error("a denied command must not be logged as sent")
	}
}

// Logging is opt-in: with no log open these hooks must be inert, not
// erroring or accumulating.
func TestAgentChromeSilentWithoutOpenLog(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("send", `assert(rune.agent_policy.send("north"))`); err != nil {
		t.Fatal(err)
	}
	if len(host.LogWrites) != 0 {
		t.Errorf("expected no log writes with no log open, got %q", host.LogWrites)
	}
}

// --- read-back ---

func TestLogReadAndSearchFromLua(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()
	startLog(t, engine)

	if err := engine.DoString("read-search", `
		rune.log.write("kobold appears")
		rune.log.write("you hit the kobold")
		rune.log.write("a rat scurries by")

		local lines = assert(rune.log.read(2))
		assert(#lines == 2, "expected 2 lines, got " .. #lines)
		assert(lines[2] == "a rat scurries by", "expected newest line last, got " .. lines[2])

		local hits = assert(rune.log.search("kobold"))
		assert(#hits == 2, "expected 2 matches, got " .. #hits)
	`); err != nil {
		t.Fatal(err)
	}
}

func TestLogReadWithoutOpenLogReportsError(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	// nil + message, not a raise - the recoverable-failure convention.
	if err := engine.DoString("no-log", `
		local lines, err = rune.log.read(10)
		assert(lines == nil, "expected no lines")
		assert(type(err) == "string" and err ~= "", "expected an error message")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestLogReadClampsToMaximum(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()
	startLog(t, engine)

	// Over the 500 cap must clamp rather than raise; a nonsense count
	// is a caller error and does raise.
	if err := engine.DoString("clamp", `
		assert(rune.log.read(10000))
		local ok = pcall(rune.log.read, -1)
		assert(not ok, "a negative line count should raise")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestLogSearchRejectsEmptyPattern(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()
	startLog(t, engine)

	if err := engine.DoString("empty-pattern", `
		local ok = pcall(rune.log.search, "")
		assert(not ok, "an empty pattern should raise")
	`); err != nil {
		t.Fatal(err)
	}
}

// --- headless auto-start ---

func TestHeadlessStartsLogAutomatically(t *testing.T) {
	engine, host, cleanup := setupHeadlessTest(t)
	defer cleanup()

	path, active := host.LogStatus()
	if !active {
		t.Fatal("headless boot should have started a log")
	}
	if !strings.Contains(path, "logs") {
		t.Errorf("expected the default logs/ path, got %q", path)
	}
	if err := engine.DoString("check-stamping", `assert(rune.log.timestamps() == true)`); err != nil {
		t.Error("headless should log with timestamps:", err)
	}
}

func TestTerminalSessionStartsNoLog(t *testing.T) {
	_, host, cleanup := setupTest(t)
	defer cleanup()

	if _, active := host.LogStatus(); active {
		t.Error("a terminal session must not start logging on its own")
	}
}

func containsLine(lines []string, substr string) bool {
	for _, line := range lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}
