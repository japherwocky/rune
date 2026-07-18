package lua

// T6 tool tests (88_agent_tools.lua): send_command, speak,
// create_trigger, create_alias, remove_group, list_automation. These
// drive the *real* agent turn cycle (start -> wake -> canned tool_use
// -> continuation), the same MockHost HTTP mechanism as agent_test.go
// and llm_test.go, rather than reaching into T5's private tool map -
// there is no other entry point, by design (see 87_agent.lua).
//
// The centerpiece is TestAgentToolsCreateTriggerFiresReflex: it
// proves the actual combat walkthrough PLAN.md T6 describes - the
// agent installs a trigger via a tool call, and a later server line
// fires it with zero further LLM involvement.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mmcdole/rune/text"
)

// startAgentAndWake starts the agent and fires a prompt, returning the
// id of the resulting HTTP call.
func startAgentAndWake(t *testing.T, engine *Engine, host *MockHost) int {
	t.Helper()
	if err := engine.DoString("start", `rune.agent.start({ model = "deepseek-v4-flash-free" })`); err != nil {
		t.Fatal(err)
	}
	before := len(host.HTTPCalls)
	engine.OnPrompt(text.NewLine("prompt"))
	if len(host.HTTPCalls) != before+1 {
		t.Fatalf("expected a new HTTP call after prompt, had %d now have %d", before, len(host.HTTPCalls))
	}
	return host.HTTPCalls[len(host.HTTPCalls)-1].ID
}

// deliverToolUse delivers a canned single-tool_use response and
// returns the id of the resulting continuation call.
func deliverToolUse(t *testing.T, engine *Engine, host *MockHost, id int, toolUseID, toolName string, input interface{}) int {
	t.Helper()
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	before := len(host.HTTPCalls)
	body := fmt.Sprintf(`{"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
		toolUseID, toolName, inputJSON)
	engine.OnHTTPResult(id, &HTTPResponse{Status: 200, Body: body}, "")
	if len(host.HTTPCalls) != before+1 {
		t.Fatalf("expected the turn to continue with a new call, had %d now have %d", before, len(host.HTTPCalls))
	}
	return host.HTTPCalls[len(host.HTTPCalls)-1].ID
}

func deliverEndTurn(engine *Engine, host *MockHost, id int, text string) {
	engine.OnHTTPResult(id, &HTTPResponse{
		Status: 200,
		Body: fmt.Sprintf(`{"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			text),
	}, "")
}

// lastToolResult decodes the most recently sent request body and
// returns the first (and, in these single-tool-call tests, only)
// tool_result block's content/is_error.
func lastToolResult(t *testing.T, host *MockHost) (content string, isError bool) {
	t.Helper()
	call := host.HTTPCalls[len(host.HTTPCalls)-1]
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(call.Req.Body), &body); err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]interface{})
	last := msgs[len(msgs)-1].(map[string]interface{})
	block := last["content"].([]interface{})[0].(map[string]interface{})
	content, _ = block["content"].(string)
	isError, _ = block["is_error"].(bool)
	return content, isError
}

func TestAgentToolsSendCommand(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	id := startAgentAndWake(t, engine, host)
	host.DrainNetworkCalls()
	deliverToolUse(t, engine, host, id, "toolu_1", "send_command", map[string]string{"cmd": "kill kobold"})

	sent := host.DrainNetworkCalls()
	if len(sent) != 1 || sent[0] != "kill kobold" {
		t.Fatalf("expected [\"kill kobold\"] sent, got %v", sent)
	}
	content, isError := lastToolResult(t, host)
	if isError {
		t.Errorf("send_command should not error, got content %q", content)
	}
}

func TestAgentToolsSpeak(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]string
		want    string
		wantErr bool
	}{
		{name: "say", input: map[string]string{"channel": "say", "message": "hello"}, want: "say hello"},
		{name: "gossip", input: map[string]string{"channel": "gossip", "message": "anyone selling a sword?"}, want: "gossip anyone selling a sword?"},
		{name: "tell", input: map[string]string{"channel": "tell", "target": "Bubba", "message": "hey"}, want: "tell Bubba hey"},
		{name: "tell without target errors", input: map[string]string{"channel": "tell", "message": "hey"}, wantErr: true},
		{name: "unknown channel errors", input: map[string]string{"channel": "yell", "message": "hey"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, host, cleanup := setupTest(t)
			defer cleanup()
			withAPIKey(host)

			id := startAgentAndWake(t, engine, host)
			host.DrainNetworkCalls()
			deliverToolUse(t, engine, host, id, "toolu_1", "speak", tc.input)

			content, isError := lastToolResult(t, host)
			if isError != tc.wantErr {
				t.Fatalf("isError = %v, want %v (content: %q)", isError, tc.wantErr, content)
			}
			if tc.wantErr {
				return
			}
			sent := host.DrainNetworkCalls()
			if len(sent) != 1 || sent[0] != tc.want {
				t.Fatalf("expected [%q] sent, got %v", tc.want, sent)
			}
		})
	}
}

func TestAgentToolsCreateTriggerFiresReflex(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	id := startAgentAndWake(t, engine, host)
	id = deliverToolUse(t, engine, host, id, "toolu_1", "create_trigger", map[string]interface{}{
		"pattern": "^A large kobold",
		"command": "kill kobold",
		"group":   "combat",
	})
	content, isError := lastToolResult(t, host)
	if isError {
		t.Fatalf("create_trigger should not error, got %q", content)
	}
	if !strings.Contains(content, "agent-combat") {
		t.Errorf("expected confirmation to mention agent-combat, got %q", content)
	}
	deliverEndTurn(engine, host, id, "reflex installed")

	// The whole point: no further LLM involvement. A matching line
	// fires the reflex at machine speed.
	host.DrainNetworkCalls()
	engine.OnOutput(text.NewLine("A large kobold is here, looking mean."))

	sent := host.DrainNetworkCalls()
	if len(sent) != 1 || sent[0] != "kill kobold" {
		t.Fatalf("expected the trigger to fire [\"kill kobold\"], got %v", sent)
	}
	if len(host.HTTPCalls) != 2 {
		t.Fatalf("the reflex must not involve the LLM: expected 2 total HTTP calls, got %d", len(host.HTTPCalls))
	}
}

func TestAgentToolsCreateTriggerCaptureSubstitution(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	id := startAgentAndWake(t, engine, host)
	deliverToolUse(t, engine, host, id, "toolu_1", "create_trigger", map[string]interface{}{
		"pattern": "^(\\S+) flees east",
		"command": "chase %1",
		"group":   "combat",
	})

	host.DrainNetworkCalls()
	engine.OnOutput(text.NewLine("kobold flees east"))
	sent := host.DrainNetworkCalls()
	if len(sent) != 1 || sent[0] != "chase kobold" {
		t.Fatalf("expected capture substitution [\"chase kobold\"], got %v", sent)
	}
}

func TestAgentToolsCreateTriggerNamespacesGroup(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	id := startAgentAndWake(t, engine, host)
	deliverToolUse(t, engine, host, id, "toolu_1", "create_trigger", map[string]interface{}{
		"pattern": "x",
		"command": "y",
		"group":   "combat",
	})

	if err := engine.DoString("check", `
		local triggers = rune.trigger.list()
		assert(#triggers == 1, "expected 1 trigger, got " .. #triggers)
		assert(triggers[1].group == "agent-combat", "group: " .. tostring(triggers[1].group))
	`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentToolsCreateTriggerRejectsInvalidGroup(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	id := startAgentAndWake(t, engine, host)
	deliverToolUse(t, engine, host, id, "toolu_1", "create_trigger", map[string]interface{}{
		"pattern": "x",
		"command": "y",
		"group":   "combat zone", // space is not allowed
	})

	content, isError := lastToolResult(t, host)
	if !isError {
		t.Fatalf("expected an error for a group label containing a space, got content %q", content)
	}
	if !strings.Contains(content, "group") {
		t.Errorf("expected the error to mention the group label, got %q", content)
	}
	if err := engine.DoString("check-none-created", `assert(rune.trigger.count() == 0, "an invalid call must not create a trigger")`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentToolsRemoveGroupClearsTriggersAndAliases(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	id := startAgentAndWake(t, engine, host)

	// One response, two tool_use blocks: a trigger and an alias in the
	// same group.
	before := len(host.HTTPCalls)
	body := `{
		"content": [
			{"type": "tool_use", "id": "toolu_1", "name": "create_trigger", "input": {"pattern": "x", "command": "y", "group": "combat"}},
			{"type": "tool_use", "id": "toolu_2", "name": "create_alias", "input": {"word": "kk", "expansion": "kill kobold", "group": "combat"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 1, "output_tokens": 1}
	}`
	engine.OnHTTPResult(id, &HTTPResponse{Status: 200, Body: body}, "")
	if len(host.HTTPCalls) != before+1 {
		t.Fatalf("expected the turn to continue, got %d calls", len(host.HTTPCalls))
	}
	id = host.HTTPCalls[len(host.HTTPCalls)-1].ID

	if err := engine.DoString("check-created", `
		assert(rune.trigger.count() == 1)
		assert(rune.alias.count() == 1)
	`); err != nil {
		t.Fatal(err)
	}

	id = deliverToolUse(t, engine, host, id, "toolu_3", "remove_group", map[string]string{"group": "combat"})
	content, isError := lastToolResult(t, host)
	if isError {
		t.Fatalf("remove_group should not error, got %q", content)
	}
	if !strings.Contains(content, "2") {
		t.Errorf("expected the count of removed items (2) in the response, got %q", content)
	}

	if err := engine.DoString("check-removed", `
		assert(rune.trigger.count() == 0, "trigger should be removed")
		assert(rune.alias.count() == 0, "alias should be removed")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentToolsListAutomationFiltersToAgentGroups(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	// A human-authored trigger, ungrouped - must never show up in the
	// agent's own view of its automation.
	if err := engine.DoString("human-setup", `rune.trigger.exact("hi", "say hello back")`); err != nil {
		t.Fatal(err)
	}

	id := startAgentAndWake(t, engine, host)
	id = deliverToolUse(t, engine, host, id, "toolu_1", "create_trigger", map[string]interface{}{
		"pattern": "^You have fled",
		"command": "recall",
		"group":   "nav",
	})
	deliverEndTurn(engine, host, id, "installed")

	id = startAgentAndWake(t, engine, host)
	deliverToolUse(t, engine, host, id, "toolu_2", "list_automation", map[string]interface{}{})

	content, isError := lastToolResult(t, host)
	if isError {
		t.Fatalf("list_automation should not error, got %q", content)
	}
	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(content), &items); err != nil {
		t.Fatalf("list_automation content not valid JSON: %v (%s)", err, content)
	}
	if len(items) != 1 {
		t.Fatalf("expected only the agent's own trigger, got %d items: %v", len(items), items)
	}
	if items[0]["group"] != "agent-nav" {
		t.Errorf("group: %v", items[0]["group"])
	}
}

func TestAgentToolsQuarantineAfterRepeatedFailures(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	badInput := map[string]interface{}{"pattern": "x", "command": "y", "group": "bad group"} // space -> always invalid

	id := startAgentAndWake(t, engine, host)
	for i := 0; i < 3; i++ {
		id = deliverToolUse(t, engine, host, id, fmt.Sprintf("toolu_bad_%d", i), "create_trigger", badInput)
		content, isError := lastToolResult(t, host)
		if !isError {
			t.Fatalf("call %d: expected a validation error, got %q", i, content)
		}
		if strings.Contains(content, "disabled") {
			t.Fatalf("call %d: quarantined too early: %q", i, content)
		}
	}

	// A 4th call, this time with valid input, must still fail - the
	// tool was quarantined after 3 consecutive failures.
	goodInput := map[string]interface{}{"pattern": "x", "command": "y", "group": "combat"}
	deliverToolUse(t, engine, host, id, "toolu_good", "create_trigger", goodInput)
	content, isError := lastToolResult(t, host)
	if !isError {
		t.Fatalf("expected the quarantined tool to still fail on valid input, got %q", content)
	}
	if !strings.Contains(content, "disabled") {
		t.Errorf("expected a quarantine message, got %q", content)
	}
	if err := engine.DoString("check-none-created", `assert(rune.trigger.count() == 0, "the quarantined call must not have created a trigger")`); err != nil {
		t.Fatal(err)
	}
}
