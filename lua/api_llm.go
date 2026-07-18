package lua

import (
	glua "github.com/yuin/gopher-lua"
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
	llmTable := e.L.NewTable()
	e.L.SetField(e.runeTable, "_llm", llmTable)

	// rune._llm.request(id, {body, model})
	e.L.SetField(llmTable, "request", e.L.NewFunction(func(L *glua.LState) int {
		id := int(L.CheckNumber(1))
		opts := L.CheckTable(2)

		req := LLMRequest{
			Body:  glua.LVAsString(opts.RawGetString("body")),
			Model: glua.LVAsString(opts.RawGetString("model")),
		}
		if req.Body == "" {
			L.RaiseError("rune._llm.request: body is required")
		}

		e.host.LLMRequest(id, req)
		return 0
	}))
}

// OnLLMResult delivers a completed LLM request into Lua
// (rune.llm._deliver). Exactly one of resp/errMsg is set. Mirrors
// OnHTTPResult exactly, reusing HTTPResponse as the delivery shape
// (see the doc comment on that type).
func (e *Engine) OnLLMResult(id int, resp *HTTPResponse, errMsg string) {
	if e.L == nil {
		return
	}
	deliver, ok := e.getRuneFunc("llm", "_deliver")
	if !ok {
		return // llm module unavailable (core failed to load)
	}

	respVal := glua.LValue(glua.LNil)
	errVal := glua.LValue(glua.LNil)
	if errMsg != "" {
		errVal = glua.LString(errMsg)
	} else if resp != nil {
		t := e.L.NewTable()
		t.RawSetString("status", glua.LNumber(resp.Status))
		t.RawSetString("body", glua.LString(resp.Body))
		headers := e.L.NewTable()
		for k, v := range resp.Headers {
			headers.RawSetString(k, glua.LString(v))
		}
		t.RawSetString("headers", headers)
		respVal = t
	}

	if err := e.guard(func() error {
		return e.L.CallByParam(glua.P{
			Fn:      deliver,
			NRet:    0,
			Protect: true,
		}, glua.LNumber(id), respVal, errVal)
	}); err != nil {
		e.reportError("llm callback", err)
	}
}
