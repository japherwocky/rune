package headless

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/rune/input"
)

const waitTimeout = 2 * time.Second

func TestPrintEchoLogVerbatim(t *testing.T) {
	var buf bytes.Buffer
	h := New(context.Background(), &buf)

	h.Print("a server line")
	h.Echo("a typed command")

	got := buf.String()
	if !strings.Contains(got, "a server line") || !strings.Contains(got, "a typed command") {
		t.Fatalf("expected both lines logged verbatim, got %q", got)
	}
}

func TestSetPromptTagsAndDropsEmpty(t *testing.T) {
	var buf bytes.Buffer
	h := New(context.Background(), &buf)

	h.SetPrompt("")
	if buf.Len() != 0 {
		t.Fatalf("expected an empty prompt (clear) to log nothing, got %q", buf.String())
	}

	h.SetPrompt("HP:100> ")
	if !strings.Contains(buf.String(), "[prompt] HP:100> ") {
		t.Fatalf("expected a tagged prompt line, got %q", buf.String())
	}
}

func TestWritePaneTagsWithName(t *testing.T) {
	var buf bytes.Buffer
	h := New(context.Background(), &buf)

	h.WritePane("agent", "thinking...")
	if !strings.Contains(buf.String(), "[pane:agent] thinking...") {
		t.Fatalf("expected a pane-tagged line, got %q", buf.String())
	}
}

func TestVisualOnlyMethodsAreNoOps(t *testing.T) {
	var buf bytes.Buffer
	h := New(context.Background(), &buf)

	// None of these have any content to log; this just proves they
	// don't panic and don't write anything.
	h.SetInput("x")
	h.SetInputSubmission(input.Command("x"))
	h.UpdateBars(nil)
	h.UpdateBinds(nil)
	h.UpdateLayout(nil, nil)
	h.CreatePane("p")
	h.TogglePane("p")
	h.SetPaneVisible("p", true)
	h.ClearPane("p")
	h.InputSetCursor(0)
	h.PaneScrollUp("p", 1)
	h.PaneScrollDown("p", 1)
	h.PaneScrollToTop("p")
	h.PaneScrollToBottom("p")

	if text, ok := h.OpenEditor("seed"); text != "" || ok {
		t.Fatalf("OpenEditor should report no editor available headlessly, got (%q, %v)", text, ok)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output from visual-only no-ops, got %q", buf.String())
	}
}

func TestRunBlocksUntilContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := New(ctx, &bytes.Buffer{})

	done := make(chan error, 1)
	go func() { done <- h.Run() }()

	select {
	case <-done:
		t.Fatal("Run returned before ctx was cancelled or Quit was called")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestRunBlocksUntilQuit(t *testing.T) {
	h := New(context.Background(), &bytes.Buffer{})

	done := make(chan error, 1)
	go func() { done <- h.Run() }()

	select {
	case <-done:
		t.Fatal("Run returned before Quit was called")
	case <-time.After(50 * time.Millisecond):
	}

	h.Quit()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return after Quit")
	}

	// Idempotent, and safe to call after Run has already returned.
	h.Quit()
}

func TestInputAndOutboundStartEmpty(t *testing.T) {
	h := New(context.Background(), &bytes.Buffer{})

	select {
	case v := <-h.Input():
		t.Fatalf("expected no input, got %v", v)
	default:
	}
	select {
	case v := <-h.Outbound():
		t.Fatalf("expected no outbound event, got %v", v)
	default:
	}
}
