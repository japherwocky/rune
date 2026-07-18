package lua

import (
	"time"

	"github.com/mmcdole/rune/input"
	"github.com/mmcdole/rune/ui"
)

// Host provides all services the Lua engine needs from the host application.
// In production, Session implements this interface.
type Host interface {
	// Network
	Send(data string) error
	Connect(addr string)
	Disconnect()

	// GMCP: send an out-of-band message ("Package.SubPackage" plus
	// optional raw JSON). Fails when disconnected or when the server
	// has not negotiated GMCP.
	GMCPSend(pkg, data string) error

	// UI
	Print(text string)
	PaneCreate(name string)
	PaneWrite(name, text string)
	PaneToggle(name string)
	PaneSetVisible(name string, visible bool)
	PaneClear(name string)
	ShowPicker(opts ui.ShowPickerMsg)
	GetInput() string
	SetInput(text string)
	SetInputSubmission(submission input.Submission)

	// Input primitives
	InputGetCursor() int
	InputSetCursor(pos int)
	OpenEditor(initial string) (string, bool)

	// Pane scrolling
	PaneScrollUp(name string, lines int)
	PaneScrollDown(name string, lines int)
	PaneScrollToTop(name string)
	PaneScrollToBottom(name string)

	// Timers
	TimerAfter(d time.Duration) int
	TimerEvery(d time.Duration) int
	TimerCancel(id int)
	TimerCancelAll()

	// System
	Quit()
	Reload()
	Load(path string)
	RefreshBars() // Force immediate bar refresh

	// Environment: read one process environment variable. ok is false
	// when unset. Host applies no policy - the allowlist restricting
	// which names scripts may read lives at the Lua boundary
	// (api_env.go), not here.
	Env(name string) (string, bool)

	// History
	GetHistory() []string
	GetHistoryEntries() []input.Submission
	AddToHistory(cmd string)

	// Session store: a small Go-owned string store that survives
	// script reloads (but not client exit). Lets Lua keep state
	// across the VM teardown of /reload. Durable state belongs in
	// the disk-backed Store* tier below.
	SessionSet(key, value string)
	SessionGet(key string) (string, bool)
	SessionDelete(key string)

	// Durable store: a Go-owned key→JSON store backed by
	// <config>/store.json; survives client exit. Values are raw JSON
	// (the lua package converts Lua values); writes are write-through
	// and atomic. Reads are served from memory.
	StoreSet(key, rawJSON string) error
	StoreGet(key string) (string, bool)
	StoreDelete(key string) error

	// Logging: Go owns the file handle so an active log survives
	// /reload and is flushed/closed on exit. WHAT gets logged (which
	// lines, stripping, headers) is Lua policy (lua/core/60_log.lua).
	LogStart(path string) (string, error) // opens (append); returns resolved path
	LogStop() bool                        // closes; reports whether a log was open
	LogWrite(text string)                 // appends one line; no-op when inactive
	LogStatus() (string, bool)            // active log path, if any

	// HTTP: perform req off the session goroutine and deliver the
	// outcome back on it via Engine.OnHTTPResult with the same id.
	// The id -> callback mapping is Lua state (lua/core/80_http.lua),
	// so pending callbacks die with the VM on /reload; a late result
	// for a stale id is dropped there.
	HTTPRequest(id int, req HTTPRequest)

	// LLM: perform an LLM chat request off the session goroutine and
	// deliver the outcome back on it via Engine.OnLLMResult with the
	// same id - the same async shape as HTTPRequest above. Unlike
	// HTTPRequest, the destination, auth header, and retry policy are
	// fixed by the implementation (see session/lua_llm.go), not
	// caller-supplied: req carries only the pre-encoded JSON body, so
	// the API key never has to pass through Lua to be attached.
	LLMRequest(id int, req LLMRequest)

	// State
	OnConfigChange()
}

// HTTPRequest describes one request handed to Host.HTTPRequest.
// Timeout <= 0 means the host's default.
type HTTPRequest struct {
	Method  string
	URL     string
	Body    string
	Headers map[string]string
	Timeout time.Duration
}

// HTTPResponse is a completed request's result, delivered back to Lua
// through Engine.OnHTTPResult. Also reused as-is for Engine.OnLLMResult
// (see LLMRequest) - an LLM response is a status/body/headers triple
// like any other HTTP result, so a second identical type would only
// duplicate this one.
type HTTPResponse struct {
	Status  int
	Body    string
	Headers map[string]string
}

// LLMRequest is one rune.llm.chat call's already-JSON-encoded body
// (built in Lua via rune.json.encode - see lua/core/86_llm.lua).
// Everything needed to actually reach the gateway - URL, auth header,
// API version, retry policy - is the implementation's responsibility,
// not the caller's; see session/lua_llm.go. Model is carried alongside
// Body (rather than requiring the implementation to parse it back out
// of the opaque JSON) purely so the "zen" provider can pick which of
// Zen's two endpoints to hit - Lua and Go each read it and apply the
// same model-family rule independently, the same "two sides agree
// without one telling the other" pattern RUNE_LLM_PROVIDER already
// uses (see 86_llm.lua's header comment).
type LLMRequest struct {
	Body  string
	Model string
}
