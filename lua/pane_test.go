package lua

import "testing"

// rune.pane.show/hide are idempotent setters over one Go primitive;
// what can silently break is the wrapper-to-flag mapping, so pin it.
func TestPaneShowHideReachHostWithVisibility(t *testing.T) {
	engine, host, cleanup := setupTest(t)
	defer cleanup()

	script := `
		rune.pane.create("chat")
		rune.pane.show("chat")
		rune.pane.hide("chat")
		rune.pane.toggle("chat")
	`
	if err := engine.DoString("test", script); err != nil {
		t.Fatalf("script failed: %v", err)
	}

	// Core boot also creates its own panes (e.g. 96_agent_ui.lua's
	// "agent" pane) - filter to "chat" so this test only pins the
	// show/hide/toggle wrapper mapping it actually cares about.
	var got []struct{ Op, Name, Data string }
	for _, call := range host.PaneCalls {
		if call.Name == "chat" {
			got = append(got, call)
		}
	}

	want := []struct{ Op, Name, Data string }{
		{"create", "chat", ""},
		{"set_visible", "chat", "true"},
		{"set_visible", "chat", "false"},
		{"toggle", "chat", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d \"chat\" pane calls, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("call %d: got %v, want %v", i, got[i], w)
		}
	}
}
