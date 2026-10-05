package session

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mmcdole/rune/lua"
	"github.com/mmcdole/rune/version"
)

// Zen asks third-party callers to identify themselves; Go's default
// "Go-http-client/1.1" reads as anonymous HTTP-library traffic and gets
// rejected. These pin the two headers that say who is calling.

func TestLLMRequestIdentifiesTheClient(t *testing.T) {
	s, _, _ := newTestSession(t)
	t.Setenv("OPENCODE_API_KEY", "test-key-123")

	var gotAgent, gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.Header.Get("User-Agent")
		gotSession = r.Header.Get("x-opencode-session")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()
	withLLMTestServer(t, srv)

	s.LLMRequest(1, lua.LLMRequest{Model: "big-pickle", Body: `{"model":"big-pickle"}`})
	awaitInternalEvent(t, s)

	if want := "rune/" + version.Number; gotAgent != want {
		t.Errorf("User-Agent = %q, want %q", gotAgent, want)
	}
	if strings.Contains(gotAgent, "Go-http-client") {
		t.Errorf("User-Agent still the Go default: %q", gotAgent)
	}
	if gotSession == "" {
		t.Error("x-opencode-session was not sent")
	}
}

// The id keys a conversation on Zen's side, so it has to be stable for
// the life of the Session - a fresh id per turn would present one
// long-running agent as a stream of strangers.
func TestLLMSessionIDIsStableAcrossRequests(t *testing.T) {
	s, _, _ := newTestSession(t)
	t.Setenv("OPENCODE_API_KEY", "test-key-123")

	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("x-opencode-session"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()
	withLLMTestServer(t, srv)

	for i := 1; i <= 2; i++ {
		s.LLMRequest(i, lua.LLMRequest{Model: "big-pickle", Body: `{"model":"big-pickle"}`})
		awaitInternalEvent(t, s)
	}

	if len(seen) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seen))
	}
	if seen[0] == "" || seen[0] != seen[1] {
		t.Errorf("session id should be stable and non-empty, got %q then %q", seen[0], seen[1])
	}
}

// The session header is Zen's bookkeeping. A local OpenAI-compatible
// runner has no use for it, and sending it would be noise - but the
// User-Agent is just honest self-identification, so it still goes.
func TestLLMOpenAIProviderGetsAgentButNoZenSession(t *testing.T) {
	s, _, _ := newTestSession(t)

	var gotAgent, gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.Header.Get("User-Agent")
		gotSession = r.Header.Get("x-opencode-session")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	t.Setenv(llmProviderEnv, "openai")
	t.Setenv(llmURLEnv, srv.URL)

	s.LLMRequest(1, lua.LLMRequest{Model: "local-model", Body: `{"model":"local-model"}`})
	awaitInternalEvent(t, s)

	if want := "rune/" + version.Number; gotAgent != want {
		t.Errorf("User-Agent = %q, want %q", gotAgent, want)
	}
	if gotSession != "" {
		t.Errorf("x-opencode-session should be Zen-only, got %q", gotSession)
	}
}
