package lua

// T13 tests (89_memory.lua): the memory stream - recording, the
// three-factor retrieval, eviction, reflection, and the two tools.
// Lua-against-MockHost throughout, the same way the agent core and
// governance are tested: MockHost captures the outbound rune._llm
// requests rune.llm.chat makes and engine.OnLLMResult delivers canned
// Zen-shaped replies synchronously, so a reflection round-trip is a
// straight-line test with no timing in it.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mmcdole/rune/text"
)

// startAgent boots the agent (which is what enables rune.memory) with a
// key configured, the precondition for anything that reflects.
func startAgent(t *testing.T, engine *Engine, host *MockHost) {
	t.Helper()
	withAPIKey(host)
	if err := engine.DoString("start", `rune.agent.start({ model = "claude-haiku-4-5" })`); err != nil {
		t.Fatal(err)
	}
}

// seedStream writes a memory stream straight into the durable store
// before anything has loaded it, which is how these tests get records
// with controlled timestamps (os.time() has one-second granularity, so
// records written by the test itself are all "now").
func seedStream(t *testing.T, engine *Engine, luaRecords string) {
	t.Helper()
	code := `rune.store.set("agent_memory", { next_id = 100, pending = 0, records = ` + luaRecords + ` })`
	if err := engine.DoString("seed", code); err != nil {
		t.Fatal(err)
	}
}

// --- recording ---

func TestMemoryRecordRoundTripsThroughStore(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("record", `
		local rec = rune.memory.record("note", "the smithy is north of the square", { tags = { "room:3054" } })
		assert(rec.id == 1, "first record should get id 1")
		assert(rec.importance == 4, "note should default to importance 4, got " .. tostring(rec.importance))
		local all = rune.memory.all()
		assert(#all == 1, "expected 1 record, got " .. #all)
		assert(all[1].text == "the smithy is north of the square")
		assert(all[1].tags[1] == "room:3054")
	`); err != nil {
		t.Fatal(err)
	}

	raw, ok := host.StoreData["agent_memory"]
	if !ok {
		t.Fatal("nothing persisted under agent_memory")
	}
	if !strings.Contains(raw, "the smithy is north of the square") {
		t.Errorf("record text missing from persisted store: %s", raw)
	}
}

func TestMemoryImportanceDefaultsByKindAndClamps(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("importance", `
		assert(rune.memory.record("room", "a room").importance == 2)
		assert(rune.memory.record("level", "a level").importance == 7)
		assert(rune.memory.record("unknown_kind", "something").importance == 4, "unknown kinds fall back to 4")
		assert(rune.memory.record("note", "way too big", { importance = 99 }).importance == 10)
		assert(rune.memory.record("note", "way too small", { importance = -5 }).importance == 1)
	`); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryRecordRejectsBadInput(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	for _, code := range []string{
		`rune.memory.record(nil, "text")`,
		`rune.memory.record("", "text")`,
		`rune.memory.record("note", "")`,
		`rune.memory.record("note", nil)`,
	} {
		if err := engine.DoString("bad", code); err == nil {
			t.Errorf("expected an error for %q", code)
		}
	}
}

// --- eviction ---

// The cap is the watchdog guarantee (recall scores every record), so it
// has to actually hold - and it must never be a reflection that gets
// dropped to make room, since a reflection is the compressed form of
// the observations it cites.
func TestMemoryEvictionHoldsCapAndKeepsReflections(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	// 400 records at the cap, the oldest of which is a reflection.
	if err := engine.DoString("seed", `
		local recs = {}
		for i = 1, 400 do
			recs[i] = { id = i, t = os.time(), kind = (i == 1) and "reflection" or "room",
			            text = "record " .. i, importance = 2, tags = {} }
		end
		rune.store.set("agent_memory", { next_id = 401, pending = 0, records = recs })
	`); err != nil {
		t.Fatal(err)
	}

	if err := engine.DoString("overflow", `
		rune.memory.record("note", "one over the cap")
		rune.memory.record("note", "two over the cap")

		local all = rune.memory.all()
		assert(#all == 400, "cap not held: " .. #all .. " records")
		assert(all[1].kind == "reflection", "the reflection was evicted; oldest is now " .. all[1].kind)
		assert(all[1].text == "record 1")
		-- The two oldest *observations* went instead.
		assert(all[2].text == "record 4", "expected record 4 to be the oldest survivor, got " .. all[2].text)
		assert(all[400].text == "two over the cap")
	`); err != nil {
		t.Fatal(err)
	}
}

// --- retrieval ---

func TestMemoryRecallEmptyStream(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("recall", `
		local recs = rune.memory.recall()
		assert(type(recs) == "table" and #recs == 0, "expected an empty result, not nil")
	`); err != nil {
		t.Fatal(err)
	}
}

// Recency is the only factor that varies here (identical importance,
// no tags, no query text), so the ordering is the decay curve's doing.
func TestMemoryRecallFavorsRecentWhenAllElseEqual(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	seedStream(t, engine, `{
		{ id = 1, t = os.time() - 86400, kind = "note", text = "a day old",     importance = 5, tags = {} },
		{ id = 2, t = os.time() - 3600,  kind = "note", text = "an hour old",   importance = 5, tags = {} },
		{ id = 3, t = os.time() - 60,    kind = "note", text = "a minute old",  importance = 5, tags = {} },
	}`)

	if err := engine.DoString("recall", `
		local recs = rune.memory.recall({ limit = 3 })
		assert(#recs == 3, "expected 3, got " .. #recs)
		assert(recs[1].text == "a minute old", "got " .. recs[1].text)
		assert(recs[2].text == "an hour old",  "got " .. recs[2].text)
		assert(recs[3].text == "a day old",    "got " .. recs[3].text)
	`); err != nil {
		t.Fatal(err)
	}
}

// Importance is the only varying factor: same second, no tags, no query.
func TestMemoryRecallFavorsImportantWhenSameAge(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("recall", `
		rune.memory.record("note", "mundane",  { importance = 1 })
		rune.memory.record("note", "critical", { importance = 10 })
		rune.memory.record("note", "middling", { importance = 5 })

		local recs = rune.memory.recall({ limit = 1 })
		assert(recs[1].text == "critical", "got " .. recs[1].text)
	`); err != nil {
		t.Fatal(err)
	}
}

// The tag join: records written in the same second with equal
// importance, so relevance alone decides. This is the "what do I know
// about room 3054" case that motivated tags over similarity.
func TestMemoryRecallJoinsOnTags(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("recall", `
		rune.memory.record("note", "about the docks",  { tags = { "room:1000" } })
		rune.memory.record("note", "about the smithy", { tags = { "room:3054" } })
		rune.memory.record("note", "about nothing",    { tags = {} })

		local recs = rune.memory.recall({ limit = 1, tags = { "room:3054" } })
		assert(recs[1].text == "about the smithy", "got " .. recs[1].text)
	`); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryRecallMatchesQueryText(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("recall", `
		rune.memory.record("note", "the fountain is dry")
		rune.memory.record("note", "cityguards patrol the gate")

		local recs = rune.memory.recall({ limit = 1, text = "cityguards", tags = {} })
		assert(recs[1].text == "cityguards patrol the gate", "got " .. recs[1].text)
	`); err != nil {
		t.Fatal(err)
	}
}

// With the paper's equal weights a perfect relevance hit only ties with
// a maximally-important non-match, and the newer-wins tiebreak then puts
// the wrong record first. An explicit question has to rank by what was
// asked about - this pins the case that exposed it.
func TestMemoryQueryWeightsPutTheMatchFirst(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("weights", `
		rune.memory.record("note", "the smithy buys weapons", { importance = 1 })
		rune.memory.record("reflection", "cityguards are fatal at this level", { importance = 10 })

		-- Default weights: the important, newer memory leads.
		local ambient = rune.memory.recall({ limit = 1, text = "smithy", tags = {} })
		assert(ambient[1].kind == "reflection", "got " .. ambient[1].text)

		-- Query weights: the record that actually matches leads.
		local asked = rune.memory.recall({ limit = 1, text = "smithy", tags = {},
		                                   weights = rune.memory.query_weights })
		assert(asked[1].text == "the smithy buys weapons", "got " .. asked[1].text)
	`); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryRecallLimitClamped(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("recall", `
		for i = 1, 30 do rune.memory.record("note", "record " .. i) end
		assert(#rune.memory.recall({ limit = 999 }) == 20, "limit should clamp to 20")
		assert(#rune.memory.recall({ limit = 0 }) == 1, "limit should clamp up to 1")
		assert(#rune.memory.recall() == 6, "default limit should be 6")
	`); err != nil {
		t.Fatal(err)
	}
}

// The seam an embedding-backed similarity drops into later.
func TestMemorySetRelevanceSwapsTheFactor(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("swap", `
		rune.memory.record("note", "alpha")
		rune.memory.record("note", "beta")

		-- A relevance function that only ever likes "alpha" must beat
		-- beta's recency advantage.
		rune.memory.set_relevance(function(rec, query)
			return rec.text == "alpha" and 1 or 0
		end)
		assert(rune.memory.recall({ limit = 1 })[1].text == "alpha")

		rune.memory.set_relevance(nil)  -- back to the default
		assert(rune.memory.recall({ limit = 1 })[1].text == "beta", "default relevance not restored")
	`); err != nil {
		t.Fatal(err)
	}
}

// A relevance function that throws must not take the turn down with it -
// recall still returns, that record just scores zero relevance.
func TestMemoryRecallSurvivesThrowingRelevance(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("throwing", `
		rune.memory.record("note", "still here")
		rune.memory.set_relevance(function() error("boom") end)
		local recs = rune.memory.recall({ limit = 1 })
		assert(#recs == 1 and recs[1].text == "still here")
	`); err != nil {
		t.Fatal(err)
	}
}

// --- automatic capture ---

func TestMemoryRecordsNewRoomsOnceEach(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	engine.OnGMCP("Room.Info", `{"num":3054,"name":"The Smithy","area":"Midgaard"}`)
	engine.OnGMCP("Room.Info", `{"num":3054,"name":"The Smithy","area":"Midgaard"}`)
	engine.OnGMCP("Room.Info", `{"num":3001,"name":"Temple Square","area":"Midgaard"}`)

	if err := engine.DoString("check", `
		local all = rune.memory.all()
		assert(#all == 2, "expected 2 room memories (revisits deduped), got " .. #all)
		assert(all[1].text:find("The Smithy", 1, true), "got " .. all[1].text)
		assert(all[1].tags[1] == "room:3054", "got " .. tostring(all[1].tags[1]))
		assert(all[1].tags[2] == "area:Midgaard", "got " .. tostring(all[1].tags[2]))
		assert(all[2].text:find("Temple Square", 1, true), "got " .. all[2].text)
	`); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryRecordsLevelUpsAndFirstFights(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	engine.OnGMCP("Char.Status", `{"level":5,"position":"standing"}`)
	engine.OnGMCP("Char.Status", `{"level":5,"position":"fighting","enemy":"a kobold"}`)
	engine.OnGMCP("Char.Status", `{"level":5,"position":"fighting","enemy":"a kobold"}`)
	engine.OnGMCP("Char.Status", `{"level":6,"position":"standing"}`)

	if err := engine.DoString("check", `
		local all = rune.memory.all()
		assert(#all == 2, "expected first-fight + level-up, got " .. #all)
		assert(all[1].kind == "enemy" and all[1].text:find("kobold", 1, true), "got " .. all[1].text)
		assert(all[2].kind == "level" and all[2].text == "Reached level 6.", "got " .. all[2].text)
	`); err != nil {
		t.Fatal(err)
	}
}

// One bad fight should leave one memory, not one per combat tick.
func TestMemoryRecordsCloseCallsOncePerFight(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	engine.OnGMCP("Char.Vitals", `{"hp":10,"maxhp":100}`)
	engine.OnGMCP("Char.Vitals", `{"hp":8,"maxhp":100}`)
	engine.OnGMCP("Char.Vitals", `{"hp":5,"maxhp":100}`)

	if err := engine.DoString("one", `
		local all = rune.memory.all()
		assert(#all == 1, "expected 1 close-call memory, got " .. #all)
		assert(all[1].kind == "close_call", "got " .. all[1].kind)
	`); err != nil {
		t.Fatal(err)
	}

	// Recovering re-arms it; the next scrape is a new close call.
	engine.OnGMCP("Char.Vitals", `{"hp":90,"maxhp":100}`)
	engine.OnGMCP("Char.Vitals", `{"hp":5,"maxhp":100}`)

	if err := engine.DoString("two", `
		assert(#rune.memory.all() == 2, "recovering should re-arm the close-call record")
	`); err != nil {
		t.Fatal(err)
	}
}

// Tells are kept; the broadcast channels are the MUD's background noise.
func TestMemoryRecordsTellsNotGossip(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	engine.OnGMCP("Comm.Channel", `{"chan":"gossip","msg":"anyone selling a sword?","player":"Bubba"}`)
	engine.OnGMCP("Comm.Channel", `{"chan":"tell","msg":"meet me at the temple","player":"Bubba"}`)

	if err := engine.DoString("check", `
		local all = rune.memory.all()
		assert(#all == 1, "expected only the tell, got " .. #all)
		assert(all[1].kind == "tell", "got " .. all[1].kind)
		assert(all[1].text == "Bubba told me: meet me at the temple", "got " .. all[1].text)
		assert(all[1].tags[1] == "player:bubba", "got " .. tostring(all[1].tags[1]))
	`); err != nil {
		t.Fatal(err)
	}
}

// --- lifecycle ---

func TestMemoryEnableIsIdempotentAndDisableUnwinds(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	if err := engine.DoString("again", `
		rune.memory.enable()  -- must not double-register
		assert(rune.memory.is_enabled() == true)
	`); err != nil {
		t.Fatal(err)
	}

	engine.OnGMCP("Room.Info", `{"num":3054,"name":"The Smithy"}`)
	if err := engine.DoString("one", `
		assert(#rune.memory.all() == 1, "double-registered: got " .. #rune.memory.all() .. " records for one room")
	`); err != nil {
		t.Fatal(err)
	}

	if err := engine.DoString("stop", `rune.agent.stop()`); err != nil {
		t.Fatal(err)
	}
	engine.OnGMCP("Room.Info", `{"num":3001,"name":"Temple Square"}`)

	if err := engine.DoString("still-one", `
		assert(rune.memory.is_enabled() == false, "agent.stop() should disable memory")
		assert(#rune.memory.all() == 1, "hooks left behind after disable")
	`); err != nil {
		t.Fatal(err)
	}
}

// disable() stops recording; it must not forget. A restarted agent
// still knows what it learned.
func TestMemorySurvivesAgentRestart(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	if err := engine.DoString("cycle", `
		rune.memory.record("note", "the smithy buys weapons")
		rune.agent.stop()
		rune.agent.start({ model = "claude-haiku-4-5" })
		local all = rune.memory.all()
		assert(#all == 1 and all[1].text == "the smithy buys weapons", "memory did not survive a restart")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryForgetAll(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("forget", `
		rune.memory.record("reflection", "even reflections go")
		rune.memory.record("note", "and notes")
		rune.memory.forget_all()
		assert(#rune.memory.all() == 0)
		assert(rune.memory.status().pending_importance == 0)
	`); err != nil {
		t.Fatal(err)
	}
}

// --- reflection ---

// pushPastThreshold records enough importance to trip reflection and
// then fires the turn-end hook memory listens on.
func pushPastThreshold(t *testing.T, engine *Engine) {
	t.Helper()
	if err := engine.DoString("push", `
		for i = 1, 6 do rune.memory.record("note", "important thing " .. i, { importance = 10 }) end
		rune.hooks.call("agent_turn_end", { stop_reason = "end_turn" })
	`); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryReflectionFiresOnThresholdAndStoresInsights(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	before := len(host.LLMCalls)
	pushPastThreshold(t, engine)
	if len(host.LLMCalls) != before+1 {
		t.Fatalf("expected a reflection call, went from %d to %d", before, len(host.LLMCalls))
	}

	// It is its own request, not a turn: no tools offered, and it
	// carries the reflection system prompt rather than the agent's.
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(host.LLMCalls[len(host.LLMCalls)-1].Req.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["tools"] != nil {
		t.Errorf("reflection should not offer tools, got %v", body["tools"])
	}
	if system, _ := body["system"].(string); !strings.Contains(system, "memory of an agent") {
		t.Errorf("reflection system prompt: %q", system)
	}

	engine.OnLLMResult(host.LLMCalls[len(host.LLMCalls)-1].ID, &HTTPResponse{
		Status: 200,
		Body: `{"content":[{"type":"text","text":"- Cityguards are fatal at this level.\n2. The smithy is north of the square.\n"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`,
	}, "")

	if err := engine.DoString("check", `
		local all = rune.memory.all()
		local reflections = {}
		for _, rec in ipairs(all) do
			if rec.kind == "reflection" then table.insert(reflections, rec) end
		end
		assert(#reflections == 2, "expected 2 reflections, got " .. #reflections)
		assert(reflections[1].text == "Cityguards are fatal at this level.", "bullet not stripped: " .. reflections[1].text)
		assert(reflections[2].text == "The smithy is north of the square.", "numbering not stripped: " .. reflections[2].text)
		assert(type(reflections[1].refs) == "table" and #reflections[1].refs > 0, "reflection stored no refs")
		assert(rune.memory.status().pending_importance == 0, "counter not reset after reflecting")
	`); err != nil {
		t.Fatal(err)
	}
}

// The counter resets when the request goes out, not when it returns -
// otherwise a reflection that errors leaves the threshold tripped and
// retries on every single subsequent turn end.
func TestMemoryReflectionResetsCounterEvenOnError(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	pushPastThreshold(t, engine)
	engine.OnLLMResult(host.LLMCalls[len(host.LLMCalls)-1].ID, nil, "connection refused")

	before := len(host.LLMCalls)
	if err := engine.DoString("turn-end", `
		assert(rune.memory.status().pending_importance == 0, "counter still tripped after a failed reflection")
		rune.hooks.call("agent_turn_end", { stop_reason = "end_turn" })
	`); err != nil {
		t.Fatal(err)
	}
	if len(host.LLMCalls) != before {
		t.Errorf("a failed reflection retried on the next turn end: %d -> %d", before, len(host.LLMCalls))
	}
}

// 87_agent.lua's `thinking` guard does not cover this call, so
// reflection needs its own.
func TestMemoryReflectionIsSingleFlight(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	pushPastThreshold(t, engine)
	after := len(host.LLMCalls)

	if err := engine.DoString("again", `
		assert(rune.memory.reflect() == false, "a second reflect() while one is in flight must be refused")
	`); err != nil {
		t.Fatal(err)
	}
	if len(host.LLMCalls) != after {
		t.Errorf("single-flight violated: %d -> %d calls", after, len(host.LLMCalls))
	}
}

func TestMemoryReflectRefusedWhileAgentStopped(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	withAPIKey(host)

	if err := engine.DoString("stopped", `
		rune.memory.record("note", "something", { importance = 10 })
		assert(rune.memory.reflect() == false, "reflect() must refuse while the agent is stopped")
	`); err != nil {
		t.Fatal(err)
	}
	if len(host.LLMCalls) != 0 {
		t.Errorf("expected no LLM call, got %d", len(host.LLMCalls))
	}
}

// Reflection tokens are real tokens: they run outside the turn loop, so
// nothing sees them unless the accounting hooks pick agent_reflection up
// alongside agent_reply.
func TestMemoryReflectionTokensAreAccounted(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	pushPastThreshold(t, engine)
	engine.OnLLMResult(host.LLMCalls[len(host.LLMCalls)-1].ID, &HTTPResponse{
		Status: 200,
		Body: `{"content":[{"type":"text","text":"Cityguards are dangerous."}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":700,"output_tokens":300}}`,
	}, "")

	if err := engine.DoString("accounted", `
		local policy = rune.agent_policy.status()
		assert(policy.input_tokens == 700, "governance missed reflection input tokens: " .. policy.input_tokens)
		assert(policy.output_tokens == 300, "governance missed reflection output tokens: " .. policy.output_tokens)
		local ui = rune.agent_ui.summary()
		assert(ui.input_tokens == 700, "the bar missed reflection input tokens: " .. ui.input_tokens)
	`); err != nil {
		t.Fatal(err)
	}
}

// --- tools and prompt integration ---

func TestMemoryToolsAreRegisteredAndQuarantinable(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	engine.OnPrompt(text.NewLine("<100hp> "))

	var body struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(host.LLMCalls[0].Req.Body), &body); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, tool := range body.Tools {
		found[tool.Name] = true
	}
	for _, name := range []string{"remember", "recall"} {
		if !found[name] {
			t.Errorf("tool %q not offered to the model", name)
		}
	}
}

func TestMemoryRememberToolStores(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	engine.OnPrompt(text.NewLine("<100hp> "))
	deliverToolUse(t, engine, host, host.LLMCalls[0].ID, "tu-1", "remember", map[string]interface{}{
		"text":       "the cityguard one-shots me",
		"importance": 9,
		"tags":       []string{"mob:cityguard"},
	})

	content, isError := lastToolResult(t, host)
	if isError {
		t.Fatalf("remember returned an error: %s", content)
	}
	if !strings.Contains(content, "remembered") {
		t.Errorf("unexpected tool result: %q", content)
	}

	if err := engine.DoString("check", `
		local all = rune.memory.all()
		assert(#all == 1, "expected 1 record, got " .. #all)
		assert(all[1].text == "the cityguard one-shots me")
		assert(all[1].importance == 9, "model-supplied importance ignored: " .. all[1].importance)
		assert(all[1].tags[1] == "mob:cityguard")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryRecallToolReturnsFormattedMemories(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	if err := engine.DoString("seed", `
		rune.memory.record("note", "the smithy buys weapons", { tags = { "room:3054" } })
	`); err != nil {
		t.Fatal(err)
	}

	engine.OnPrompt(text.NewLine("<100hp> "))
	deliverToolUse(t, engine, host, host.LLMCalls[0].ID, "tu-1", "recall", map[string]interface{}{
		"query": "smithy",
	})

	content, isError := lastToolResult(t, host)
	if isError {
		t.Fatalf("recall returned an error: %s", content)
	}
	if !strings.Contains(content, "the smithy buys weapons") {
		t.Errorf("recall did not return the memory: %q", content)
	}
}

func TestMemoryRecallToolOnEmptyStream(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	engine.OnPrompt(text.NewLine("<100hp> "))
	deliverToolUse(t, engine, host, host.LLMCalls[0].ID, "tu-1", "recall", map[string]interface{}{})

	content, isError := lastToolResult(t, host)
	if isError {
		t.Fatalf("recall on an empty stream should not be an error: %s", content)
	}
	if content != "no memories yet" {
		t.Errorf("got %q", content)
	}
}

// The observation carries memories as established fact, distinct from
// the goal line, which is explicitly the model's own unverified words.
func TestMemorySectionAppearsInObservation(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	if err := engine.DoString("seed", `
		rune.memory.record("note", "the cityguard one-shots me", { importance = 9 })
	`); err != nil {
		t.Fatal(err)
	}

	engine.OnPrompt(text.NewLine("<100hp> "))

	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(host.LLMCalls[0].Req.Body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) == 0 {
		t.Fatal("no messages in the request")
	}
	content := body.Messages[0].Content
	if !strings.Contains(content, "What you've learned") {
		t.Errorf("observation is missing the memory section:\n%s", content)
	}
	if !strings.Contains(content, "the cityguard one-shots me") {
		t.Errorf("observation is missing the memory text:\n%s", content)
	}
}

// An empty stream must not push an empty heading into every prompt.
func TestMemorySectionOmittedWhenEmpty(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	startAgent(t, engine, host)

	engine.OnPrompt(text.NewLine("<100hp> "))

	if strings.Contains(host.LLMCalls[0].Req.Body, "What you've learned") {
		t.Error("empty memory stream should contribute no section at all")
	}
}
