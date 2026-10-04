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

// withLLMTestServer points the "zen" provider at srv for the calling
// test via RUNE_LLM_URL - the same override a real deployment would
// use, just aimed at a local httptest.Server instead of the real
// gateway. Also pins RUNE_LLM_PROVIDER=zen explicitly so a real
// RUNE_LLM_PROVIDER set in the developer's own shell (increasingly
// plausible now that T8b exists) can't leak into a test expecting the
// zen-default path - t.Setenv restores both on cleanup.
func withLLMTestServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv(llmProviderEnv, "zen")
	t.Setenv(llmURLEnv, srv.URL)
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
			model = "claude-haiku-4-5",
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

	awaitInternalEvent(t, s)

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
	if !strings.Contains(gotBody, `"claude-haiku-4-5"`) {
		t.Errorf("request body missing model: %s", gotBody)
	}
}

// TestZenResolvesEndpointByModelFamily proves the "zen" provider's
// default URL depends on req.model, not just the provider name (see
// zenModelUsesMessagesAPI). PLAN.md T4's original live probe never
// had a valid key to observe a single 200, so this split went
// unnoticed until every non-Claude model - the actual free/promotional
// models this provider exists for - failed live with a confusing
// "Input required: specify \"prompt\" or \"messages\"" (PLAN.md T4b).
func TestZenResolvesEndpointByModelFamily(t *testing.T) {
	s, _, _ := newTestSession(t)
	t.Setenv(llmProviderEnv, "zen")
	t.Setenv(llmURLEnv, "") // no override - exercise the real default

	for _, tc := range []struct {
		model   string
		wantURL string
	}{
		{"claude-haiku-4-5", llmZenMessagesURL},
		{"claude-opus-4-8", llmZenMessagesURL},
		{"deepseek-v4-flash-free", llmZenChatCompletionsURL},
		{"hy3-free", llmZenChatCompletionsURL},
		{"big-pickle", llmZenChatCompletionsURL},
		{"", llmZenChatCompletionsURL}, // no model given: default to the more common family rather than guessing Claude
	} {
		provider, err := resolveLLMProvider(s, tc.model)
		if err != nil {
			t.Fatalf("model %q: unexpected error: %v", tc.model, err)
		}
		if provider.url != tc.wantURL {
			t.Errorf("model %q: url = %q, want %q", tc.model, provider.url, tc.wantURL)
		}
	}
}

// TestZenURLOverrideBypassesModelRouting proves RUNE_LLM_URL, when
// set, wins outright regardless of model - the escape hatch stays a
// full override, not a second default to route around.
func TestZenURLOverrideBypassesModelRouting(t *testing.T) {
	s, _, _ := newTestSession(t)
	t.Setenv(llmProviderEnv, "zen")
	t.Setenv(llmURLEnv, "https://example.invalid/custom")

	for _, model := range []string{"claude-haiku-4-5", "deepseek-v4-flash-free"} {
		provider, err := resolveLLMProvider(s, model)
		if err != nil {
			t.Fatalf("model %q: unexpected error: %v", model, err)
		}
		if provider.url != "https://example.invalid/custom" {
			t.Errorf("model %q: url = %q, want the override to win", model, provider.url)
		}
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

	awaitInternalEvent(t, s)

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
			model = "claude-haiku-4-5",
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

	awaitInternalEvent(t, s)

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

	awaitInternalEvent(t, s)

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

// TestLLMOpenAIProviderOmitsAuthWhenNoKey proves the common local
// case - llama.cpp's llama-server needs no key by default - sends a
// clean request with no Authorization header at all, not an empty
// "Bearer ".
func TestLLMOpenAIProviderOmitsAuthWhenNoKey(t *testing.T) {
	s, _, _ := newTestSession(t)
	t.Setenv("RUNE_LLM_API_KEY", "")

	var authHeaderPresent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, authHeaderPresent = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer srv.Close()
	t.Setenv("RUNE_LLM_PROVIDER", "openai")
	t.Setenv(llmURLEnv, srv.URL)

	err := s.engine.DoString("test", `
		rune.llm.chat({
			model = "local",
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

	awaitInternalEvent(t, s)

	if v, _ := s.SessionGet("text"); v != "hi there" {
		t.Errorf("text = %q, want %q", v, "hi there")
	}
	if authHeaderPresent {
		t.Errorf("Authorization header present, want none (no key configured)")
	}
}

// TestLLMOpenAIProviderSendsBearerWhenKeySet proves a configured
// RUNE_LLM_API_KEY (e.g. a remote OpenAI-compatible host that does
// require one) is attached as a standard Bearer token.
func TestLLMOpenAIProviderSendsBearerWhenKeySet(t *testing.T) {
	s, _, _ := newTestSession(t)

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer srv.Close()
	t.Setenv("RUNE_LLM_PROVIDER", "openai")
	t.Setenv(llmURLEnv, srv.URL)
	t.Setenv("RUNE_LLM_API_KEY", "local-key-456")

	err := s.engine.DoString("test", `
		rune.llm.chat({
			model = "local",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err) end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	awaitInternalEvent(t, s)

	if gotAuth != "Bearer local-key-456" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer local-key-456")
	}
}

// TestLLMOpenAIProviderMissingURLDeliversError proves a local server's
// address is never guessed - RUNE_LLM_PROVIDER=openai without
// RUNE_LLM_URL fails fast with a clear error instead of silently
// falling back to Zen's endpoint or some hardcoded localhost:port
// that might be wrong.
func TestLLMOpenAIProviderMissingURLDeliversError(t *testing.T) {
	s, _, _ := newTestSession(t)
	t.Setenv("RUNE_LLM_PROVIDER", "openai")
	t.Setenv(llmURLEnv, "")

	err := s.engine.DoString("test", `
		rune.llm.chat({
			model = "local",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			rune.session.set("err", tostring(err))
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	awaitInternalEvent(t, s)

	if v, _ := s.SessionGet("err"); !strings.Contains(v, "RUNE_LLM_URL") {
		t.Errorf("err = %q, want it to mention RUNE_LLM_URL", v)
	}
}

// TestLLMUnknownProviderDeliversError proves a typo'd
// RUNE_LLM_PROVIDER fails with a clear message rather than silently
// behaving like "zen" (which would produce a confusing "OPENCODE_API_KEY
// is not set" for someone who never meant to talk to Zen at all).
func TestLLMUnknownProviderDeliversError(t *testing.T) {
	s, _, _ := newTestSession(t)
	t.Setenv("RUNE_LLM_PROVIDER", "openia")

	err := s.engine.DoString("test", `
		rune.llm.chat({
			model = "local",
			messages = { { role = "user", content = "look" } },
			max_tokens = 512,
		}, function(reply, err)
			rune.session.set("err", tostring(err))
		end)
	`)
	if err != nil {
		t.Fatal(err)
	}

	awaitInternalEvent(t, s)

	if v, _ := s.SessionGet("err"); !strings.Contains(v, "openia") {
		t.Errorf("err = %q, want it to mention the bad provider name", v)
	}
}
