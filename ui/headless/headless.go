// Package headless implements ui.UI without a terminal (PLAN.md T10),
// so the agent can run unattended - long-running bots and swarms with
// no TUI to host them. Content (scrollback, prompts, pane writes) is
// logged to an io.Writer instead of rendered; the visual-only half of
// the interface (bars, layout, pickers, the input-line/editor
// primitives) has nothing to draw against and is a no-op, exactly as
// it already is for a plain human session that never touches those
// Lua APIs.
package headless

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/mmcdole/rune/input"
	"github.com/mmcdole/rune/ui"
)

// UI implements ui.UI headlessly. Run blocks on the context given to
// New rather than a terminal event loop - the same
// signal.NotifyContext ctx cmd/rune/main.go already derives for
// SIGINT/SIGTERM unblocks it directly, with Quit (Lua's /quit, same as
// the TUI) as the other way out. Input/Outbound start empty and stay
// that way until something drives them - PLAN.md T11's control surface.
type UI struct {
	ctx context.Context
	w   io.Writer
	mu  sync.Mutex // serializes writes to w

	inputChan chan input.Submission
	outbound  chan ui.UIEvent

	done     chan struct{}
	doneOnce sync.Once
}

var _ ui.UI = (*UI)(nil)

// New creates a headless UI logging content to w (os.Stdout in
// production). Run returns as soon as ctx is done or Quit is called,
// whichever comes first.
func New(ctx context.Context, w io.Writer) *UI {
	return &UI{
		ctx:       ctx,
		w:         w,
		inputChan: make(chan input.Submission, 64),
		outbound:  make(chan ui.UIEvent, 64),
		done:      make(chan struct{}),
	}
}

// Run blocks until ctx is cancelled or Quit is called.
func (h *UI) Run() error {
	select {
	case <-h.ctx.Done():
	case <-h.done:
	}
	h.doneOnce.Do(func() { close(h.done) })
	return nil
}

// Quit unblocks Run.
func (h *UI) Quit() {
	h.doneOnce.Do(func() { close(h.done) })
}

// Input has nothing feeding it until a control surface exists (T11).
func (h *UI) Input() <-chan input.Submission { return h.inputChan }

// Outbound has nothing feeding it - there is no keyboard/mouse to turn
// into WindowSizeChangedMsg, PickerSelectMsg, etc.
func (h *UI) Outbound() <-chan ui.UIEvent { return h.outbound }

func (h *UI) println(line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintln(h.w, line)
}

// Print logs a scrollback line (server output, Lua prints) as-is.
func (h *UI) Print(text string) { h.println(text) }

// Echo logs a local echo (already styled by the Lua "echo" hook) into
// the same stream as Print - headless has only the one transcript.
func (h *UI) Echo(text string) { h.println(text) }

// SetPrompt logs the server's partial-line prompt, tagged so it isn't
// mistaken for a committed scrollback line. A clear (empty text, sent
// once the prompt is committed or superseded) is dropped rather than
// logged as a bare tag.
func (h *UI) SetPrompt(text string) {
	if text == "" {
		return
	}
	h.println("[prompt] " + text)
}

// WritePane logs a named pane's content (e.g. the agent reasoning
// pane, lua/core/96_agent_ui.lua) tagged with its pane name - this is
// how the agent stays observable with no pane to look at.
func (h *UI) WritePane(name, text string) {
	h.println("[pane:" + name + "] " + text)
}

// The rest of the interface is visual-only - bars, layout, pickers, and
// an input line to move a cursor in or suspend for $EDITOR - and has
// nothing to do without a terminal.
func (h *UI) SetInput(string)                           {}
func (h *UI) SetInputSubmission(input.Submission)       {}
func (h *UI) UpdateBars(map[string]ui.BarContent)       {}
func (h *UI) UpdateBinds(map[string]bool)               {}
func (h *UI) UpdateLayout(top, bottom []ui.LayoutEntry) {}
func (h *UI) ShowPicker(ui.ShowPickerMsg)               {}
func (h *UI) CreatePane(string)                         {}
func (h *UI) TogglePane(string)                         {}
func (h *UI) SetPaneVisible(string, bool)               {}
func (h *UI) ClearPane(string)                          {}
func (h *UI) InputSetCursor(int)                        {}
func (h *UI) OpenEditor(string) (string, bool)          { return "", false }
func (h *UI) PaneScrollUp(string, int)                  {}
func (h *UI) PaneScrollDown(string, int)                {}
func (h *UI) PaneScrollToTop(string)                    {}
func (h *UI) PaneScrollToBottom(string)                 {}
