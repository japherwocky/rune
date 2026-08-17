package lua

import "github.com/mmcdole/rune/script"

// registerLogFuncs registers rune._log.* primitives.
// The public rune.log API and the hooks that decide what gets written
// are defined in Lua (60_log.lua). Go only owns the file handle, so an
// active log survives /reload.
func (e *Engine) registerLogFuncs() {
	e.vm.RegisterModule("rune._log", map[string]script.GoFunc{
		// rune._log.start(path): open a log file (append, parents created).
		// Returns the resolved path, or nil + error message.
		"start": func(c *script.Call) error {
			path := c.Str(1)
			resolved, err := e.host.LogStart(path)
			if err != nil {
				c.Return(nil, err.Error())
				return nil
			}
			c.Return(resolved)
			return nil
		},

		// rune._log.stop(): close the log. Returns true if one was open.
		"stop": func(c *script.Call) error {
			c.Return(e.host.LogStop())
			return nil
		},

		// rune._log.write(text): append one line. No-op when no log is open.
		"write": func(c *script.Call) error {
			e.host.LogWrite(c.Str(1))
			return nil
		},

		// rune._log.status(): returns the active log path, or nil.
		"status": func(c *script.Call) error {
			if path, active := e.host.LogStatus(); active {
				c.Return(path)
			} else {
				c.Return(nil)
			}
			return nil
		},

		// rune._log.read(max_lines): the last max_lines lines of the active
		// log as an array, oldest first. Returns nil + error message when no
		// log is open or the file cannot be read.
		"read": func(c *script.Call) error {
			lines, err := e.host.LogRead(c.Int(1))
			returnLines(c, lines, err)
			return nil
		},

		// rune._log.search(pattern, max_results): lines of the active log
		// matching a Go-regexp pattern, oldest first, keeping the most
		// recent max_results matches. Returns nil + error message on a bad
		// pattern, no open log, or an unreadable file.
		"search": func(c *script.Call) error {
			lines, err := e.host.LogSearch(c.Str(1), c.Int(2))
			returnLines(c, lines, err)
			return nil
		},
	}, nil)
}

// returnLines sets lines as a Lua array, or nil + message on error -
// the recoverable-failure convention every other rune._* primitive
// follows (see registerCoreFuncs).
func returnLines(c *script.Call, lines []string, err error) {
	if err != nil {
		c.Return(nil, err.Error())
		return
	}
	arr := make([]any, len(lines))
	for i, line := range lines {
		arr[i] = line
	}
	c.Return(script.Tree{V: arr})
}
