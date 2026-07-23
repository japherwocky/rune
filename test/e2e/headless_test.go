package e2e

// T10 (PLAN.md): a real Session driving a real network.TCPClient
// against a scripted MUD, with headless.UI (not the mock) as the only
// UI - proving the full agent-facing path (boot -> connect -> server
// output logged) works with no terminal at all, and that cancelling
// the context handed to headless.New - standing in for the SIGINT/
// SIGTERM signal.NotifyContext delivers in cmd/rune/main.go - shuts
// the whole session down cleanly.

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmcdole/rune/lua"
	"github.com/mmcdole/rune/network"
	"github.com/mmcdole/rune/session"
	"github.com/mmcdole/rune/ui/headless"
)

// syncBuffer serializes writes from the Session goroutine against
// reads from the test goroutine - bytes.Buffer alone isn't safe for that.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestHeadlessRunConnectsAndLogsServerOutput(t *testing.T) {
	mud := newFakeMUD(t)

	// Mirrors cmd/rune/main.go: one ctx handed to both headless.New and
	// Session.Run, so cancelling it stands in for the real SIGINT/SIGTERM path.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out syncBuffer
	h := headless.New(ctx, &out)

	s := session.New(network.NewTCPClient(), h, session.Config{
		CoreScripts: lua.CoreScripts,
		ConfigDir:   t.TempDir(),
		// Headless has no keyboard to type "/connect" at - the CLI
		// connect target is how a real `rune --headless host port` dials.
		ConnectTarget: mud.addr(),
	})

	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = s.Run(ctx)
		close(done)
	}()

	mud.accept()
	mud.writeLine("Welcome to the MUD")

	deadline := time.Now().Add(waitTimeout)
	for !strings.Contains(out.String(), "Welcome to the MUD") {
		if time.Now().After(deadline) {
			t.Fatalf("server output never reached the log, got %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Simulate SIGTERM: cancel the shared context and confirm the whole
	// session unwinds instead of hanging on the (nonexistent) terminal.
	cancel()
	select {
	case <-done:
		if runErr != nil {
			t.Fatalf("Session.Run returned error after context cancel: %v", runErr)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Session.Run did not shut down after context cancellation")
	}
}
