package lua

// T9 governance tests (91_agent_policy.lua): the irreversible-command
// denylist, the rate limiter, oscillation detection, the budget cap,
// and the quarantine -> re-plan hook (00_init.lua's new "quarantined"
// event). Most exercise rune.agent_policy.send directly, the same
// choke point 88_agent_tools.lua's send_command/speak/create_trigger
// now call instead of rune.send - see agent_tools_test.go for the
// tool-level integration proof (TestAgentToolsSendCommandRespectsDenylist).
//
// Rate-limit tests monkeypatch the Lua global os.time (an ordinary
// reassignable stdlib global in gopher-lua, not part of the rune._*
// Go-primitive surface) so the fixed-window check is deterministic
// without real sleeps - the same "small, contained seam" instinct as
// session/llm_test.go's llmRetryBackoff var, just at the Lua layer.

import (
	"strings"
	"testing"

	"github.com/mmcdole/rune/text"
)

func TestAgentPolicySendDeniesQuit(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("test", `
		local ok, err, reason = rune.agent_policy.send("quit")
		assert(ok == nil, "expected send(\"quit\") to be denied")
		assert(reason == "denied", "reason: " .. tostring(reason))
		assert(tostring(err):find("quit", 1, true), "err should mention the command: " .. tostring(err))
	`); err != nil {
		t.Fatal(err)
	}
	if sent := host.DrainNetworkCalls(); len(sent) != 0 {
		t.Fatalf("a denylisted command must never reach the wire, got %v", sent)
	}
}

func TestAgentPolicySendAllowsOrdinaryCommand(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("test", `
		local ok, err = rune.agent_policy.send("look")
		assert(ok == true, "expected send(\"look\") to succeed: " .. tostring(err))
	`); err != nil {
		t.Fatal(err)
	}
	sent := host.DrainNetworkCalls()
	if len(sent) != 1 || sent[0] != "look" {
		t.Fatalf(`expected ["look"] sent, got %v`, sent)
	}
}

func TestAgentPolicyDenyAddsCustomPattern(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("test", `
		rune.agent_policy.deny("^drop all$")
		local ok1, _, reason1 = rune.agent_policy.send("drop all")
		assert(ok1 == nil and reason1 == "denied", "expected the custom pattern to deny \"drop all\"")

		local ok2 = rune.agent_policy.send("drop sword")
		assert(ok2 == true, "a non-matching command must still be allowed")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentPolicyDenyRejectsInvalidPattern(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("test", `rune.agent_policy.deny("(unclosed")`); err == nil {
		t.Fatal("expected an error for an invalid regex pattern")
	}
}

func TestAgentPolicyRateLimitThrottlesBurst(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("test", `
		os.time = function() return 1000 end
		local sent = 0
		for i = 1, 3 do
			if rune.agent_policy.send("look") then sent = sent + 1 end
		end
		assert(sent == 3, "expected all 3 within the default 3/sec cap to succeed, got " .. sent)

		local ok, err, reason = rune.agent_policy.send("look")
		assert(ok == nil, "expected a 4th send within the same second to be throttled")
		assert(reason == "rate_limited", "reason: " .. tostring(reason))

		os.time = function() return 1001 end
		local ok2 = rune.agent_policy.send("look")
		assert(ok2 == true, "expected a send in the next second to succeed")
	`); err != nil {
		t.Fatal(err)
	}
	sent := host.DrainNetworkCalls()
	if len(sent) != 4 {
		t.Fatalf("expected 4 commands to actually reach the wire (3 + 1 after the window rolled over), got %d: %v", len(sent), sent)
	}
}

func TestAgentPolicyConfigureChangesRateLimit(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("test", `
		rune.agent_policy.configure({ max_commands_per_second = 1 })
		os.time = function() return 2000 end
		assert(rune.agent_policy.send("look") == true)
		local ok = rune.agent_policy.send("look")
		assert(ok == nil, "expected the 2nd send to be throttled once the cap is 1/sec")
	`); err != nil {
		t.Fatal(err)
	}
	if sent := host.DrainNetworkCalls(); len(sent) != 1 {
		t.Fatalf("expected exactly 1 command to reach the wire, got %v", sent)
	}
}

// Oscillation is exercised directly against rune.agent_policy.send
// (not through a full tool_use turn) so the test is not also
// incidentally exercising 87_agent.lua's tool dispatch - perception is
// never fed any GMCP here, so its snapshot is trivially unchanged
// across every send, satisfying the "no state change" condition
// deterministically rather than by luck.
func TestAgentPolicyOscillationWakesIdleAgent(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("start", `rune.agent.start({ model = "deepseek-v4-flash-free" })`); err != nil {
		t.Fatal(err)
	}
	if len(host.LLMCalls) != 0 {
		t.Fatalf("starting must not itself think, got %d calls", len(host.LLMCalls))
	}

	if err := engine.DoString("oscillate", `
		rune.agent_policy.configure({ max_commands_per_second = 100 })
		for i = 1, 4 do
			assert(rune.agent_policy.send("flee") == true)
		end
	`); err != nil {
		t.Fatal(err)
	}

	if len(host.LLMCalls) != 1 {
		t.Fatalf("expected oscillation to wake the idle agent into a new think, got %d LLM calls", len(host.LLMCalls))
	}
}

func TestAgentPolicyOscillationResetsAfterFiring(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("start", `
		rune.agent.start({ model = "deepseek-v4-flash-free" })
		rune.agent_policy.configure({ max_commands_per_second = 100 })
		for i = 1, 4 do
			assert(rune.agent_policy.send("flee") == true)
		end
	`); err != nil {
		t.Fatal(err)
	}
	if len(host.LLMCalls) != 1 {
		t.Fatalf("expected the first 4-in-a-row to wake once, got %d LLM calls", len(host.LLMCalls))
	}

	// Two more repeats: the ring was cleared after firing, so this must
	// NOT immediately re-fire (it would need 4 fresh repeats again).
	if err := engine.DoString("more", `
		assert(rune.agent_policy.send("flee") == true)
		assert(rune.agent_policy.send("flee") == true)
	`); err != nil {
		t.Fatal(err)
	}
	if len(host.LLMCalls) != 1 {
		t.Fatalf("expected no second wake after only 2 more repeats, got %d LLM calls", len(host.LLMCalls))
	}
}

func TestAgentPolicyBudgetCapStopsAgent(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("configure", `
		rune.agent_policy.configure({
			pricing = { input_per_million = 3, output_per_million = 15 },
			budget_usd = 0.001,
		})
	`); err != nil {
		t.Fatal(err)
	}

	id := startAgentAndWake(t, engine, host)
	if err := engine.DoString("check-active", `assert(rune.agent.is_active(), "should still be active before the budget trips")`); err != nil {
		t.Fatal(err)
	}

	// (100000/1e6)*3 + (50000/1e6)*15 = 0.3 + 0.75 = $1.05, comfortably
	// over the $0.001 cap.
	engine.OnLLMResult(id, &HTTPResponse{
		Status: 200,
		Body:   `{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":100000,"output_tokens":50000}}`,
	}, "")

	if err := engine.DoString("check-stopped", `assert(not rune.agent.is_active(), "expected the budget cap to stop the agent")`); err != nil {
		t.Fatal(err)
	}
}

// Mirrors 96_agent_ui.lua's own documented principle: fabricating a $
// figure from guessed pricing would be worse than not enforcing a cap
// at all, so an unpriced budget_usd must never trip.
func TestAgentPolicyBudgetInactiveWithoutPricing(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("configure", `rune.agent_policy.configure({ budget_usd = 0.000001 })`); err != nil {
		t.Fatal(err)
	}

	id := startAgentAndWake(t, engine, host)
	engine.OnLLMResult(id, &HTTPResponse{
		Status: 200,
		Body:   `{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":100000,"output_tokens":50000}}`,
	}, "")

	if err := engine.DoString("check-still-active", `
		assert(rune.agent.is_active(), "without pricing configured, a $ budget must never trip")
	`); err != nil {
		t.Fatal(err)
	}
}

// The payoff loop: an agent-owned reflex that keeps failing gets
// quarantined by the existing 3-failures machinery (00_init.lua), and
// this module's "quarantined" listener wakes the LLM because the
// trigger's group starts with "agent-".
func TestAgentPolicyQuarantinedAgentTriggerWakesAgent(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	id := startAgentAndWake(t, engine, host)
	id = deliverToolUse(t, engine, host, id, "toolu_1", "create_trigger", map[string]interface{}{
		"pattern": "^A large kobold",
		"command": "quit", // denylisted -> every firing raises, tripping quarantine
		"group":   "combat",
	})
	deliverEndTurn(engine, host, id, "reflex installed")

	host.DrainNetworkCalls()
	before := len(host.LLMCalls)

	for i := 0; i < 3; i++ {
		engine.OnOutput(text.NewLine("A large kobold is here, looking mean."))
	}

	if sent := host.DrainNetworkCalls(); len(sent) != 0 {
		t.Fatalf("a denylisted reflex must never reach the wire, got %v", sent)
	}

	if err := engine.DoString("check-quarantined", `
		local triggers = rune.trigger.list()
		assert(#triggers == 1, "expected the trigger to still exist (quarantined, not removed)")
		assert(triggers[1].enabled == false, "expected the trigger to be quarantined")
	`); err != nil {
		t.Fatal(err)
	}

	if len(host.LLMCalls) != before+1 {
		t.Fatalf("expected quarantine to wake the agent into a new think, had %d now have %d", before, len(host.LLMCalls))
	}
}

// The scoping half of the same story: a human's own quarantined
// trigger (no "agent-" group) must never wake somebody else's bot.
func TestAgentPolicyIgnoresNonAgentQuarantine(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("setup", `
		rune.agent.start({ model = "deepseek-v4-flash-free" })
		rune.trigger.regex("^boom$", function() error("nope") end, {})
	`); err != nil {
		t.Fatal(err)
	}
	if len(host.LLMCalls) != 0 {
		t.Fatalf("starting must not itself think, got %d calls", len(host.LLMCalls))
	}

	for i := 0; i < 3; i++ {
		engine.OnOutput(text.NewLine("boom"))
	}

	if err := engine.DoString("check", `
		local triggers = rune.trigger.list()
		assert(#triggers == 1 and triggers[1].enabled == false, "expected the human trigger to be quarantined")
	`); err != nil {
		t.Fatal(err)
	}
	if len(host.LLMCalls) != 0 {
		t.Fatalf("a human trigger's quarantine must never wake the agent, got %d LLM calls", len(host.LLMCalls))
	}
}

func TestAgentPolicyStatusReflectsConfig(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("test", `
		rune.agent_policy.configure({ max_commands_per_second = 7, oscillation_window = 5 })
		local s = rune.agent_policy.status()
		assert(s.max_commands_per_second == 7, tostring(s.max_commands_per_second))
		assert(s.oscillation_window == 5, tostring(s.oscillation_window))
		assert(s.cost == nil, "cost must stay nil without pricing configured")
		assert(#s.denylist >= 1, "expected the default denylist entry")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentPolicyCommandPrintsStatus(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("test", `rune.command.dispatch("policy", "")`); err != nil {
		t.Fatal(err)
	}
	printed := strings.Join(host.DrainPrintCalls(), "\n")
	if !strings.Contains(printed, "Policy") {
		t.Fatalf("expected /policy output to mention Policy, got %q", printed)
	}
	if !strings.Contains(printed, "no cap configured") {
		t.Fatalf("expected /policy to report no budget cap by default, got %q", printed)
	}
}
