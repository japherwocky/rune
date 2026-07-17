# PLAN.md — LLM MUD-bot framework on Rune

Status: design accepted, not yet implemented. This document is the implementation
plan. It is written to be **self-contained**: an agent picking up any single task
below should be able to execute it from this file plus the referenced source, without
the design conversation that produced it.

## 1. Goal

Turn Rune (a MUD client: Go kernel, Lua user-space) into a framework for an
**LLM-powered MUD bot** that plays [botmud](https://github.com/japherwocky/botmud) (a
ROM 2.4b6 derivative we also own).

Two run modes, same agent logic underneath:

- **Live mode** — the existing TUI. A human shares control with the agent, watches its
  reasoning in a pane, and can override by typing. This is the primary dev/debug surface.
- **Headless mode** — no terminal; the agent runs autonomously. For long-running bots
  and swarms.

Design tenets settled in design review:

- **LLM sets intent; the scripting layer executes tactics.** The LLM does not drive
  every keystroke. Repetitive play (especially combat) is handled by fast deterministic
  triggers/aliases the agent itself *authors*. The LLM re-engages only for novel
  situations or when a reflex it installed stops working.
- **Single agent per process.** Multiple bots may collaborate, but only through
  **in-game channels** (gossip/tell/say) — never shared memory or IPC. The game is the
  message bus. A swarm is N independent headless bots that talk to each other in-game.
- **No cheating.** Perception comes over the wire via GMCP, exposing only what a human
  player already sees on screen. See §4 and botmud#20.

## 2. Non-negotiable conventions (read before writing code)

Canonical: `AGENTS.md`, `docs/architecture.md`, `docs/testing.md`. Summary of the rules
that will bite you if ignored:

- **Go/Lua boundary.** Go registers only `rune._*` primitives (internal). Every public
  `rune.*` name is defined in Lua, in `lua/core/NN_*.lua` (loaded in numeric order).
  New public API for this project should be Lua modules on top of thin Go primitives.
  (Note: this fork removed the docs-coverage test, so new public `rune.*` functions no
  longer require a website page — but keep header comments per the existing core files.)
- **Single Session goroutine.** One goroutine (`session/session.go`, `processEvents`)
  owns the Lua VM and processes every event sequentially. **Never block it on I/O.**
- **The async pattern (memorize this — it is the template for the LLM call).** Slow I/O
  runs in its own goroutine and returns its result *back into* the Session as an
  `event.AsyncResult` carrying an `event.Callback` closure, which re-enters Lua on the
  Session goroutine under the watchdog. The canonical example is HTTP:
  - `session/lua_http.go` — `Session.HTTPRequest` spawns a goroutine, does the blocking
    request, posts `event.Event{Type: event.AsyncResult, Payload: event.Callback(...)}`.
  - `lua/api_http.go` — registers `rune._http.request`; `Engine.OnHTTPResult` calls back
    into Lua `rune.http._deliver(id, resp, err)`.
  - `lua/core/80_http.lua` — owns the `id -> callback` map (`pending`), so pending
    callbacks die with the VM on `/reload`; a late result for a stale id is dropped.
  **Every LLM interaction MUST follow this shape. A synchronous LLM call would freeze the
  whole client and trip the watchdog.** The agent is therefore a callback-driven state
  machine, not a straight-line script.
- **Watchdog (5s).** Every Go→Lua entry runs under `Engine.guard` with a deadline
  (`Engine.CallTimeout`, default 5s). Keep per-callback Lua work bounded. Building a
  prompt from a huge transcript or encoding a big map on the Session goroutine can trip
  it. Heavy CPU work belongs in Go, off-goroutine, delivered back as an event. (The LLM
  network call is fine — it is async by construction.)
- **Error convention.** Go primitives return `nil, err` for recoverable failures; raise
  only on programmer error (wrong arg types).
- **Failure quarantine.** Hooks/triggers/aliases/timers/bars/commands run under
  `rune.guarded_call(label, data, fn, ...)`, which disables an entry after 3 consecutive
  failures. Agent-authored triggers get this for free — and it is load-bearing (§ T9).
- **gopher-lua quirks.** See `80_http.lua`: `opts, callback = nil, opts` clears `opts`
  before the RHS is read. Do sequential reassignment. Watch `RawGetString` typing.
- **Tests first.** `docs/testing.md` is the decision guide. Test at the lowest layer that
  can express the failure: Go unit (table-driven) → Lua-against-MockHost
  (`lua/mock_host_test.go`) → session synchronous → e2e scenario JSON
  (`test/e2e/scenarios/`) → e2e imperative Go. For a reported bug, add a failing
  `test/e2e/scenarios/regressions/` scenario first. The e2e harness can inject GMCP
  (see `test/e2e/scenarios/regressions/30-aardwolf-gmcp-color.json`).

Build/test:

```bash
go build ./cmd/rune/    # build
go test ./...           # tests
```

## 3. Architecture (where each task fits)

```
                         ┌─────────────────────────── Lua user-space ───────────────────────────┐
  MUD (botmud)           │                                                                        │
   │  GMCP + text        │   worldmodel ──▶ agent core (state machine) ──▶ tool layer ──▶ rune.send│
   ▼                     │   (perception)     idle→observe→wait→act        send_command            │
  network ──▶ Session ───┼──▶ rune.gmcp.on / hooks("output","prompt")      speak(channel)          │
  (Go kernel, 1 goroutine)│                        │                       create_trigger ────────┐│
   ▲                     │                    llm client ──(async)──▶ Anthropic Messages API       ││
   │  rune.send          │   governance: budget / rate-limit / oscillation / quarantine→re-plan   ││
   │                     │   observability: reasoning pane, state bar, turn log                    ││
   └─────────────────────┴────────────────────────────────────────────────────────────────────────┘│
  UI seam (ui.UI): BubbleTeaUI (live)  |  HeadlessUI (headless)  ◀── agent logic is UI-agnostic ────┘
```

- **Perception** — consume GMCP into a live world-model table; keep a rolling text
  transcript from the `output` hook. Uses existing `rune.gmcp`/`hooks`.
- **Transport** — Phase 1 uses existing `rune.http.post` to the Messages API; Phase 2
  hardens into a Go `rune._llm` primitive (streaming, key-in-Go, retries, usage).
- **Cognition** — the callback-driven state machine and think-cadence policy.
- **Action** — tools: `send_command`, `speak`, and the reflex-programming tools
  (`create_trigger` etc.) built on `rune.trigger`.
- **Memory** — working context + rolling LLM summary + durable state in `rune.store`.
- **Governance** — budget/rate/oscillation limits; quarantine→re-plan wiring.
- **Dual-mode** — a second `ui.UI` implementation; the Session is unchanged.
- **Observability** — `rune.pane`/`rune.bars`/`rune.log`.

## 4. Fairness principle (client side must not work around it)

Perception is GMCP-only and mirrors what a human with a capable client sees. Do **not**
add code that infers or requests information a human couldn't get (exact enemy hp, exit
destinations before walking, invisible entities). The server enforces this
(botmud#20); the client must not reintroduce cheating by other means (e.g. scraping
hidden state). Map-building is allowed — accumulate dir→destination by *walking*, like a
human's mapper client.

## 5. Tasks

Dependency-ordered. `[Go]` = kernel primitive, `[Lua]` = user-space module,
`[Go+cmd]` = kernel + entry point. Each task is meant to be a self-contained unit of
work with its own tests and commit.

### Phase 1 — one bot fighting in the live TUI

The two tasks that unblock everything and do **not** depend on botmud#20 are T1 and T3
(T3 is testable now with GMCP fixtures). Start there.

#### T1 `[Go]` — `rune.json` primitive
- **Why:** Lua must build Messages API request bodies and parse responses (HTTP body is
  a string; `rune.store` already handles tables, but the wire needs JSON).
- **Create:** `lua/api_json.go` registering `rune._json.encode(value) -> string` and
  `rune._json.decode(string) -> value, err`. Add `lua/core/NN_json.lua` (pick a free
  number, e.g. `82_json.lua`) exposing `rune.json.encode` / `rune.json.decode`.
- **Mirror:** the registration style in `lua/api_http.go`; reuse the JSON encode/decode
  the GMCP bridge already uses in `lua/engine.go` (search `json.Unmarshal` there).
- **Decode convention:** return `nil, errmsg` on invalid JSON (recoverable). Map JSON
  objects→Lua tables (string keys), arrays→sequence tables. Decide and document the
  empty-array vs empty-object ambiguity.
- **Tests:** table-driven Go round-trip in `lua/` (encode∘decode identity for
  object/array/string/number/bool/null/nested; invalid JSON → error). A Lua-against-mock
  test that `rune.json` is callable.
- **Done when:** `rune.json.encode`/`decode` round-trip in Lua; invalid input returns
  `nil, err` not a raise.

#### T2 `[Go]` — `rune.env` primitive (API-key access)
- **Why:** the LLM client needs the API key; no env accessor exists today (confirmed).
  Keeps the key out of scripts-in-git.
- **Create:** `lua/api_env.go` registering `rune._env.get(name) -> string|nil`, and a
  thin `rune.env(name)` in a core file (or fold into `82_json.lua`'s neighbor). Consider
  an allowlist (only expose `ANTHROPIC_API_KEY` and a small set) so scripts can't read
  arbitrary environment.
- **Tests:** Go unit via the Host/mock; allowlist enforced.
- **Done when:** `rune.env("ANTHROPIC_API_KEY")` returns the value in a real run and
  `nil` for non-allowlisted names.

#### T3 `[Lua]` — world-model module (perception)
- **Why:** the agent's sensors. **Buildable/testable now** with GMCP fixtures, before
  botmud#20 ships.
- **Create:** `lua/core/NN_worldmodel.lua` (or ship as a user script first, then
  graduate to core). Public surface e.g. `rune.world.snapshot()` returning a compact
  table `{ vitals, status, room, channels = {recent...} }`.
- **Wire:**
  - `rune.gmcp.subscribe("Char", 1)`, `rune.gmcp.subscribe("Room", 1)`,
    `rune.gmcp.subscribe("Comm", 1)` (triggers `Core.Supports.Set`).
  - `rune.gmcp.on("Char.Vitals", ...)`, `"Char.Status"`, `"Room.Info"`,
    `"Comm.Channel"` — update the model. Handler signature is `function(data, package)`
    where `data` is the decoded value (see `lua/core/70_gmcp.lua`).
  - `rune.hooks.on("output", ...)` — append cleaned lines to a bounded rolling
    transcript (ring buffer, cap ~200 lines). Do not gag.
  - `rune.hooks.on("disconnected", ...)` — reset volatile model.
- **Durable map:** on `Room.Info`, record the room by `num` in `rune.store` and, when
  you know the previous room + the direction moved, record `prev.num --dir--> num`
  (learned by walking; see §4). Keep this out of the hot path if large.
- **Tests:** e2e scenario JSON that injects `Char.Vitals`/`Room.Info`/`Comm.Channel`
  GMCP messages and asserts `rune.world.snapshot()` reflects them (model on
  `test/e2e/scenarios/regressions/30-aardwolf-gmcp-color.json`). Lua-against-mock for
  the transcript ring buffer.
- **Done when:** snapshot reflects injected GMCP; transcript stays bounded; map records
  dir→dest only after a move.

#### T4 `[Lua]` — LLM client (Phase 1 transport via rune.http)
- **Why:** talk to the Messages API without new Go yet.
- **Create:** `lua/core/NN_llm.lua` exposing e.g.
  `rune.llm.chat({ system, messages, tools, max_tokens }, function(reply, err) ... end)`.
- **Implement:** build the request table, `rune.json.encode` it, `rune.http.post(url,
  body, { headers = { ["x-api-key"] = rune.env("ANTHROPIC_API_KEY"),
  ["anthropic-version"] = "...", ["content-type"] = "application/json" } }, cb)`. In
  `cb`, `rune.json.decode(resp.body)`, surface `stop_reason`, `content` (text +
  `tool_use` blocks), and `usage`. Own an `id -> callback` map only if you multiplex;
  otherwise rely on single-flight (T5).
- **Reference:** load the `claude-api` skill for exact endpoint, headers,
  `anthropic-version`, tool-use request/response shape, model id, and prompt-caching
  params. **Do not hardcode these from memory.**
- **Tests:** Lua-against-mock where the mock Host returns a canned Messages API JSON body
  for an HTTP request; assert `rune.llm.chat` parses text + tool_use + usage. Cover the
  error path (HTTP err, non-200, malformed JSON).
- **Done when:** a canned response parses into a normalized reply table; errors surface
  as `err`.

#### T5 `[Lua]` — agent core (state machine + cadence)
- **Depends:** T3, T4.
- **Create:** `lua/core/NN_agent.lua`. A single-flight state machine:
  `idle → observing → waiting_llm → acting → observing`.
- **Cadence (combine):**
  - wake flag set by salient events (register triggers/gmcp handlers/hooks for
    combat-start, low-hp, a tell arriving);
  - `rune.hooks.on("prompt", ...)` as the natural "my turn" signal;
  - a debounce `rune.timer` (~1–3s) that fires a think if one is pending and none is in
    flight;
  - **single-flight:** never two LLM calls outstanding; events during a think coalesce
    into "think again on return."
- **Turn:** gather `rune.world.snapshot()` + recent transcript + goal + tool defs →
  `rune.llm.chat` → on `tool_use`, dispatch to T6 tools, capture results, feed
  `tool_result` back and continue the turn; on text/end, update goal and idle.
- **Reload safety:** keep in-flight state and pending callbacks in Lua (like
  `80_http.lua`) so `/reload` abandons them cleanly. Durable memory lives in
  `rune.store`.
- **Tests:** session-synchronous or e2e driving a scripted MUD + a mock LLM (canned
  tool_use → assert the command is sent; canned end_turn → assert idle). Assert
  single-flight (a second wake during a think does not launch a second call).
- **Done when:** the loop completes a full observe→think→act→observe cycle against a
  mock LLM without blocking the Session.

#### T6 `[Lua]` — tool layer (incl. reflex-programming; the centerpiece)
- **Depends:** T5.
- **Create:** `lua/core/NN_agent_tools.lua`. Model tools as a
  `rune.registry.new{ kind = "tool" }` so they get names/groups/quarantine/`/tools`
  listing like every other subsystem.
- **Ship these tools:**
  - `send_command(cmd)` — generic escape hatch → `rune.send`.
  - `speak(channel, msg)` — `say`/`tell <who>`/`gossip` (for collaboration-via-channels).
  - `create_trigger(pattern, command, opts)` — the key one. Builds
    `rune.trigger.regex(pattern, command, { group = "agent-combat"|"agent-nav"|..., ... })`.
    **Use the string-action form** (`"kill %1"` with `%1` capture substitution — see
    `lua/core/50_triggers.lua`): it is data, not code, so no `loadstring`, fully
    auditable and quarantine-able.
  - `create_alias`, `remove_group(group)`, `list_automation()` (from
    `rune.trigger.list()`), so the agent can inspect/prune its own reflexes.
- **Group discipline:** every agent-authored entry goes in an `agent-*` group so a
  zone/goal switch is one `rune.trigger.remove_group("agent-combat")`, and the human can
  see agent reflexes in live mode.
- **Do NOT (yet):** ship a `run_lua(code)` codegen tool. Deferred, gated (see §6).
- **Combat walkthrough to validate against:** new mob type → LLM writes triggers for the
  repetitive rounds (chase on flee, quaff-heal below threshold) into `agent-combat` →
  disengages → triggers autopilot at machine speed, zero LLM cost → LLM re-wakes on
  exception (see T9).
- **Tests:** e2e where a canned tool_use `create_trigger` installs a trigger, then a
  matching server line fires the command with captures substituted; `remove_group` clears
  it. Confirm agent triggers are quarantined after 3 failures (shared machinery).
- **Done when:** the agent can install, fire, list, and clear its own triggers by group.

#### T7 `[Lua]` — observability (live mode)
- **Create:** `lua/core/NN_agent_ui.lua` (or fold into agent). A reasoning pane
  (`rune.pane` — note this fork has `rune.pane.show/hide`), a state bar (`rune.bars`,
  250ms tick) showing state/goal/tokens/$/last-action, and per-turn logging via
  `rune.log` (prompt, response, tool calls, results).
- **Tests:** light — bar renderer returns expected content for a given agent state
  (Lua-against-mock). Pane writes are best-effort.
- **Done when:** in a live run you can watch reasoning + state; every turn is logged.

### Phase 2 — harden + govern

#### T8 `[Go]` — `rune._llm` transport
- **Why:** streaming (live reasoning), key held in Go (out of Lua), retries/backoff on
  429/529, usage/token reporting.
- **Create:** `lua/api_llm.go` + `session/lua_llm.go`, **mirroring** `lua/api_http.go` +
  `session/lua_http.go` exactly:
  - `session/lua_llm.go`: `Session.LLMRequest(id, req)` spawns a goroutine, streams from
    the Messages API; deliver either incremental deltas as multiple `AsyncResult` events
    (for live streaming) or one final `AsyncResult` (start non-streaming, add streaming
    behind the same id-based delivery).
  - `lua/api_llm.go`: register `rune._llm.request`; `Engine.OnLLMResult(id, ...)` →
    `rune.llm._deliver`.
  - Key from Go env (`ANTHROPIC_API_KEY`); never passed through Lua.
- **Swap:** `rune.llm.chat` (T4) moves from `rune.http` onto `rune._llm`; its public
  signature stays stable so T5/T6 don't change.
- **Tests:** byte-level/unit for request build + streaming assembly; error/retry paths;
  the Lua `_deliver` id map drops stale ids on reload (mirror `80_http.lua` tests).
- **Done when:** streaming reasoning renders live; key never appears in Lua; usage is
  reported to governance.

#### T9 `[Lua + small Go]` — governance
- **Create:** `lua/core/NN_agent_policy.lua`.
  - **Budget:** accumulate `usage` (tokens → $); pause + notify when a per-session cap is
    hit.
  - **Rate limit:** cap outgoing commands/sec (be a good MUD citizen and stop runaways);
    covers channel output too (two bots gossiping forever is the failure mode).
  - **Oscillation:** detect same command / same tool cycle repeated N times with no state
    change → break and wake the LLM.
  - **Irreversible-command gate:** denylist/confirm for `quit`, dropping gear, spending
    currency, etc. before they hit the wire.
  - **Quarantine → re-plan (the payoff loop):** when an `agent-*` trigger is quarantined
    (3 failures), wake the LLM to re-plan. This likely needs a **small hook**: have the
    registry/`guarded_call` emit an event/callback on quarantine (today it just disables +
    reports). Add a minimal signal in `lua/core/15_registry.lua` (or wherever quarantine
    lives) and consume it here.
- **Tests:** budget cap pauses at threshold; rate limiter throttles; oscillation detector
  fires on a repeat loop; a quarantined agent trigger triggers a re-plan wake.
- **Done when:** a runaway/looping agent is contained and a failing reflex re-engages
  cognition.

### Phase 3 — headless

#### T10 `[Go+cmd]` — HeadlessUI + `--headless`
- **Create:** `ui/headless/headless.go` implementing `ui.UI` (see `ui/interface.go` for
  the full contract): `Print`/`Echo`/`SetPrompt`/`UpdateBars`/pane methods → structured
  logging (stdout/JSON/file) or no-ops; `Input()`/`Outbound()` return channels driven by
  the control surface (T11) rather than a keyboard; `Run()` blocks until context
  cancel/shutdown; `Quit()` unblocks it.
- **Wire:** add a `--headless` flag in `cmd/rune/main.go` (around line 85) that
  constructs `headless.New()` instead of `tui.NewBubbleTeaUI()`. **The Session is
  unchanged** — this is the whole point of the `ui.UI` seam. Shutdown already flows
  through `signal.NotifyContext` (SIGINT/SIGTERM) → ctx cancel → `Session.Run` returns.
- **Tests:** a headless run boots, connects, the agent loop runs, output is logged, and
  SIGTERM shuts down cleanly. Reuse e2e harness patterns.
- **Done when:** `rune --headless <target>` runs the agent with no terminal and exits
  cleanly on signal.

#### T11 `[Go/Lua]` — control surface for headless
- **Base:** config-driven autonomy (goal/limits from config or a startup script).
- **Out-of-band:** a small local control (unix socket or signals) for
  pause/resume/step/status/reload — the reliable kill switch that never depends on the
  game being up.
- **In-game (optional, fits the channels model):** supervise via allowlisted `tell`s
  (`tell mybot pause`); the bot already perceives tells. Never the *only* control path.
- **Done when:** a headless bot can be paused/stepped/queried without a terminal.

## 6. Open decisions

- **API key in Phase 1:** recommended — `rune.env("ANTHROPIC_API_KEY")` (T2) so the key
  stays out of git. Alternative: an `init.lua` constant for a throwaway spike.
- **`run_lua(code)` codegen tool:** deferred. Ship the structured `create_trigger`
  vocabulary (T6) first; add codegen later as a **gated** power tool (config/confirm,
  restricted env). Rune's watchdog + pcall + quarantine already sandbox runaway/throwing
  code, but arbitrary codegen can still clobber `rune.*`.
- **Model + Messages API specifics:** resolve via the `claude-api` skill at T4/T8, not
  from memory.

## 7. Suggested commit/PR breakdown

One PR per task (T1…T11), each with its own tests, in dependency order. T1 and T3 first
and in parallel (neither depends on botmud#20). botmud#20 (server GMCP) unblocks a *real*
end-to-end run but not the client critical path — T3 develops against GMCP fixtures
meanwhile.
