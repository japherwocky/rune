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
	llmAPIKeyEnv        = "OPENCODE_API_KEY"
	// 1 initial attempt + 3 retries. Only 429/529 trigger a retry
	// (see llmRetryableStatus); anything else - including a real
	// transport error - is delivered on the first try.
	llmMaxAttempts = 4
)

// llmURL is the OpenCode Zen endpoint (see PLAN.md T2/T4). A var, not
// a const, so session-level tests can redirect it at an
// httptest.Server instead of the real gateway.
var llmURL = "https://opencode.ai/zen/v1/messages"

// llmRetryBackoff computes the delay before retry attempt n (n >= 1).
// A var so tests can zero it out rather than actually sleeping.
var llmRetryBackoff = func(attempt int) time.Duration {
	return time.Duration(1<<uint(attempt)) * 250 * time.Millisecond
}

// LLMRequest implements lua.Host. Mirrors HTTPRequest's async shape
// exactly (see lua_http.go): the request runs in its own goroutine and
// the outcome comes back through the event loop as a single final
// AsyncResult, so the Lua callback executes on the session goroutine
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
	key, ok := s.Env(llmAPIKeyEnv)
	if !ok || key == "" {
		return nil, fmt.Errorf("%s is not set", llmAPIKeyEnv)
	}

	var resp *lua.HTTPResponse
	var err error
	for attempt := 0; attempt < llmMaxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(llmRetryBackoff(attempt))
		}
		resp, err = doLLMRequest(key, req)
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

func doLLMRequest(key string, req lua.LLMRequest) (*lua.HTTPResponse, error) {
	httpReq, err := http.NewRequest(http.MethodPost, llmURL, strings.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", key)
	httpReq.Header.Set("anthropic-version", llmAnthropicVersion)

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
