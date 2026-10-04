package lua

// Perception tests (84_perception.lua): the agent's world-model, built
// from GMCP + the output hook, driven against MockHost. This is the
// lowest layer that can express these failures (docs/testing.md) -
// nothing here is user-visible yet (that's T7), so there is no e2e
// scenario for it.
//
// The module is inert until rune.perception.enable() runs (it's a core
// file loaded for every session, bot or not - see 84_perception.lua's
// header), so every test that exercises live behavior enables it first.

import (
	"strconv"
	"testing"

	"github.com/mmcdole/rune/text"
)

func enablePerception(t *testing.T, engine *Engine) {
	t.Helper()
	if err := engine.DoString("enable", `rune.perception.enable()`); err != nil {
		t.Fatalf("rune.perception.enable() failed: %v", err)
	}
}

func TestPerceptionSnapshotEmptyInitially(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()
	enablePerception(t, engine)

	script := `
		local s = rune.perception.snapshot()
		assert(type(s.vitals) == "table" and next(s.vitals) == nil)
		assert(type(s.status) == "table" and next(s.status) == nil)
		assert(type(s.room) == "table" and next(s.room) == nil)
		assert(type(s.channels) == "table" and #s.channels == 0)
	`
	if err := engine.DoString("check", script); err != nil {
		t.Fatal(err)
	}
}

// TestPerceptionDisabledByDefault verifies the module registers no GMCP
// subscriptions or hook handlers until enable() is called - it must not
// impose side effects (wire traffic, durable-store writes) on a plain
// human user who never asked for the agent framework.
func TestPerceptionDisabledByDefault(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	if err := engine.DoString("check", `assert(rune.perception.is_enabled() == false)`); err != nil {
		t.Fatal(err)
	}

	engine.NotifyGMCPEnabled()
	for _, send := range host.GMCPSends {
		if send.Package == "Core.Supports.Set" {
			t.Fatalf("perception subscribed to GMCP before enable(): %v", send)
		}
	}

	engine.OnGMCP("Room.Info", `{"num":3001,"name":"Square","area":"Midgaard","exits":["north"]}`)
	if err := engine.DoString("check-inert", `
		local s = rune.perception.snapshot()
		assert(next(s.room) == nil, "Room.Info should be ignored before enable()")
	`); err != nil {
		t.Fatal(err)
	}
}

// TestPerceptionEnableDisableToggle verifies enable() wires GMCP
// subscriptions and disable() cleanly unwinds them (both idempotent).
func TestPerceptionEnableDisableToggle(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	enablePerception(t, engine)
	if err := engine.DoString("check-enabled", `
		assert(rune.perception.is_enabled() == true)
		rune.perception.enable() -- idempotent, must not error or double-register
	`); err != nil {
		t.Fatal(err)
	}

	engine.NotifyGMCPEnabled()
	found := false
	for _, send := range host.GMCPSends {
		if send.Package == "Core.Supports.Set" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected Core.Supports.Set after enable(), got %v", host.GMCPSends)
	}

	if err := engine.DoString("disable", `
		rune.perception.disable()
		assert(rune.perception.is_enabled() == false)
		rune.perception.disable() -- idempotent
	`); err != nil {
		t.Fatal(err)
	}

	engine.OnGMCP("Room.Info", `{"num":3001,"name":"Square","area":"Midgaard","exits":["north"]}`)
	if err := engine.DoString("check-disabled", `
		local s = rune.perception.snapshot()
		assert(next(s.room) == nil, "Room.Info should be ignored after disable()")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestPerceptionSnapshotReflectsVitals(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()
	enablePerception(t, engine)

	engine.OnGMCP("Char.Vitals", `{"hp":80,"maxhp":100,"mana":40,"maxmana":50,"move":90,"maxmove":100}`)

	script := `
		local s = rune.perception.snapshot()
		assert(s.vitals.hp == 80, "hp")
		assert(s.vitals.maxhp == 100, "maxhp")
		assert(s.vitals.mana == 40, "mana")
	`
	if err := engine.DoString("check", script); err != nil {
		t.Fatal(err)
	}
}

// TestPerceptionStatusReplacesNotMerges verifies a later Char.Status
// update that omits enemy/enemy_condition (combat ended) actually
// clears those fields in the snapshot, rather than leaving a stale
// "phantom" enemy behind from the earlier update - the GMCP spec emits
// a full snapshot of the package each time, not a delta.
func TestPerceptionStatusReplacesNotMerges(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()
	enablePerception(t, engine)

	engine.OnGMCP("Char.Status", `{"level":12,"gold":15,"position":"fighting","enemy":"a large kobold","enemy_condition":"has some wounds"}`)
	if err := engine.DoString("check-fighting", `
		local s = rune.perception.snapshot()
		assert(s.status.enemy == "a large kobold")
		assert(s.status.enemy_condition == "has some wounds")
	`); err != nil {
		t.Fatal(err)
	}

	engine.OnGMCP("Char.Status", `{"level":12,"gold":15,"position":"standing"}`)
	if err := engine.DoString("check-disengaged", `
		local s = rune.perception.snapshot()
		assert(s.status.position == "standing")
		assert(s.status.enemy == nil, "enemy should be cleared, got " .. tostring(s.status.enemy))
		assert(s.status.enemy_condition == nil, "enemy_condition should be cleared")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestPerceptionSnapshotReflectsRoom(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()
	enablePerception(t, engine)

	engine.OnGMCP("Room.Info", `{"num":3001,"name":"The Temple Square","area":"Midgaard","exits":["north","east"],"terrain":"city"}`)

	script := `
		local s = rune.perception.snapshot()
		assert(s.room.num == 3001)
		assert(s.room.name == "The Temple Square")
		assert(#s.room.exits == 2 and s.room.exits[1] == "north")

		-- defensive copy: mutating the returned snapshot must not
		-- corrupt the model's internal state
		table.insert(s.room.exits, "up")
		local s2 = rune.perception.snapshot()
		assert(#s2.room.exits == 2, "mutating a snapshot leaked into the model")
	`
	if err := engine.DoString("check", script); err != nil {
		t.Fatal(err)
	}
}

func TestPerceptionChannelsRingBuffer(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()
	enablePerception(t, engine)

	for i := 0; i < 25; i++ {
		engine.OnGMCP("Comm.Channel", `{"chan":"gossip","msg":"message `+strconv.Itoa(i)+`","player":"Bubba"}`)
	}

	script := `
		local s = rune.perception.snapshot()
		assert(#s.channels == 20, "expected cap of 20, got " .. #s.channels)
		assert(s.channels[1].msg == "message 5", "oldest should have been dropped, got " .. s.channels[1].msg)
		assert(s.channels[20].msg == "message 24", "newest should be last")
	`
	if err := engine.DoString("check", script); err != nil {
		t.Fatal(err)
	}
}

func TestPerceptionTranscriptRingBuffer(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()
	enablePerception(t, engine)

	if err := engine.DoString("setup", `
		seen = {}
		rune.hooks.on("output", function(line)
			table.insert(seen, line:clean())
		end)
	`); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 210; i++ {
		engine.OnOutput(text.NewLine("line " + strconv.Itoa(i)))
	}

	script := `
		assert(#seen == 210, "output hook should still see every line, got " .. #seen)
		local transcript = rune.perception.transcript()
		assert(#transcript == 200, "transcript should cap at 200, got " .. #transcript)
		assert(transcript[1] == "line 10", "oldest should have been dropped, got " .. transcript[1])
		assert(transcript[200] == "line 209", "newest should be last")
	`
	if err := engine.DoString("check", script); err != nil {
		t.Fatal(err)
	}
}

func TestPerceptionDisconnectResetsModel(t *testing.T) {
	engine, _, cleanup := setupTest(t)
	defer cleanup()
	enablePerception(t, engine)

	engine.OnGMCP("Char.Vitals", `{"hp":80,"maxhp":100}`)
	engine.OnGMCP("Room.Info", `{"num":3001,"name":"Square","area":"Midgaard","exits":["north"]}`)
	engine.OnGMCP("Comm.Channel", `{"chan":"gossip","msg":"hi","player":"Bubba"}`)
	engine.OnOutput(text.NewLine("some server text"))

	if err := engine.DoString("check-populated", `
		local s = rune.perception.snapshot()
		assert(s.vitals.hp == 80)
		assert(s.room.num == 3001)
		assert(#s.channels == 1)
	`); err != nil {
		t.Fatal(err)
	}

	if err := engine.DoString("disconnect", `rune.hooks.call("disconnected")`); err != nil {
		t.Fatal(err)
	}

	if err := engine.DoString("check-reset", `
		local s = rune.perception.snapshot()
		assert(next(s.vitals) == nil, "vitals should be reset")
		assert(next(s.room) == nil, "room should be reset")
		assert(#s.channels == 0, "channels should be reset")
	`); err != nil {
		t.Fatal(err)
	}
}

// TestPerceptionMapRecordsEdgeOnlyAfterMove verifies the durable map
// only learns a dir -> destination edge when a movement command
// immediately preceded a room change to a *different* room - never
// inferred from exits, never attributed across an unrelated
// (non-movement) input.
func TestPerceptionMapRecordsEdgeOnlyAfterMove(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()
	enablePerception(t, engine)

	// First room: nothing to attribute yet (no previous room).
	engine.OnGMCP("Room.Info", `{"num":3001,"name":"Square","area":"Midgaard","exits":["north","east"]}`)
	if err := engine.DoString("check-first", `
		local m = rune.perception.map()
		assert(m.rooms["3001"].name == "Square")
		assert(m.edges["3001"] == nil, "no edge should exist yet")
	`); err != nil {
		t.Fatal(err)
	}

	// A real move: north from 3001 lands in 3002.
	commitAndDispatchTestCommand(t, engine, host, "north")
	engine.OnGMCP("Room.Info", `{"num":3002,"name":"Temple","area":"Midgaard","exits":["south"]}`)
	if err := engine.DoString("check-edge", `
		local m = rune.perception.map()
		assert(m.rooms["3002"].name == "Temple")
		assert(m.edges["3001"].north == 3002, "edge not recorded: " .. tostring(m.edges["3001"] and m.edges["3001"].north))
	`); err != nil {
		t.Fatal(err)
	}

	// A non-movement command, then an unrelated room change (e.g. a
	// teleport): must NOT be recorded as a "south" edge from 3002.
	commitAndDispatchTestCommand(t, engine, host, "look")
	engine.OnGMCP("Room.Info", `{"num":3099,"name":"Elsewhere","area":"Midgaard","exits":["south"]}`)
	if err := engine.DoString("check-no-bogus-edge", `
		local m = rune.perception.map()
		assert(m.edges["3002"] == nil, "a non-movement input must not produce an edge")
	`); err != nil {
		t.Fatal(err)
	}
}
