package lua

// T7 tests (96_agent_ui.lua): the reasoning pane, the "agent" bar
// segment, per-turn rune.log entries, and the read-only /agent
// command. Per PLAN.md T7 these are meant to be light - bar/summary
// content for a given agent state, and "best-effort" confirmation
// that pane/log writes happen at the right lifecycle points, not an
// exhaustive check of every rendered string.

import (
	"strings"
	"testing"
)

func TestAgentUIBarStoppedWhenInactive(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("check", `
		local bar = rune.bars._render_all(80).agent
		assert(bar ~= nil, "expected an agent bar entry")
		assert(bar:find("stopped"), bar)
	`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentUIBarReflectsThinkingThenIdle(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	id := startAgentAndWake(t, engine, host)
	if err := engine.DoString("check-thinking", `
		local bar = rune.bars._render_all(80).agent
		assert(bar:find("thinking"), bar)
	`); err != nil {
		t.Fatal(err)
	}

	deliverEndTurn(engine, host, id, "all clear")
	if err := engine.DoString("check-idle", `
		local bar = rune.bars._render_all(80).agent
		assert(bar:find("idle"), bar)
		assert(bar:find("all clear"), "expected the goal in the bar: " .. bar)
	`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentUISummaryTracksTokensAndLastAction(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	id := startAgentAndWake(t, engine, host)
	id = deliverToolUse(t, engine, host, id, "toolu_1", "send_command", map[string]string{"cmd": "look"})

	if err := engine.DoString("check-mid-turn", `
		local s = rune.agent_ui.summary()
		assert(s.last_action == "tool: send_command", "last_action: " .. tostring(s.last_action))
		assert(s.input_tokens > 0 and s.output_tokens > 0, "expected non-zero usage after the first hop")
		assert(s.cost == nil, "cost should be nil with no pricing configured")
	`); err != nil {
		t.Fatal(err)
	}

	deliverEndTurn(engine, host, id, "done")
	if err := engine.DoString("check-end", `
		local s = rune.agent_ui.summary()
		assert(s.last_action == "idle", "last_action: " .. tostring(s.last_action))
	`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentUIPricingOptional(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("set-pricing", `
		rune.agent_ui.pricing = { input_per_million = 3, output_per_million = 15 }
	`); err != nil {
		t.Fatal(err)
	}

	id := startAgentAndWake(t, engine, host)
	deliverEndTurn(engine, host, id, "done")

	if err := engine.DoString("check-cost", `
		local s = rune.agent_ui.summary()
		-- deliverEndTurn's canned usage is {input_tokens:1, output_tokens:1} (see agent_tools_test.go helper)
		local want = (s.input_tokens / 1e6) * 3 + (s.output_tokens / 1e6) * 15
		assert(math.abs(s.cost - want) < 1e-9, "cost: " .. tostring(s.cost) .. " want " .. tostring(want))
	`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentUIPaneWritesDuringTurn(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	writesBefore := countPaneWrites(host, "agent")
	id := startAgentAndWake(t, engine, host)
	if countPaneWrites(host, "agent") <= writesBefore {
		t.Error("expected at least one pane write on turn start")
	}

	writesBeforeTool := countPaneWrites(host, "agent")
	id = deliverToolUse(t, engine, host, id, "toolu_1", "send_command", map[string]string{"cmd": "look"})
	if countPaneWrites(host, "agent") <= writesBeforeTool {
		t.Error("expected at least one pane write on tool_call")
	}

	writesBeforeEnd := countPaneWrites(host, "agent")
	deliverEndTurn(engine, host, id, "done")
	if countPaneWrites(host, "agent") <= writesBeforeEnd {
		t.Error("expected at least one pane write on turn end")
	}
}

func countPaneWrites(host *MockHost, pane string) int {
	n := 0
	for _, call := range host.PaneCalls {
		if call.Op == "write" && call.Name == pane {
			n++
		}
	}
	return n
}

func TestAgentUILogSilentWhenNoLogActive(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	id := startAgentAndWake(t, engine, host)
	deliverEndTurn(engine, host, id, "done")

	if len(host.LogWrites) != 0 {
		t.Fatalf("rune.log.write should no-op with no log open, got %v", host.LogWrites)
	}
}

func TestAgentUILogWritesPerTurnWhenLogActive(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("start-log", `
		local path, err = rune.log.start("test.log")
		assert(path, err)
	`); err != nil {
		t.Fatal(err)
	}

	id := startAgentAndWake(t, engine, host)
	id = deliverToolUse(t, engine, host, id, "toolu_1", "send_command", map[string]string{"cmd": "look"})
	deliverEndTurn(engine, host, id, "done")

	joined := strings.Join(host.LogWrites, "\n")
	for _, want := range []string{"[Agent] turn start", "[Agent] [tool] send_command", "[Agent] turn end"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected log to contain %q, got:\n%s", want, joined)
		}
	}
}

func TestAgentCommandShowsStatus(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	engine.OnInput("/agent")
	joined := strings.Join(host.PrintCalls, "\n")
	if !strings.Contains(joined, "stopped") {
		t.Fatalf("expected /agent to report stopped before start(), got:\n%s", joined)
	}

	host.PrintCalls = nil
	id := startAgentAndWake(t, engine, host)
	deliverEndTurn(engine, host, id, "resting up")

	engine.OnInput("/agent")
	joined = strings.Join(host.PrintCalls, "\n")
	if !strings.Contains(joined, "idle") || !strings.Contains(joined, "resting up") {
		t.Fatalf("expected /agent to report idle + goal after a turn, got:\n%s", joined)
	}
}
