package session

// LLM transport tests (lua_llm.go, PLAN.md T8): the parts that only
// exist at this layer because they moved out of Lua - the destination
// URL, the auth header (built from the real process environment, see
// llmAPIKeyEnv), and the 429/529 retry/backoff policy. Request
// building/response parsing is already covered against MockHost in
// lua/llm_test.go; these tests instead run a real Session against a
// local httptest.Server, the same pattern as http_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withLLMTestServer points llmURL at srv for the calling test and
// restores the real Zen endpoint on cleanup. llmURL is package state,
// so callers must not run this under t.Parallel().
func withLLMTestServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := llmURL
	llmURL = srv.URL
	t.Cleanup(func() { llmURL = orig })
}

// withNoLLMBackoff zeroes the retry delay so retry tests run at full
// speed instead of actually sleeping.
func withNoLLMBackoff(t *testing.T) {
	t.Helper()
	orig := llmRetryBackoff
	llmRetryBackoff = func(attempt int) time.Duration { return 0 }
	t.Cleanup(func() { llmRetryBackoff = orig })
}

func TestLLMRoundTrip(t *testing.T) {
	s, _, _ := newTestSession(t)
	t.Setenv("OPENCODE_API_KEY", "test-key-123")

	var gotMethod, gotKey, gotVersion, gotContentType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotContentType = r.Header.Get("Content-Type")
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"hi there"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer srv.Close()
	withLLMTestServer(t, srv)

	err := s.engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			rune.session.set("text", tostring(reply and reply.text))
			rune.session.set("err", tostring(err))
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	awaitAsyncResult(t, s)

	if v, _ := s.SessionGet("text"); v != "hi there" {
		t.Errorf("text = %q, want %q", v, "hi there")
	}
	if v, _ := s.SessionGet("err"); v != "nil" {
		t.Errorf("err = %q, want nil", v)
	}

	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotKey != "test-key-123" {
		t.Errorf("x-api-key = %q, want test-key-123", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", gotVersion)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if !strings.Contains(gotBody, `"deepseek-v4-flash-free"`) {
		t.Errorf("request body missing model: %s", gotBody)
	}
}

// TestLLMMissingAPIKeyDeliversError proves the key check that used to
// live in 86_llm.lua (rune.env before T8) now happens here instead -
// and that the gateway is never contacted when it fails.
func TestLLMMissingAPIKeyDeliversError(t *testing.T) {
	s, _, _ := newTestSession(t)
	t.Setenv("OPENCODE_API_KEY", "") // explicitly unset, not just absent

	var hit int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hit, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withLLMTestServer(t, srv)

	err := s.engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			rune.session.set("err", tostring(err))
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	awaitAsyncResult(t, s)

	if v, _ := s.SessionGet("err"); !strings.Contains(v, "OPENCODE_API_KEY") {
		t.Errorf("err = %q, want it to mention OPENCODE_API_KEY", v)
	}
	if atomic.LoadInt32(&hit) != 0 {
		t.Error("the gateway must never be contacted without a key")
	}
}

func TestLLMRetriesOnRateLimitThenSucceeds(t *testing.T) {
	s, _, _ := newTestSession(t)
	t.Setenv("OPENCODE_API_KEY", "test-key")
	withNoLLMBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok now"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	withLLMTestServer(t, srv)

	err := s.engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			rune.session.set("text", tostring(reply and reply.text))
			rune.session.set("err", tostring(err))
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	awaitAsyncResult(t, s)

	if v, _ := s.SessionGet("text"); v != "ok now" {
		t.Errorf("text = %q, want %q (should succeed after retrying past the 429s)", v, "ok now")
	}
	if v, _ := s.SessionGet("err"); v != "nil" {
		t.Errorf("err = %q, want nil", v)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("server saw %d requests, want 3 (2 rate-limited + the eventual success)", got)
	}
}

func TestLLMRetriesExhaustedDeliversLastResponse(t *testing.T) {
	s, _, _ := newTestSession(t)
	t.Setenv("OPENCODE_API_KEY", "test-key")
	withNoLLMBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"still slow down"}}`))
	}))
	defer srv.Close()
	withLLMTestServer(t, srv)

	err := s.engine.DoString("test", `
		rune.llm.chat({
			model = "deepseek-v4-flash-free",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			rune.session.set("err", tostring(err))
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	awaitAsyncResult(t, s)

	// A persistent 429 is still a response, not a transport error - it
	// must reach the caller as the ordinary non-200 path (extracting
	// the nested error.message), exactly like a one-shot 429 would.
	if v, _ := s.SessionGet("err"); !strings.Contains(v, "429") || !strings.Contains(v, "still slow down") {
		t.Errorf("err = %q, want it to surface the final 429 body", v)
	}
	if got := atomic.LoadInt32(&hits); got != llmMaxAttempts {
		t.Errorf("server saw %d requests, want %d (every attempt exhausted)", got, llmMaxAttempts)
	}
}
