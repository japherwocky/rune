package lua

import glua "github.com/yuin/gopher-lua"

// registerLogFuncs registers rune._log.* primitives.
// The public rune.log API and the hooks that decide what gets written
// are defined in Lua (60_log.lua). Go only owns the file handle, so an
// active log survives /reload.
func (e *Engine) registerLogFuncs() {
	log := e.L.NewTable()
	e.L.SetField(e.runeTable, "_log", log)

	// rune._log.start(path): open a log file (append, parents created).
	// Returns the resolved path, or nil + error message.
	e.L.SetField(log, "start", e.L.NewFunction(func(L *glua.LState) int {
		path := L.CheckString(1)
		resolved, err := e.host.LogStart(path)
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(glua.LString(resolved))
		return 1
	}))

	// rune._log.stop(): close the log. Returns true if one was open.
	e.L.SetField(log, "stop", e.L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LBool(e.host.LogStop()))
		return 1
	}))

	// rune._log.write(text): append one line. No-op when no log is open.
	e.L.SetField(log, "write", e.L.NewFunction(func(L *glua.LState) int {
		e.host.LogWrite(L.CheckString(1))
		return 0
	}))

	// rune._log.status(): returns the active log path, or nil.
	e.L.SetField(log, "status", e.L.NewFunction(func(L *glua.LState) int {
		if path, active := e.host.LogStatus(); active {
			L.Push(glua.LString(path))
		} else {
			L.Push(glua.LNil)
		}
		return 1
	}))

	// rune._log.read(max_lines): the last max_lines lines of the active
	// log as an array, oldest first. Returns nil + error message when no
	// log is open or the file cannot be read.
	e.L.SetField(log, "read", e.L.NewFunction(func(L *glua.LState) int {
		lines, err := e.host.LogRead(L.CheckInt(1))
		return pushLines(L, lines, err)
	}))

	// rune._log.search(pattern, max_results): lines of the active log
	// matching a Go-regexp pattern, oldest first, keeping the most
	// recent max_results matches. Returns nil + error message on a bad
	// pattern, no open log, or an unreadable file.
	e.L.SetField(log, "search", e.L.NewFunction(func(L *glua.LState) int {
		lines, err := e.host.LogSearch(L.CheckString(1), L.CheckInt(2))
		return pushLines(L, lines, err)
	}))
}

// pushLines returns lines as a Lua array, or nil + message on error -
// the recoverable-failure convention every other rune._* primitive
// follows (see registerCoreFuncs).
func pushLines(L *glua.LState, lines []string, err error) int {
	if err != nil {
		L.Push(glua.LNil)
		L.Push(glua.LString(err.Error()))
		return 2
	}
	t := L.NewTable()
	for _, line := range lines {
		t.Append(glua.LString(line))
	}
	L.Push(t)
	return 1
}
