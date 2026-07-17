package lua

// LLM client tests (86_llm.lua): Phase 1 transport over rune.http
// against OpenCode Zen's Anthropic-Messages-API-shaped endpoint (see
// PLAN.md T4). Driven against MockHost's HTTP capture/delivery, same
// approach as api_http_test.go - no real network call, no real key.

import (
	"encoding/json"
	"strings"
	"testing"
)

// withAPIKey sets the one allowlisted env var (see api_env.go) so
// rune.llm.chat gets past its own key check.
func withAPIKey(host *MockHost) {
	host.EnvVars = map[string]string{"OPENCODE_API_KEY": "test-key-123"}
}

func TestLLMChatRequestShape(t *testing.T) {
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

	if len(host.HTTPCalls) != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", len(host.HTTPCalls))
	}
	call := host.HTTPCalls[0]
	if call.Req.Method != "POST" || call.Req.URL != "https://opencode.ai/zen/v1/messages" {
		t.Errorf("unexpected request: %+v", call.Req)
	}
	if call.Req.Headers["x-api-key"] != "test-key-123" {
		t.Errorf("x-api-key not sent: %+v", call.Req.Headers)
	}
	if call.Req.Headers["anthropic-version"] != "2023-06-01" {
		t.Errorf("anthropic-version not sent: %+v", call.Req.Headers)
	}
	if call.Req.Headers["Authorization"] != "" {
		t.Errorf("Authorization should not be sent (Zen ignores it, see T4): %+v", call.Req.Headers)
	}

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

	id := host.HTTPCalls[0].ID
	engine.OnHTTPResult(id, &HTTPResponse{
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

	id := host.HTTPCalls[0].ID
	engine.OnHTTPResult(id, &HTTPResponse{
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

func TestLLMChatMissingAPIKey(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	// host.EnvVars deliberately left unset - no OPENCODE_API_KEY.

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

	if len(host.HTTPCalls) != 0 {
		t.Fatalf("expected no HTTP call without an API key, got %d", len(host.HTTPCalls))
	}

	err = engine.DoString("assert", `
		assert(got_reply == nil, "reply should be nil")
		assert(got_err ~= nil and got_err:find("OPENCODE_API_KEY"), "err: " .. tostring(got_err))
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLLMChatHTTPTransportError(t *testing.T) {
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

	engine.OnHTTPResult(host.HTTPCalls[0].ID, nil, "dial tcp: timeout")

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
	engine.OnHTTPResult(host.HTTPCalls[0].ID, &HTTPResponse{
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

	engine.OnHTTPResult(host.HTTPCalls[0].ID, &HTTPResponse{
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

	engine.OnHTTPResult(host.HTTPCalls[0].ID, &HTTPResponse{
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
		`rune.llm.chat("not a table", function() end)`,                                       // not a table
		`rune.llm.chat({ messages = {}, max_tokens = 512 }, function() end)`,                  // missing model
		`rune.llm.chat({ model = "", messages = {}, max_tokens = 512 }, function() end)`,       // empty model
		`rune.llm.chat({ model = "m", max_tokens = 512 }, function() end)`,                     // missing messages
		`rune.llm.chat({ model = "m", messages = {} }, function() end)`,                        // missing max_tokens
		`rune.llm.chat({ model = "m", messages = {}, max_tokens = "many" }, function() end)`,    // wrong type
		`rune.llm.chat({ model = "m", messages = {}, max_tokens = 512 })`,                       // missing callback
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

	engine.OnHTTPResult(host.HTTPCalls[0].ID, &HTTPResponse{
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
