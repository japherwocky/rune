package lua

import (
	"github.com/mmcdole/rune/script"
)

// registerLLMFuncs registers rune._llm.* primitives.
//
// Go performs the request (off the session goroutine) against the
// configured LLM gateway - including attaching the API key, which
// never passes through Lua - and delivers the outcome through
// Engine.OnLLMResult; the Lua llm module (86_llm.lua) owns the
// id -> callback mapping, the same split of responsibilities as
// rune._http/80_http.lua (api_http.go).
func (e *Engine) registerLLMFuncs() {
	e.vm.RegisterModule("rune._llm", map[string]script.GoFunc{
		// rune._llm.request(id, {body, model})
		"request": func(c *script.Call) error {
			id := c.Int(1)
			opts := c.Table(2)

			req := LLMRequest{
				Body:  opts.Field("body").Str(),
				Model: opts.Field("model").Str(),
			}
			if req.Body == "" {
				return c.Errorf("rune._llm.request: body is required")
			}

			e.host.LLMRequest(id, req)
			return nil
		},
	}, nil)
}

// OnLLMResult delivers a completed LLM request into Lua
// (rune.llm._deliver). Exactly one of resp/errMsg is set. Mirrors
// OnHTTPResult exactly, reusing HTTPResponse as the delivery shape
// (see the doc comment on that type).
func (e *Engine) OnLLMResult(id int, resp *HTTPResponse, errMsg string) {
	var respArg any
	var errArg any
	if errMsg != "" {
		errArg = errMsg
	} else if resp != nil {
		headers := make(map[string]any, len(resp.Headers))
		for k, v := range resp.Headers {
			headers[k] = v
		}
		respArg = script.Tree{V: map[string]any{
			"status":  float64(resp.Status),
			"body":    resp.Body,
			"headers": headers,
		}}
	}

	if err := e.guard(func() error {
		// found=false means the llm module is unavailable (core failed
		// to load); deliver silently becomes a no-op.
		_, _, err := e.vm.CallModule("rune.llm", "_deliver", 0, id, respArg, errArg)
		return err
	}); err != nil {
		e.reportError("llm callback", err)
	}
}
