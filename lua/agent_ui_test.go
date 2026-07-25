package lua

// T7 tests (96_agent_ui.lua): the reasoning pane, the "agent_status" bar
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
		local bar = rune.bars._render_all(80).agent_status
		assert(bar ~= nil, "expected an agent_status bar entry")
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
		local bar = rune.bars._render_all(80).agent_status
		assert(bar:find("thinking"), bar)
	`); err != nil {
		t.Fatal(err)
	}

	deliverEndTurn(engine, host, id, "all clear")
	if err := engine.DoString("check-idle", `
		local bar = rune.bars._render_all(80).agent_status
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

func TestAgentUIEchoesDuringTurn(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	writesBefore := len(host.PrintCalls)
	id := startAgentAndWake(t, engine, host)
	if len(host.PrintCalls) <= writesBefore {
		t.Error("expected at least one echo on turn start")
	}

	writesBeforeTool := len(host.PrintCalls)
	id = deliverToolUse(t, engine, host, id, "toolu_1", "send_command", map[string]string{"cmd": "look"})
	if len(host.PrintCalls) <= writesBeforeTool {
		t.Error("expected at least one echo on tool_call")
	}

	writesBeforeEnd := len(host.PrintCalls)
	deliverEndTurn(engine, host, id, "done")
	if len(host.PrintCalls) <= writesBeforeEnd {
		t.Error("expected at least one echo on turn end")
	}
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

// T7-addendum tests (PLAN.md): "/agent start|stop" control, added after
// the fact by explicit user request - see 96_agent_ui.lua's updated
// header comment for why this reverses T7's original status-only call.

func TestAgentCommandStartWithExplicitModel(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	engine.OnInput("/agent start big-pickle")

	if err := engine.DoString("check", `
		local s = rune.agent.status()
		assert(s.active, "expected agent active after /agent start")
		assert(s.model == "big-pickle", "model: " .. tostring(s.model))
	`); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(host.PrintCalls, "\n")
	if !strings.Contains(joined, "started") || !strings.Contains(joined, "big-pickle") {
		t.Fatalf("expected start confirmation with model name, got:\n%s", joined)
	}
	if !strings.Contains(joined, "provider: zen") {
		t.Fatalf("expected the default provider (zen) alongside the model, got:\n%s", joined)
	}
}

func TestAgentCommandShowsProviderOverrideInStatus(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	host.EnvVars = map[string]string{
		"OPENCODE_API_KEY":  "test-key-123",
		"RUNE_LLM_PROVIDER": "openai",
	}

	engine.OnInput("/agent start big-pickle")
	host.PrintCalls = nil
	engine.OnInput("/agent")

	joined := strings.Join(host.PrintCalls, "\n")
	if !strings.Contains(joined, "provider: openai") {
		t.Fatalf("expected /agent status to reflect RUNE_LLM_PROVIDER=openai, got:\n%s", joined)
	}
}

func TestAgentCommandStartUsesEnvModelDefault(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	host.EnvVars = map[string]string{
		"OPENCODE_API_KEY": "test-key-123",
		"RUNE_LLM_MODEL":   "claude-haiku-4-5",
	}

	engine.OnInput("/agent start")

	if err := engine.DoString("check", `
		local s = rune.agent.status()
		assert(s.active, "expected agent active after /agent start with env default")
		assert(s.model == "claude-haiku-4-5", "model: " .. tostring(s.model))
	`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentCommandStartWithoutModelOrEnvShowsUsage(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host) // no RUNE_LLM_MODEL set

	engine.OnInput("/agent start")

	if err := engine.DoString("check", `assert(not rune.agent.status().active, "agent must not start with no model available")`); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(host.PrintCalls, "\n")
	if !strings.Contains(joined, "Usage") {
		t.Fatalf("expected a usage message, got:\n%s", joined)
	}
}

func TestAgentCommandStartWhenAlreadyActiveIsNoop(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	engine.OnInput("/agent start big-pickle")
	host.PrintCalls = nil
	engine.OnInput("/agent start some-other-model")

	if err := engine.DoString("check", `
		local s = rune.agent.status()
		assert(s.model == "big-pickle", "second /agent start must not replace the running model, got " .. tostring(s.model))
	`); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(host.PrintCalls, "\n")
	if !strings.Contains(joined, "already running") {
		t.Fatalf("expected an already-running notice, got:\n%s", joined)
	}
}

func TestAgentCommandStop(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	engine.OnInput("/agent start big-pickle")
	host.PrintCalls = nil
	engine.OnInput("/agent stop")

	if err := engine.DoString("check", `assert(not rune.agent.status().active, "expected agent stopped")`); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(host.PrintCalls, "\n")
	if !strings.Contains(joined, "stopped") {
		t.Fatalf("expected a stopped confirmation, got:\n%s", joined)
	}
}

func TestAgentCommandStopWhenAlreadyStoppedIsNoop(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	engine.OnInput("/agent stop")

	joined := strings.Join(host.PrintCalls, "\n")
	if !strings.Contains(joined, "already stopped") {
		t.Fatalf("expected an already-stopped notice, got:\n%s", joined)
	}
}

func TestAgentCommandUnknownSubcommandShowsUsage(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	engine.OnInput("/agent bogus")

	joined := strings.Join(host.PrintCalls, "\n")
	if !strings.Contains(joined, "Usage") {
		t.Fatalf("expected a usage message for an unknown subcommand, got:\n%s", joined)
	}
}
