package session

import (
	"context"

	"github.com/mmcdole/rune/lua"
)

// internalEvent is a typed message produced by Session-owned asynchronous or
// deferred work. It carries data, never code: Session applies every result on
// its event loop so only that goroutine mutates application and Lua state.
type internalEvent interface {
	isInternalEvent()
}

type connectFinished struct {
	connectionID uint64
	address      string
	err          error
}

func (connectFinished) isInternalEvent() {}

type httpFinished struct {
	luaGeneration uint64
	callbackID    int
	response      *lua.HTTPResponse
	errorText     string
}

func (httpFinished) isInternalEvent() {}

// llmFinished mirrors httpFinished: the LLM transport is the same
// off-loop request/deliver shape, with the key attached in the worker
// so it never passes through Lua (see lua_llm.go).
type llmFinished struct {
	luaGeneration uint64
	callbackID    int
	response      *lua.HTTPResponse
	errorText     string
}

func (llmFinished) isInternalEvent() {}

type reloadRequested struct{}

func (reloadRequested) isInternalEvent() {}

func (s *Session) handleInternalEvent(event internalEvent) {
	defer s.flushPresentation()
	switch event := event.(type) {
	case connectFinished:
		s.handleConnectFinished(event)
	case httpFinished:
		s.handleHTTPFinished(event)
	case llmFinished:
		s.handleLLMFinished(event)
	case reloadRequested:
		s.handleReloadRequested()
	}
}

// postInternalEvent returns false instead of stranding a producer after its
// Session lifetime has ended. The up-front Err check makes cancellation
// deterministic: without it, a cancelled context racing a writable queue may
// still enqueue the event.
func (s *Session) postInternalEvent(ctx context.Context, event internalEvent) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case s.internalEvents <- event:
		return true
	}
}
