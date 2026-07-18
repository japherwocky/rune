package session

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mmcdole/rune/event"
	"github.com/mmcdole/rune/lua"
)

const (
	llmDefaultTimeout   = 60 * time.Second
	llmMaxBodyBytes     = 5 << 20
	llmAnthropicVersion = "2023-06-01"

	// llmProviderEnv selects the backend: "zen" (default, unset also
	// means "zen") or "openai" - the OpenAI chat/completions wire
	// format spoken by llama.cpp's llama-server and other
	// self-hosted/local runners (Ollama, vLLM, LM Studio, ...), not
	// just OpenAI itself. Read independently by this file (URL/auth)
	// and by lua/core/86_llm.lua (which request/response JSON shape to
	// speak) - the two sides agree by reading the same env var rather
	// than one telling the other, matching how OPENCODE_API_KEY is
	// already Go-only and invisible to Lua (see T8).
	llmProviderEnv = "RUNE_LLM_PROVIDER"
	// llmURLEnv overrides the endpoint for either provider. Required
	// for "openai" (a local server's address is deployment-specific -
	// there is no safe default to guess, same reasoning as declining
	// to default req.model in 86_llm.lua or a per-model $ price in
	// 96_agent_ui.lua). Optional for "zen", which falls back to
	// llmZenURL.
	llmURLEnv = "RUNE_LLM_URL"

	llmZenAPIKeyEnv    = "OPENCODE_API_KEY"
	llmOpenAIAPIKeyEnv = "RUNE_LLM_API_KEY"

	// 1 initial attempt + 3 retries. Only 429/529 trigger a retry
	// (see llmRetryableStatus); anything else - including a real
	// transport error - is delivered on the first try.
	llmMaxAttempts = 4
)

// llmZenURL is the OpenCode Zen endpoint (see PLAN.md T2/T4) - the
// default target for the "zen" provider when RUNE_LLM_URL doesn't
// override it.
const llmZenURL = "https://opencode.ai/zen/v1/messages"

// llmRetryBackoff computes the delay before retry attempt n (n >= 1).
// A var so tests can zero it out rather than actually sleeping.
var llmRetryBackoff = func(attempt int) time.Duration {
	return time.Duration(1<<uint(attempt)) * 250 * time.Millisecond
}

// llmProvider carries what varies by backend: the endpoint, which env
// var (if any) supplies the key, whether a key is mandatory, and how
// to attach it to the request. The wire-format JSON body (Anthropic
// Messages-API-shaped vs. OpenAI chat/completions-shaped) is entirely
// lua/core/86_llm.lua's concern - this type never looks inside Body,
// consistent with LLMRequest's doc comment in lua/host.go ("Go only
// ever moves the already-encoded body as bytes").
type llmProvider struct {
	url         string
	apiKeyEnv   string
	keyRequired bool
	setAuth     func(req *http.Request, key string)
}

// resolveLLMProvider reads llmProviderEnv/llmURLEnv from the real
// process environment and returns the resolved backend. An
// unrecognized provider name or a missing required URL is a
// recoverable configuration error, delivered through the same
// _deliver(id, nil, err) path as any other LLM failure - not a raise,
// per the Go-primitive error convention (PLAN.md §2).
func resolveLLMProvider(s *Session) (llmProvider, error) {
	name, ok := s.Env(llmProviderEnv)
	if !ok || name == "" {
		name = "zen"
	}
	url, _ := s.Env(llmURLEnv)

	switch name {
	case "zen":
		if url == "" {
			url = llmZenURL
		}
		return llmProvider{
			url:         url,
			apiKeyEnv:   llmZenAPIKeyEnv,
			keyRequired: true,
			setAuth: func(req *http.Request, key string) {
				req.Header.Set("x-api-key", key)
				req.Header.Set("anthropic-version", llmAnthropicVersion)
			},
		}, nil
	case "openai":
		if url == "" {
			return llmProvider{}, fmt.Errorf("%s is required when %s=openai", llmURLEnv, llmProviderEnv)
		}
		return llmProvider{
			url:         url,
			apiKeyEnv:   llmOpenAIAPIKeyEnv,
			keyRequired: false,
			setAuth: func(req *http.Request, key string) {
				// llama.cpp and most local runners need no key at
				// all by default - only attach one if configured, so
				// the common no-auth local case sends a clean request
				// rather than an empty "Authorization: Bearer ".
				if key != "" {
					req.Header.Set("Authorization", "Bearer "+key)
				}
			},
		}, nil
	default:
		return llmProvider{}, fmt.Errorf("%s: unknown provider %q (want \"zen\" or \"openai\")", llmProviderEnv, name)
	}
}

// LLMRequest implements lua.Host. Mirrors HTTPRequest's async shape
// exactly (see lua_http.go): the request runs in its own goroutine and
// the outcome comes back through the event loop as a single final
// AsyncResult, so the Lua callback executes on the Session goroutine
// under the watchdog like every other callback (see PLAN.md T8 - a
// non-streaming start behind the same id-based delivery, streaming
// left for later behind this same primitive).
//
// Unlike plain rune.http, the destination and auth are fixed here
// rather than caller-supplied: the API key is read straight from the
// process environment and attached in this goroutine, so it never has
// to pass through Lua to reach the request. A 429/529 response is
// retried with backoff in-goroutine before delivery, rather than
// surfacing a still-recoverable failure straight to the agent.
func (s *Session) LLMRequest(id int, req lua.LLMRequest) {
	go func() {
		resp, err := doLLMRequestWithRetry(s, req)
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		s.events <- event.Event{
			Type: event.AsyncResult,
			Payload: event.Callback(func() {
				s.engine.OnLLMResult(id, resp, errMsg)
			}),
		}
	}()
}

func doLLMRequestWithRetry(s *Session, req lua.LLMRequest) (*lua.HTTPResponse, error) {
	provider, err := resolveLLMProvider(s)
	if err != nil {
		return nil, err
	}

	key, _ := s.Env(provider.apiKeyEnv)
	if provider.keyRequired && key == "" {
		return nil, fmt.Errorf("%s is not set", provider.apiKeyEnv)
	}

	var resp *lua.HTTPResponse
	for attempt := 0; attempt < llmMaxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(llmRetryBackoff(attempt))
		}
		resp, err = doLLMRequest(provider, key, req)
		if err != nil {
			return nil, err
		}
		if !llmRetryableStatus(resp.Status) {
			return resp, nil
		}
	}
	// Retries exhausted: hand back the last (still-retryable)
	// response as-is rather than an error, so the caller sees the
	// real status/body (e.g. a persistent 429) exactly like any other
	// non-2xx - not a transport failure.
	return resp, nil
}

// llmRetryableStatus: 429 (rate limited) and 529 (Anthropic-shaped
// "overloaded") are worth a backoff-and-retry; any other status - a
// real client/server error or a 2xx - is returned immediately.
func llmRetryableStatus(status int) bool {
	return status == 429 || status == 529
}

func doLLMRequest(provider llmProvider, key string, req lua.LLMRequest) (*lua.HTTPResponse, error) {
	httpReq, err := http.NewRequest(http.MethodPost, provider.url, strings.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	provider.setAuth(httpReq, key)

	client := &http.Client{Timeout: llmDefaultTimeout}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(httpResp.Body, llmMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > llmMaxBodyBytes {
		return nil, fmt.Errorf("response body exceeds the %d MB cap", llmMaxBodyBytes>>20)
	}

	headers := make(map[string]string, len(httpResp.Header))
	for k := range httpResp.Header {
		headers[k] = httpResp.Header.Get(k)
	}
	return &lua.HTTPResponse{
		Status:  httpResp.StatusCode,
		Body:    string(data),
		Headers: headers,
	}, nil
}
