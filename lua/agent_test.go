package lua

// Agent core tests (87_agent.lua): the state machine, cadence, and
// tool dispatch seam (see PLAN.md T5). Driven the same way as the
// LLM client tests - MockHost captures the outbound rune.http.post
// calls that rune.llm.chat makes, and engine.OnHTTPResult delivers
// canned Zen-shaped responses synchronously. withAPIKey is shared
// with llm_test.go (same package).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mmcdole/rune/text"
)

func TestAgentStartRequiresModel(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	for _, code := range []string{
		`rune.agent.start()`,
		`rune.agent.start({})`,
		`rune.agent.start({ model = "" })`,
	} {
		if err := engine.DoString("test", code); err == nil {
			t.Errorf("expected error for %q", code)
		}
	}
}

func TestAgentStartEnablesPerception(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("start", `rune.agent.start({ model = "deepseek-v4-flash-free" })`); err != nil {
		t.Fatal(err)
	}
	if err := engine.DoString("check", `
		assert(rune.agent.is_active() == true)
		assert(rune.perception.is_enabled() == true, "perception should be enabled by agent.start()")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentStartIsIdempotent(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("start", `
		rune.agent.start({ model = "deepseek-v4-flash-free" })
		rune.agent.start({ model = "some-other-model" }) -- must not double-register
	`); err != nil {
		t.Fatal(err)
	}

	repeating := 0
	for _, timer := range host.ScheduledTimers {
		if timer.Repeat {
			repeating++
		}
	}
	if repeating != 1 {
		t.Fatalf("expected exactly 1 debounce timer after two start() calls, got %d", repeating)
	}
}

func TestAgentPromptWakesAndSendsRequest(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("start", `rune.agent.start({ model = "deepseek-v4-flash-free" })`); err != nil {
		t.Fatal(err)
	}

	engine.OnPrompt(text.NewLine("<100hp 100mv> "))

	if len(host.HTTPCalls) != 1 {
		t.Fatalf("expected 1 HTTP call after a prompt, got %d", len(host.HTTPCalls))
	}

	var body map[string]interface{}
	if err := json.Unmarshal([]byte(host.HTTPCalls[0].Req.Body), &body); err != nil {
		t.Fatalf("request body not valid JSON: %v", err)
	}
	if body["model"] != "deepseek-v4-flash-free" {
		t.Errorf("model: %v", body["model"])
	}
	if body["max_tokens"].(float64) != 1024 {
		t.Errorf("expected default max_tokens 1024, got %v", body["max_tokens"])
	}
	// T6's tools (88_agent_tools.lua) register unconditionally at core
	// load, same as every other module - so they're present in every
	// session's request, not just one that opted in.
	toolNames := map[string]bool{}
	for _, def := range body["tools"].([]interface{}) {
		toolNames[def.(map[string]interface{})["name"].(string)] = true
	}
	for _, want := range []string{"send_command", "speak", "create_trigger", "create_alias", "remove_group", "list_automation"} {
		if !toolNames[want] {
			t.Errorf("expected built-in tool %q in request tools, got %v", want, toolNames)
		}
	}
	msgs := body["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !strings.Contains(content, "## Goal") || !strings.Contains(content, "none yet") {
		t.Errorf("observation message missing expected sections: %s", content)
	}

	if err := engine.DoString("check", `assert(rune.agent.status().thinking == true)`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSingleFlightThenCoalescedRewake(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("start", `rune.agent.start({ model = "deepseek-v4-flash-free" })`); err != nil {
		t.Fatal(err)
	}

	engine.OnPrompt(text.NewLine("prompt 1"))
	if len(host.HTTPCalls) != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", len(host.HTTPCalls))
	}

	// A second wake while the first think is in flight must not launch
	// a second call (single-flight).
	engine.OnPrompt(text.NewLine("prompt 2"))
	if len(host.HTTPCalls) != 1 {
		t.Fatalf("single-flight violated: expected still 1 HTTP call, got %d", len(host.HTTPCalls))
	}

	// Resolving the first think with no further tool use must
	// immediately fire a second call, since a wake (prompt 2) arrived
	// during the think ("think again on return").
	engine.OnHTTPResult(host.HTTPCalls[0].ID, &HTTPResponse{
		Status: 200,
		Body:   `{"content":[{"type":"text","text":"resting"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	}, "")

	if len(host.HTTPCalls) != 2 {
		t.Fatalf("expected a coalesced re-think to fire a 2nd call, got %d calls", len(host.HTTPCalls))
	}
}

func TestAgentToolUseDispatchAndContinuesTurn(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	err := engine.DoString("setup", `
		rune.agent.register_tool("fake_tool", "a fake tool for testing",
			{ type = "object", properties = { foo = { type = "string" } } },
			function(input)
				fake_tool_calls = (fake_tool_calls or 0) + 1
				fake_tool_input = input
				return "did the thing"
			end)
		rune.agent.start({ model = "deepseek-v4-flash-free" })
	`)
	if err != nil {
		t.Fatal(err)
	}

	engine.OnPrompt(text.NewLine("prompt"))
	if len(host.HTTPCalls) != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", len(host.HTTPCalls))
	}

	// The first request should advertise the registered tool.
	var firstBody map[string]interface{}
	if err := json.Unmarshal([]byte(host.HTTPCalls[0].Req.Body), &firstBody); err != nil {
		t.Fatal(err)
	}
	toolDefs, _ := firstBody["tools"].([]interface{})
	found := false
	for _, def := range toolDefs {
		if def.(map[string]interface{})["name"] == "fake_tool" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected fake_tool among tools (alongside T6's built-ins), got %v", firstBody["tools"])
	}

	engine.OnHTTPResult(host.HTTPCalls[0].ID, &HTTPResponse{
		Status: 200,
		Body: `{
			"content": [
				{"type": "text", "text": "Let me check that."},
				{"type": "tool_use", "id": "toolu_1", "name": "fake_tool", "input": {"foo": "bar"}}
			],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 10, "output_tokens": 8}
		}`,
	}, "")

	if err := engine.DoString("check-dispatched", `
		assert(fake_tool_calls == 1, "expected fake_tool to be called once, got " .. tostring(fake_tool_calls))
		assert(fake_tool_input.foo == "bar", "tool input not passed through")
	`); err != nil {
		t.Fatal(err)
	}

	if len(host.HTTPCalls) != 2 {
		t.Fatalf("expected the turn to continue with a 2nd call, got %d", len(host.HTTPCalls))
	}

	var secondBody map[string]interface{}
	if err := json.Unmarshal([]byte(host.HTTPCalls[1].Req.Body), &secondBody); err != nil {
		t.Fatal(err)
	}
	msgs := secondBody["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (observation, assistant, tool_result), got %d", len(msgs))
	}
	assistantMsg := msgs[1].(map[string]interface{})
	if assistantMsg["role"] != "assistant" {
		t.Errorf("messages[1] should be the assistant turn echoed back, got role=%v", assistantMsg["role"])
	}
	toolResultMsg := msgs[2].(map[string]interface{})
	toolResultContent := toolResultMsg["content"].([]interface{})[0].(map[string]interface{})
	if toolResultContent["type"] != "tool_result" || toolResultContent["tool_use_id"] != "toolu_1" {
		t.Fatalf("unexpected tool_result block: %v", toolResultContent)
	}
	if toolResultContent["content"] != "did the thing" {
		t.Errorf("tool_result content: %v", toolResultContent["content"])
	}
	if _, hasError := toolResultContent["is_error"]; hasError {
		t.Errorf("successful tool call should not set is_error, got %v", toolResultContent["is_error"])
	}

	// Complete the turn.
	engine.OnHTTPResult(host.HTTPCalls[1].ID, &HTTPResponse{
		Status: 200,
		Body:   `{"content":[{"type":"text","text":"All clear, resting now."}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":4}}`,
	}, "")

	if err := engine.DoString("check-complete", `
		local s = rune.agent.status()
		assert(s.thinking == false, "should be idle after end_turn")
		assert(s.goal == "All clear, resting now.", "goal: " .. tostring(s.goal))
	`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentUnknownToolReportsErrorInToolResult(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("start", `rune.agent.start({ model = "deepseek-v4-flash-free" })`); err != nil {
		t.Fatal(err)
	}
	engine.OnPrompt(text.NewLine("prompt"))

	engine.OnHTTPResult(host.HTTPCalls[0].ID, &HTTPResponse{
		Status: 200,
		Body: `{
			"content": [{"type": "tool_use", "id": "toolu_1", "name": "no_such_tool", "input": {}}],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`,
	}, "")

	if len(host.HTTPCalls) != 2 {
		t.Fatalf("expected the turn to continue even for an unknown tool, got %d calls", len(host.HTTPCalls))
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(host.HTTPCalls[1].Req.Body), &body); err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]interface{})
	toolResult := msgs[2].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
	if toolResult["is_error"] != true {
		t.Errorf("expected is_error=true for an unknown tool, got %v", toolResult["is_error"])
	}
	if !strings.Contains(toolResult["content"].(string), "unknown tool") {
		t.Errorf("expected content to mention 'unknown tool', got %v", toolResult["content"])
	}
}

func TestAgentLowHPWakesButDoesNotThinkImmediately(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("start", `rune.agent.start({ model = "deepseek-v4-flash-free" })`); err != nil {
		t.Fatal(err)
	}

	engine.OnGMCP("Char.Vitals", `{"hp":20,"maxhp":100,"mana":50,"maxmana":50,"move":50,"maxmove":50}`)

	if len(host.HTTPCalls) != 0 {
		t.Fatalf("a salient GMCP event alone should not immediately think, got %d calls", len(host.HTTPCalls))
	}
	if err := engine.DoString("check", `assert(rune.agent.status().wake_pending == true)`); err != nil {
		t.Fatal(err)
	}

	// The debounce timer is the fallback that turns the pending wake
	// into an actual think.
	timers := host.DrainScheduledTimers()
	var debounceID int
	for _, timer := range timers {
		if timer.Repeat {
			debounceID = timer.ID
		}
	}
	if debounceID == 0 {
		t.Fatal("no repeating (debounce) timer was scheduled by start()")
	}
	engine.OnTimer(debounceID)

	if len(host.HTTPCalls) != 1 {
		t.Fatalf("expected the debounce tick to fire the pending wake, got %d calls", len(host.HTTPCalls))
	}
}

func TestAgentCombatStartIsEdgeTriggered(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("start", `rune.agent.start({ model = "deepseek-v4-flash-free" })`); err != nil {
		t.Fatal(err)
	}

	assertWake := func(label string, want bool) {
		t.Helper()
		script := `assert(rune.agent.status().wake_pending == ` + boolLua(want) + `, "` + label + `")`
		if err := engine.DoString("check", script); err != nil {
			t.Fatal(err)
		}
	}

	assertWake("baseline", false)

	engine.OnGMCP("Char.Status", `{"level":1,"position":"standing"}`)
	assertWake("standing should not wake", false)

	engine.OnGMCP("Char.Status", `{"level":1,"position":"fighting"}`)
	assertWake("entering combat should wake", true)

	// Consume the wake via the debounce timer so we can observe the
	// next delta in isolation.
	timers := host.DrainScheduledTimers()
	for _, timer := range timers {
		if timer.Repeat {
			engine.OnTimer(timer.ID)
		}
	}
	assertWake("wake should be consumed by the think", false)

	engine.OnGMCP("Char.Status", `{"level":1,"position":"fighting"}`)
	assertWake("still fighting (no transition) should not re-wake", false)

	engine.OnGMCP("Char.Status", `{"level":1,"position":"standing"}`)
	assertWake("disengaging should not wake", false)

	engine.OnGMCP("Char.Status", `{"level":1,"position":"fighting"}`)
	assertWake("re-entering combat should wake again", true)
}

func TestAgentChannelMessageWakes(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("start", `rune.agent.start({ model = "deepseek-v4-flash-free" })`); err != nil {
		t.Fatal(err)
	}

	engine.OnGMCP("Comm.Channel", `{"chan":"tell","msg":"Bubba tells you 'hey'","player":"Bubba"}`)

	if len(host.HTTPCalls) != 0 {
		t.Fatalf("a channel message alone should not immediately think, got %d calls", len(host.HTTPCalls))
	}
	if err := engine.DoString("check", `assert(rune.agent.status().wake_pending == true)`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentStopUnwindsAndDropsInFlightResult(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("start", `rune.agent.start({ model = "deepseek-v4-flash-free" })`); err != nil {
		t.Fatal(err)
	}
	engine.OnPrompt(text.NewLine("prompt"))
	if len(host.HTTPCalls) != 1 {
		t.Fatalf("expected 1 HTTP call in flight, got %d", len(host.HTTPCalls))
	}

	if err := engine.DoString("stop", `rune.agent.stop()`); err != nil {
		t.Fatal(err)
	}
	if err := engine.DoString("check-stopped", `
		assert(rune.agent.is_active() == false)
		assert(rune.perception.is_enabled() == false)
	`); err != nil {
		t.Fatal(err)
	}

	// The in-flight call's result lands after stop() - it must be
	// dropped, not acted on.
	engine.OnHTTPResult(host.HTTPCalls[0].ID, &HTTPResponse{
		Status: 200,
		Body:   `{"content":[{"type":"text","text":"should be ignored"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	}, "")

	if err := engine.DoString("check-dropped", `
		local s = rune.agent.status()
		assert(s.thinking == false)
		assert(s.goal ~= "should be ignored", "a stale result must not update goal after stop()")
	`); err != nil {
		t.Fatal(err)
	}
	if len(host.HTTPCalls) != 1 {
		t.Fatalf("a dropped result must not launch a new call, got %d calls", len(host.HTTPCalls))
	}

	// The prompt hook itself must be gone, not just internally
	// disabled - firing a prompt post-stop should not even set the
	// wake flag.
	engine.OnPrompt(text.NewLine("prompt after stop"))
	if err := engine.DoString("check-hook-removed", `assert(rune.agent.status().wake_pending == false)`); err != nil {
		t.Fatal(err)
	}
}


func boolLua(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
