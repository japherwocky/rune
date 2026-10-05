package lua

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mmcdole/rune/text"
)

// Steering tests: the observation handed to the model each turn, the
// operator-set mission, and the per-turn tool-hop cap. These exist
// because a live run against a GMCP-less MUD showed the agent never
// working out that it was reading a room description - the observation
// handed it an empty Room section while the description sat unlabelled
// in the game text, and nothing but its own last sentence steered it.

// lastObservation decodes the most recent LLM request and returns the
// single user message the turn started from.
func lastObservation(t *testing.T, host *MockHost) string {
	t.Helper()
	if len(host.LLMCalls) == 0 {
		t.Fatal("no LLM calls recorded")
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(host.LLMCalls[len(host.LLMCalls)-1].Req.Body), &body); err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]interface{})
	return msgs[0].(map[string]interface{})["content"].(string)
}

// A MUD that sends no GMCP leaves vitals, status, and room empty.
// Printing them as empty JSON answers "where am I" with "nowhere" under
// the exact heading the model needs, so those sections are omitted and
// replaced by one line saying where to look instead.
func TestObservationOmitsEmptyGMCPSections(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	startAgentAndWake(t, engine, host)
	obs := lastObservation(t, host)

	for _, heading := range []string{"## Room", "## Vitals"} {
		if strings.Contains(obs, heading) {
			t.Errorf("expected no %q section without GMCP, got:\n%s", heading, obs)
		}
	}
	if !strings.Contains(obs, "## Status data") {
		t.Errorf("expected the no-structured-data note, got:\n%s", obs)
	}
	if !strings.Contains(obs, "## Recent game output") {
		t.Errorf("expected the game output section, got:\n%s", obs)
	}
}

// The flip side: when the game does send GMCP, the structured sections
// are the better answer and the fallback note must get out of the way.
func TestObservationIncludesGMCPSectionsWhenPresent(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("enable", `rune.perception.enable()`); err != nil {
		t.Fatal(err)
	}
	engine.OnGMCP("Room.Info", `{"num":3001,"name":"Temple Square","exits":["north"]}`)

	startAgentAndWake(t, engine, host)
	obs := lastObservation(t, host)

	if !strings.Contains(obs, "## Room") || !strings.Contains(obs, "Temple Square") {
		t.Errorf("expected a populated Room section, got:\n%s", obs)
	}
	if strings.Contains(obs, "## Status data") {
		t.Errorf("no-structured-data note should be absent when GMCP arrived, got:\n%s", obs)
	}
}

// The mission is the only part of the observation the model did not
// write, so it is the only thing that can hold a long run on course. It
// must be present, labelled as the operator's, and distinct from the
// plan the model writes for itself.
func TestObservationIncludesOperatorMission(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("start", `rune.agent.start({
		model = "claude-haiku-4-5",
		goal = "Get through mud school without dying.",
	})`); err != nil {
		t.Fatal(err)
	}
	engine.OnPrompt(text.NewLine("prompt"), true)
	obs := lastObservation(t, host)

	if !strings.Contains(obs, "## Your mission") ||
		!strings.Contains(obs, "Get through mud school without dying.") {
		t.Errorf("expected the operator mission in the observation, got:\n%s", obs)
	}
	if !strings.Contains(obs, "## Your plan") {
		t.Errorf("mission must not replace the model's own plan section, got:\n%s", obs)
	}
	if err := engine.DoString("check", `
		assert(rune.agent.mission() == "Get through mud school without dying.")
		assert(rune.agent.status().mission == rune.agent.mission())
	`); err != nil {
		t.Fatal(err)
	}
}

func TestSetMissionTakesEffectOnNextTurn(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	id := startAgentAndWake(t, engine, host)
	if obs := lastObservation(t, host); strings.Contains(obs, "## Your mission") {
		t.Fatalf("no mission was set, so no mission section belongs here:\n%s", obs)
	}
	deliverEndTurn(engine, host, id, "wandering")

	if err := engine.DoString("set", `rune.agent.set_mission("Find the temple.")`); err != nil {
		t.Fatal(err)
	}
	engine.OnPrompt(text.NewLine("prompt"), true)
	if obs := lastObservation(t, host); !strings.Contains(obs, "Find the temple.") {
		t.Errorf("expected the new mission on the next turn, got:\n%s", obs)
	}

	if err := engine.DoString("clear", `rune.agent.set_mission(nil)`); err != nil {
		t.Fatal(err)
	}
	if err := engine.DoString("check", `assert(rune.agent.mission() == nil)`); err != nil {
		t.Fatal(err)
	}
}

// A model that keeps asking for tools and never emits end_turn held the
// turn open indefinitely (25 calls in one observed run), which also
// meant agent_turn_end never fired, so everything hanging off the turn
// boundary - the goal update, T9's budget and oscillation checks - never
// ran either. The cap has to close the turn, not merely stop dispatching.
func TestToolHopCapEndsTurn(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("watch", `
		turn_ends = 0
		rune.hooks.on("agent_turn_end", function() turn_ends = turn_ends + 1 end)
	`); err != nil {
		t.Fatal(err)
	}

	id := startAgentAndWake(t, engine, host)

	// Answer every continuation with another tool_use. The cap must stop
	// this well before it can run away.
	const attempts = 40
	stoppedAt := 0
	for i := 0; i < attempts; i++ {
		before := len(host.LLMCalls)
		body := fmt.Sprintf(`{"content":[{"type":"tool_use","id":"call_%d","name":"send_command","input":{"cmd":"look"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`, i)
		engine.OnLLMResult(id, &HTTPResponse{Status: 200, Body: body}, "")
		if len(host.LLMCalls) == before {
			stoppedAt = i + 1
			break
		}
		id = host.LLMCalls[len(host.LLMCalls)-1].ID
	}

	if stoppedAt == 0 {
		t.Fatalf("the turn never stopped continuing after %d tool_use replies", attempts)
	}
	if err := engine.DoString("check", `
		assert(turn_ends == 1, "agent_turn_end should fire exactly once, got " .. turn_ends)
		assert(rune.agent.status().thinking == false, "the turn should be closed")
	`); err != nil {
		t.Fatal(err)
	}
}

// 96_agent_ui.lua logs every tool call and its result under an "[Agent]"
// tag, into the same log read_log reads back - so read_log returned its
// own earlier output, nested one level deeper each time. The agent's own
// sent commands and the governance notices are a real record of what it
// did and must survive the filter.
func TestLogReadbackStripsAgentNarration(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("seed", `
		assert(rune.log.start("session.log"))
		rune.log.write("You are standing in a temple.")
		rune.log.write("[Agent] [tool] read_log -> 20 line(s)")
		rune.log.write("[agent] north")
		rune.log.write("[agent-policy] rate_limit: throttled")
		rune.log.write("[12:00:01] [Agent] thinking about the temple")
	`); err != nil {
		t.Fatal(err)
	}

	id := startAgentAndWake(t, engine, host)
	deliverToolUse(t, engine, host, id, "call_01", "read_log", map[string]any{"lines": 50})
	got, isErr := lastToolResult(t, host)
	if isErr {
		t.Fatalf("read_log reported an error: %s", got)
	}

	for _, want := range []string{
		"You are standing in a temple.",
		"[agent] north",
		"[agent-policy] rate_limit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("read_log dropped %q, which it should keep:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[Agent]") {
		t.Errorf("read_log returned the agent's own narration:\n%s", got)
	}
}
