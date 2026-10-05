package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mmcdole/rune/lua"
	"github.com/mmcdole/rune/version"
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

// Zen fronts two differently-shaped endpoints, not one (see PLAN.md
// T4b): Claude models live behind the Anthropic-Messages-API-shaped
// /v1/messages, everything else (the free/promotional models this
// provider exists for - deepseek, glm, hy3, mimo, ...) behind the
// OpenAI-chat-completions-shaped /v1/chat/completions. Sending a
// non-Claude model to /v1/messages doesn't 404 - it reaches a real
// backend that then fails opaquely ("Input required: specify
// \"prompt\" or \"messages\"" or "Upstream request failed"), which is
// what made this take a live-fire investigation to find rather than
// showing up as an obvious error. zenModelUsesMessagesAPI below is the
// default target for the "zen" provider when RUNE_LLM_URL doesn't
// override it.
const (
	llmZenMessagesURL        = "https://opencode.ai/zen/v1/messages"
	llmZenChatCompletionsURL = "https://opencode.ai/zen/v1/chat/completions"
)

// zenModelUsesMessagesAPI reports whether model belongs to Zen's
// Claude family (and so must go to /v1/messages in the Anthropic
// shape) as opposed to every other model in the catalog (which goes
// to /v1/chat/completions in the OpenAI shape). Read independently by
// this file (URL selection) and by lua/core/86_llm.lua (wire-format
// selection) from the same req.model - the two sides agree by
// applying the same rule to the same value rather than one telling
// the other, matching how RUNE_LLM_PROVIDER already works. Verified
// live 2026-07-18: a Claude model 200s at /v1/messages and 401s at
// /v1/chat/completions; a non-Claude model 200s at
// /v1/chat/completions and 400s at /v1/messages - every Zen Claude id
// in the live catalog is "claude-*" (claude-sonnet-5, claude-opus-4-8,
// claude-haiku-4-5, ...) and no non-Claude id is, so the prefix is a
// reliable, low-maintenance split rather than a hardcoded model list
// that would need updating every time Zen's rotating catalog changes.
func zenModelUsesMessagesAPI(model string) bool {
	return strings.HasPrefix(model, "claude")
}

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
	name        string
	sessionID   string
	url         string
	apiKeyEnv   string
	keyRequired bool
	setAuth     func(req *http.Request, key string)
}

// resolveLLMProvider reads llmProviderEnv/llmURLEnv from the real
// process environment and returns the resolved backend. model (from
// the same LLMRequest the caller is about to send) picks which of
// Zen's two endpoints to default to - see zenModelUsesMessagesAPI. An
// unrecognized provider name or a missing required URL is a
// recoverable configuration error, delivered through the same
// _deliver(id, nil, err) path as any other LLM failure - not a raise,
// per the Go-primitive error convention (PLAN.md §2).
// llmSessionID returns this Session's conversation id for Zen, minting
// it on first use. Zen asks third-party callers to carry a session id
// alongside a real User-Agent; one id for the whole Session is the
// honest shape, since that is the conversation. Random rather than
// derived from anything local - it is an opaque correlation handle, and
// nothing about the user should be inferable from it. A failed read
// from crypto/rand degrades to an empty header rather than failing the
// request: identification is Zen's bookkeeping, not ours to enforce.
func (s *Session) llmSessionID() string {
	if s.llmSession == "" {
		var buf [16]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return ""
		}
		s.llmSession = hex.EncodeToString(buf[:])
	}
	return s.llmSession
}

func resolveLLMProvider(s *Session, model string) (llmProvider, error) {
	name, ok := s.Env(llmProviderEnv)
	if !ok || name == "" {
		name = "zen"
	}
	url, _ := s.Env(llmURLEnv)

	switch name {
	case "zen":
		if url == "" {
			if zenModelUsesMessagesAPI(model) {
				url = llmZenMessagesURL
			} else {
				url = llmZenChatCompletionsURL
			}
		}
		return llmProvider{
			name:        "zen",
			sessionID:   s.llmSessionID(),
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
			name:        "openai",
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
// the outcome comes back through the async-result channel as a single
// final continuation, so the Lua callback executes on the Session
// goroutine under the watchdog like every other callback (see PLAN.md T8 - a
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
	luaGeneration := s.luaGeneration
	backgroundCtx := s.backgroundCtx
	s.backgroundWork.Go(func() {
		resp, err := doLLMRequestWithRetry(backgroundCtx, s, req)
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		s.postInternalEvent(backgroundCtx, llmFinished{
			luaGeneration: luaGeneration,
			callbackID:    id,
			response:      resp,
			errorText:     errMsg,
		})
	})
}

// handleLLMFinished drops a result whose VM is gone: a /reload between
// request and delivery means the callback that was waiting for this id
// no longer exists, and the id could since have been reissued.
func (s *Session) handleLLMFinished(event llmFinished) {
	if event.luaGeneration != s.luaGeneration {
		return
	}
	s.engine.OnLLMResult(event.callbackID, event.response, event.errorText)
}

func doLLMRequestWithRetry(ctx context.Context, s *Session, req lua.LLMRequest) (*lua.HTTPResponse, error) {
	provider, err := resolveLLMProvider(s, req.Model)
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
			// Abandon a pending retry when the session is going away,
			// rather than holding backgroundWork open in a sleep.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(llmRetryBackoff(attempt)):
			}
		}
		resp, err = doLLMRequest(ctx, provider, key, req)
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

func doLLMRequest(ctx context.Context, provider llmProvider, key string, req lua.LLMRequest) (*lua.HTTPResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.url, strings.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Identify the client. Go's default "Go-http-client/1.1" reads as
	// anonymous HTTP-library traffic, which Zen rejects outright; its
	// docs ask third-party callers to say who they are. Harmless
	// against a local runner, so it is set unconditionally.
	httpReq.Header.Set("User-Agent", "rune/"+version.Number)
	if provider.name == "zen" {
		// One id for the life of the Session, not per request: it keys a
		// conversation on Zen's side, and a fresh id per turn would
		// present a long-running agent as a stream of strangers.
		httpReq.Header.Set("x-opencode-session", provider.sessionID)
	}
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
