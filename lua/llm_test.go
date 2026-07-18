package lua

// LLM client tests (86_llm.lua): the Go-backed transport (rune._llm,
// see PLAN.md T8) against OpenCode Zen's Anthropic-Messages-API-shaped
// endpoint. Driven against MockHost's LLM capture/delivery, same
// approach as api_http_test.go - no real network call, no real key.
//
// The destination URL, auth headers, and 429/529 retry policy are now
// Go's responsibility (session/lua_llm.go), not Lua's, so they are not
// re-asserted here - see session/llm_test.go for those. This file
// covers only what 86_llm.lua itself still owns: building the request
// body, the id -> callback map, and parsing/normalizing the response
// (or surfacing whatever error Go delivers, including a missing API
// key - session/llm_test.go proves that specific error string; here
// it's just another delivered error like a transport failure).

import (
	"encoding/json"
	"strings"
	"testing"
)

// withAPIKey sets the one allowlisted env var (see api_env.go).
// Kept for the agent/tool test helpers that share this package and
// still document "assume a key is configured" at the call site, but
// as of T8 it has no effect on rune.llm.chat itself: MockHost.LLMRequest
// is a dumb recorder, and the real key check now happens Go-side
// (session.Session.LLMRequest), not via rune.env - see
// TestLLMMissingAPIKeyDeliversError in session/llm_test.go.
func withAPIKey(host *MockHost) {
	host.EnvVars = map[string]string{"OPENCODE_API_KEY": "test-key-123"}
}

// withOpenAIProvider sets RUNE_LLM_PROVIDER=openai on the MockHost so
// rune.llm.chat builds/parses the OpenAI chat/completions shape
// (llama.cpp and other local/self-hosted runners, PLAN.md T8b)
// instead of the default Anthropic-Messages shape. These tests never
// reach session.Session.LLMRequest (MockHost.LLMRequest is a dumb
// recorder), so no key needs to be configured here - see
// session/llm_test.go for the Go-side URL/auth coverage.
func withOpenAIProvider(host *MockHost) {
	host.EnvVars = map[string]string{"RUNE_LLM_PROVIDER": "openai"}
}

func TestLLMChatRequestBodyShape(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			system = "You are a MUD agent.",
			messages = {
				{ role = "user", content = "look" },
			},
			max_tokens = 512,
		}, function() end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	if len(host.LLMCalls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(host.LLMCalls))
	}
	call := host.LLMCalls[0]

	var body map[string]interface{}
	if err := json.Unmarshal([]byte(call.Req.Body), &body); err != nil {
		t.Fatalf("request body not valid JSON: %v\nbody: %s", err, call.Req.Body)
	}
	if body["model"] != "deepseek-v4-flash-free" {
		t.Errorf("model: %v", body["model"])
	}
	if body["system"] != "You are a MUD agent." {
		t.Errorf("system: %v", body["system"])
	}
	if body["max_tokens"].(float64) != 512 {
		t.Errorf("max_tokens: %v", body["max_tokens"])
	}
	msgs, ok := body["messages"].([]interface{})
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages: %v", body["messages"])
	}
	if _, hasTools := body["tools"]; hasTools {
		t.Errorf("tools should be omitted when not passed, got %v", body["tools"])
	}
}

func TestLLMChatSuccessTextOnly(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			got_reply = reply
			got_err = err
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	id := host.LLMCalls[0].ID
	engine.OnLLMResult(id, &HTTPResponse{
		Status: 200,
		Body: `{
			"id": "msg_01",
			"type": "message",
			"role": "assistant",
			"content": [{"type": "text", "text": "There is nothing here."}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 12, "output_tokens": 5}
		}`,
	}, "")

	err = engine.DoString("assert", `
		assert(got_err == nil, "err should be nil, got " .. tostring(got_err))
		assert(got_reply.text == "There is nothing here.", "text: " .. tostring(got_reply.text))
		assert(#got_reply.tool_uses == 0, "expected no tool_uses")
		assert(got_reply.stop_reason == "end_turn", "stop_reason: " .. tostring(got_reply.stop_reason))
		assert(got_reply.usage.input_tokens == 12, "input_tokens")
		assert(got_reply.usage.output_tokens == 5, "output_tokens")
		assert(#got_reply.content == 1, "raw content should be preserved")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLLMChatSuccessToolUse(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			messages = { { role = "user", content = "kill kobold" } },
			max_tokens = 512,
		}, function(reply, err)
			got_reply = reply
			got_err = err
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	id := host.LLMCalls[0].ID
	engine.OnLLMResult(id, &HTTPResponse{
		Status: 200,
		Body: `{
			"id": "msg_02",
			"type": "message",
			"role": "assistant",
			"content": [
				{"type": "text", "text": "Let me set up a reflex."},
				{"type": "tool_use", "id": "toolu_01", "name": "create_trigger", "input": {"pattern": "You are dead", "command": "recall"}}
			],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 20, "output_tokens": 15}
		}`,
	}, "")

	err = engine.DoString("assert", `
		assert(got_err == nil, "err should be nil, got " .. tostring(got_err))
		assert(got_reply.text == "Let me set up a reflex.", "text: " .. tostring(got_reply.text))
		assert(got_reply.stop_reason == "tool_use", "stop_reason")
		assert(#got_reply.tool_uses == 1, "expected 1 tool_use, got " .. #got_reply.tool_uses)
		local tu = got_reply.tool_uses[1]
		assert(tu.id == "toolu_01", "tool_use id")
		assert(tu.name == "create_trigger", "tool_use name")
		assert(tu.input.pattern == "You are dead", "tool_use input.pattern")
		assert(tu.input.command == "recall", "tool_use input.command")
		assert(#got_reply.content == 2, "raw content should keep both blocks for the tool_result round-trip")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLLMChatTransportError(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			got_reply = reply
			got_err = err
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Stands in for any Go-side delivery failure, including the
	// missing-API-key case now enforced in session.Session.LLMRequest
	// (see session/llm_test.go) - 86_llm.lua treats every such error
	// identically, just prefixing it for the caller.
	engine.OnLLMResult(host.LLMCalls[0].ID, nil, "dial tcp: timeout")

	err = engine.DoString("assert", `
		assert(got_reply == nil, "reply should be nil")
		assert(got_err:find("timeout"), "err: " .. tostring(got_err))
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLLMChatNon200ExtractsErrorMessage(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			got_reply = reply
			got_err = err
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	// The exact shape observed from a live bad-key request against Zen
	// (see PLAN.md T4): a 401 with a nested error.message.
	engine.OnLLMResult(host.LLMCalls[0].ID, &HTTPResponse{
		Status: 401,
		Body:   `{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}`,
	}, "")

	err = engine.DoString("assert", `
		assert(got_reply == nil, "reply should be nil")
		assert(got_err:find("401"), "err should mention status: " .. tostring(got_err))
		assert(got_err:find("Invalid API key%."), "err should surface the message: " .. tostring(got_err))
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLLMChatNon200FallsBackToRawBody(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			got_err = err
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	engine.OnLLMResult(host.LLMCalls[0].ID, &HTTPResponse{
		Status: 502,
		Body:   "bad gateway",
	}, "")

	err = engine.DoString("assert", `
		assert(got_err:find("502"), "err: " .. tostring(got_err))
		assert(got_err:find("bad gateway"), "err should fall back to raw body: " .. tostring(got_err))
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLLMChatMalformedResponseJSON(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			got_reply = reply
			got_err = err
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	engine.OnLLMResult(host.LLMCalls[0].ID, &HTTPResponse{
		Status: 200,
		Body:   "not json at all",
	}, "")

	err = engine.DoString("assert", `
		assert(got_reply == nil, "reply should be nil")
		assert(got_err:find("malformed response JSON"), "err: " .. tostring(got_err))
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLLMChatBadArgumentsRaise(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	for _, code := range []string{
		`rune.llm.chat("not a table", function() end)`,                                    // not a table
		`rune.llm.chat({ messages = {}, max_tokens = 512 }, function() end)`,               // missing model
		`rune.llm.chat({ model = "", messages = {}, max_tokens = 512 }, function() end)`,    // empty model
		`rune.llm.chat({ model = "m", max_tokens = 512 }, function() end)`,                  // missing messages
		`rune.llm.chat({ model = "m", messages = {} }, function() end)`,                     // missing max_tokens
		`rune.llm.chat({ model = "m", messages = {}, max_tokens = "many" }, function() end)`, // wrong type
		`rune.llm.chat({ model = "m", messages = {}, max_tokens = 512 })`,                   // missing callback
	} {
		if err := engine.DoString("test", code); err == nil {
			t.Errorf("expected error for %q", code)
		}
	}
}

func TestLLMChatCallbackErrorReported(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function()
			error("callback boom")
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	engine.OnLLMResult(host.LLMCalls[0].ID, &HTTPResponse{
		Status: 200,
		Body:   `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	}, "")

	found := false
	for _, p := range host.PrintCalls {
		if strings.Contains(p, "callback boom") {
			found = true
		}
	}
	if !found {
		t.Errorf("callback error not reported; prints: %v", host.PrintCalls)
	}
}

// ---- OpenAI chat/completions shape (PLAN.md T8b: llama.cpp and
// other local/self-hosted runners) ----

func TestLLMChatOpenAIRequestBodyShape(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withOpenAIProvider(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "local-model",
			system = "You are a MUD agent.",
			messages = {
				{ role = "user", content = "look" },
			},
			tools = {
				{ name = "send_command", description = "Send a command", input_schema = { type = "object", properties = {} } },
			},
			max_tokens = 512,
		}, function() end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	if len(host.LLMCalls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(host.LLMCalls))
	}
	call := host.LLMCalls[0]

	var body map[string]interface{}
	if err := json.Unmarshal([]byte(call.Req.Body), &body); err != nil {
		t.Fatalf("request body not valid JSON: %v\nbody: %s", err, call.Req.Body)
	}
	if _, hasSystem := body["system"]; hasSystem {
		t.Errorf("top-level system should be absent for the openai shape, got %v", body["system"])
	}

	msgs, ok := body["messages"].([]interface{})
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages: %v, want [system, user]", body["messages"])
	}
	sysMsg := msgs[0].(map[string]interface{})
	if sysMsg["role"] != "system" || sysMsg["content"] != "You are a MUD agent." {
		t.Errorf("messages[1] = %v, want the system prompt as a leading system message", sysMsg)
	}
	userMsg := msgs[1].(map[string]interface{})
	if userMsg["role"] != "user" || userMsg["content"] != "look" {
		t.Errorf("messages[2] = %v, want the original user message unchanged", userMsg)
	}

	tools, ok := body["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("tools: %v", body["tools"])
	}
	tool := tools[0].(map[string]interface{})
	if tool["type"] != "function" {
		t.Errorf("tools[1].type = %v, want %q", tool["type"], "function")
	}
	fn, ok := tool["function"].(map[string]interface{})
	if !ok || fn["name"] != "send_command" || fn["description"] != "Send a command" {
		t.Fatalf("tools[1].function = %v", tool["function"])
	}
	if _, hasParams := fn["parameters"]; !hasParams {
		t.Errorf("tools[1].function.parameters missing (should carry input_schema through)")
	}
}

func TestLLMChatOpenAISuccessTextOnly(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withOpenAIProvider(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "local-model",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			got_reply = reply
			got_err = err
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	id := host.LLMCalls[0].ID
	engine.OnLLMResult(id, &HTTPResponse{
		Status: 200,
		Body: `{
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "There is nothing here."}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 12, "completion_tokens": 5, "total_tokens": 17}
		}`,
	}, "")

	err = engine.DoString("assert", `
		assert(got_err == nil, "err should be nil, got " .. tostring(got_err))
		assert(got_reply.text == "There is nothing here.", "text: " .. tostring(got_reply.text))
		assert(#got_reply.tool_uses == 0, "expected no tool_uses")
		assert(got_reply.stop_reason == "stop", "stop_reason: " .. tostring(got_reply.stop_reason))
		assert(got_reply.usage.input_tokens == 12, "input_tokens")
		assert(got_reply.usage.output_tokens == 5, "output_tokens")
		assert(#got_reply.content == 1, "raw content should be synthesized for the tool_result round-trip")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLLMChatOpenAISuccessToolCalls(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withOpenAIProvider(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "local-model",
			messages = { { role = "user", content = "kill kobold" } },
			max_tokens = 512,
		}, function(reply, err)
			got_reply = reply
			got_err = err
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	id := host.LLMCalls[0].ID
	// function.arguments arrives as a JSON *string*, unlike
	// Anthropic's already-decoded tool_use.input - proves
	// from_openai_response decodes it rather than handing the model a
	// raw string where a table is expected.
	engine.OnLLMResult(id, &HTTPResponse{
		Status: 200,
		Body: `{
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": null,
					"tool_calls": [
						{"id": "call_01", "type": "function", "function": {"name": "create_trigger", "arguments": "{\"pattern\":\"You are dead\",\"command\":\"recall\"}"}}
					]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 20, "completion_tokens": 15}
		}`,
	}, "")

	err = engine.DoString("assert", `
		assert(got_err == nil, "err should be nil, got " .. tostring(got_err))
		assert(got_reply.stop_reason == "tool_use", "stop_reason should normalize tool_calls -> tool_use, got " .. tostring(got_reply.stop_reason))
		assert(#got_reply.tool_uses == 1, "expected 1 tool_use, got " .. #got_reply.tool_uses)
		local tu = got_reply.tool_uses[1]
		assert(tu.id == "call_01", "tool_use id")
		assert(tu.name == "create_trigger", "tool_use name")
		assert(type(tu.input) == "table", "tool_use input should be a decoded table, got " .. type(tu.input))
		assert(tu.input.pattern == "You are dead", "tool_use input.pattern: " .. tostring(tu.input.pattern))
		assert(tu.input.command == "recall", "tool_use input.command")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

// TestLLMChatOpenAIToolResultContinuation exercises the trickiest
// direction of the translation: a follow-up rune.llm.chat call built
// the same way 87_agent.lua's on_reply constructs one (the prior
// assistant reply's content array echoed back verbatim, plus a user
// message carrying tool_result blocks) must translate into OpenAI's
// flat assistant-with-tool_calls + separate role="tool" messages, and
// a failed tool_result's is_error flag - which OpenAI's tool message
// has no field for - must survive as a text prefix instead of being
// silently dropped.
func TestLLMChatOpenAIToolResultContinuation(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withOpenAIProvider(host)

	err := engine.DoString("test", `
		rune.llm.chat({
			model = "local-model",
			messages = {
				{ role = "user", content = "kill kobold" },
				{ role = "assistant", content = {
					{ type = "text", text = "Let me try that." },
					{ type = "tool_use", id = "call_01", name = "send_command", input = { cmd = "kill kobold" } },
				} },
				{ role = "user", content = {
					{ type = "tool_result", tool_use_id = "call_01", content = "no such target", is_error = true },
				} },
			},
			max_tokens = 512,
		}, function() end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	call := host.LLMCalls[0]
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(call.Req.Body), &body); err != nil {
		t.Fatalf("request body not valid JSON: %v\nbody: %s", err, call.Req.Body)
	}
	msgs, ok := body["messages"].([]interface{})
	if !ok || len(msgs) != 3 {
		t.Fatalf("messages: %v, want [user, assistant-with-tool_calls, tool]", body["messages"])
	}

	assistantMsg := msgs[1].(map[string]interface{})
	if assistantMsg["role"] != "assistant" || assistantMsg["content"] != "Let me try that." {
		t.Errorf("messages[2] = %v, want the assistant's text preserved", assistantMsg)
	}
	toolCalls, ok := assistantMsg["tool_calls"].([]interface{})
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("messages[2].tool_calls: %v", assistantMsg["tool_calls"])
	}
	tc := toolCalls[0].(map[string]interface{})
	if tc["id"] != "call_01" {
		t.Errorf("tool_calls[1].id = %v, want call_01", tc["id"])
	}

	toolMsg := msgs[2].(map[string]interface{})
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_01" {
		t.Fatalf("messages[3] = %v, want a role=tool message for call_01", toolMsg)
	}
	if toolMsg["content"] != "Error: no such target" {
		t.Errorf("messages[3].content = %q, want the is_error flag folded into the text as a prefix", toolMsg["content"])
	}
}
