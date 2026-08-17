package lua

import (
	"encoding/json"

	"github.com/mmcdole/rune/script"
)

// registerJSONFuncs registers rune._json.* primitives.
// The public rune.json API is defined in Lua (82_json.lua). Both
// directions reuse the shared script<->Go value bridge (script.DecodeTree
// and script.Tree) that rune.store and rune.gmcp already build on, so
// all three agree on what's encodable and on the empty-table
// convention.
func (e *Engine) registerJSONFuncs() {
	e.vm.RegisterModule("rune._json", map[string]script.GoFunc{
		// rune._json.encode(value) -> string, or nil + error message.
		"encode": func(c *script.Call) error {
			gv, err := script.DecodeTree(c.Arg(1), maxStoreDepth)
			if err != nil {
				c.Return(nil, err.Error())
				return nil
			}
			raw, err := json.Marshal(gv)
			if err != nil {
				c.Return(nil, err.Error())
				return nil
			}
			c.Return(string(raw))
			return nil
		},

		// rune._json.decode(text) -> value, or nil + error message. A
		// malformed text argument (wrong type) raises, per convention -
		// that's a programmer error, not a decode failure.
		"decode": func(c *script.Call) error {
			text := c.Str(1)

			var decoded any
			if err := json.Unmarshal([]byte(text), &decoded); err != nil {
				c.Return(nil, err.Error())
				return nil
			}
			c.Return(script.Tree{V: decoded})
			return nil
		},
	}, nil)
}
