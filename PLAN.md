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
   │  GMCP + text        │   rune.perception ──▶ agent core (state machine) ──▶ tool layer ──▶ rune.send│
   ▼                     │   (perception)     idle→observe→wait→act        send_command            │
  network ──▶ Session ───┼──▶ rune.gmcp.on / hooks("output","prompt")      speak(channel)          │
  (Go kernel, 1 goroutine)│                        │                       create_trigger ────────┐│
   ▲                     │                    llm client ──(async)──▶ OpenCode Zen (opencode.ai/zen) ││
   │  rune.send          │   governance: budget / rate-limit / oscillation / quarantine→re-plan   ││
   │                     │   observability: reasoning pane, state bar, turn log                    ││
   └─────────────────────┴────────────────────────────────────────────────────────────────────────┘│
  UI seam (ui.UI): BubbleTeaUI (live)  |  HeadlessUI (headless)  ◀── agent logic is UI-agnostic ────┘
```

- **Perception** — consume GMCP into a live world-model table (`rune.perception`); keep a rolling text
  transcript from the `output` hook. Uses existing `rune.gmcp`/`hooks`.
- **Transport** — Phase 1 uses existing `rune.http.post` to OpenCode Zen
  (opencode.ai/zen, see T2/T4); Phase 2 hardens into a Go `rune._llm` primitive
  (streaming, key-in-Go, retries, usage).
- **Cognition** — the callback-driven state machine and think-cadence policy.
- **Action** — tools: `send_command` and `speak`. (The reflex-programming tools
  `create_trigger` et al. shipped in T6 and were dropped on 2026-07-25 — see §6.)
- **Memory** — three tiers, narrowest first. `rune.perception.transcript()` is the
  200-line rolling window of raw output (T3); `rune.memory` is the durable, scored stream
  of what the agent *learned*, retrieved into every observation and periodically
  compressed by reflection (T13); the session log is the exhaustive backstop the agent
  can grep when neither of the first two has it (T12: `rune.log.read`/`search`,
  `search_log`/`read_log`).
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

`[Go]` = kernel primitive, `[Lua]` = user-space module, `[Go+cmd]` = kernel + entry
point. Each task is a self-contained unit of work with its own tests and commit.

### Shipped (T1–T12, 2026-07)

T1–T10 and T12 all landed; **T11 (headless control surface) was cut, not deferred** —
the point of headless mode is a bot that runs without input, and everything T11 was
meant to guarantee is covered by T9's governance, SIGTERM→ctx-cancel shutdown, and the
session log. Their full write-ups were removed from this file once the work was done;
`git log PLAN.md` has them, and source comments still cite task numbers:

- T1 `rune.json` · T2 `rune.env` · T3 perception (`84_perception.lua`) · T4/T4b/T8/T9b
  LLM transport (`86_llm.lua`, `session/lua_llm.go`) · T5 agent core (`87_agent.lua`) ·
  T6 tools (`88_agent_tools.lua`) · T7 observability (`96_agent_ui.lua`) · T9 governance
  (`91_agent_policy.lua`) · T10 headless (`ui/headless/`) · T12 logging-as-memory
  (`92_agent_log.lua`).

### Phase 4 — memory

#### T13 `[Lua]` — memory stream (retrieval + reflection) ✓
- **Why:** the agent had no durable memory of *what it learned*, only of what it saw.
  `rune.perception.transcript()` is a 200-line rolling window (`84_perception.lua`);
  past that, T12's `search_log`/`read_log` could find a line again only if the model
  thought to ask and guessed a regex. The one thing that persisted across turns was
  `agent_goal` — the model's own last reply text, replayed verbatim. That single slot was
  carrying the entire cross-turn memory load, badly: it is why the observation heading
  has to disclaim "your own words … not confirmed fact", and why `accddb2` had to stop
  fabricated narrative being fed back as the goal. Facts live in the stream now; the goal
  is a goal again.
- **Prior art:** Park et al., *Generative Agents* (arXiv:2304.03442). Adopted
  selectively — the departures below are as load-bearing as what was kept.
- **Created `lua/core/89_memory.lua`** (`rune.memory`), inert until
  `rune.memory.enable()`, which `rune.agent.start` calls and `rune.agent.stop` unwinds —
  the same lifecycle as `84_perception.lua`, for the same reason: a core file loads in
  every session, and subscribing to GMCP or writing the durable store are side effects a
  human who never started a bot did not ask for. `disable()` stops recording but
  deliberately does **not** forget, so a restarted agent still knows what it learned
  (tested).
- **Record:** `{ id, t, kind, text, importance, tags, refs }`, persisted in `rune.store`
  under `agent_memory` as `{ next_id, pending, records }`. Not a bare array: ids must
  survive eviction because a reflection's `refs` point at them, and the reflection
  counter must survive a reconnect or a long-running bot reflects on a clock that resets
  every restart. `kind` carries the event type (`room`/`enemy`/`level`/`close_call`/
  `tell`/`note`/`reflection`) rather than a flat observation-vs-reflection split — it is
  what importance is derived from, so collapsing it would have meant storing the score
  with no way to explain it.
- **Four departures from the paper, each because a MUD is not a sandbox town:**
  - *Creation is salience-gated, not per-observation.* Automatic records come only from
    GMCP deltas — first sight of a room or a mob, a level, a close call, an inbound tell
    — plus what the model writes with `remember`. A ROM combat round emits lines faster
    than any of them could be worth storing. Each automatic source is deduped: rooms and
    mobs against session-local sets seeded from the stream (so eviction can't cause
    rediscovery), and close calls re-arm only after recovering past 50% hp, so one bad
    fight leaves one memory rather than one per tick. Tells are kept and the broadcast
    channels are not — gossip is the MUD's background noise.
  - *Importance is a static table, not an LLM call.* The paper rates every memory's
    poignancy 1-10 with its own model call; here that would be one extra request per
    memory, on a hot path, that can fail mid-turn. Scoring by `kind` is free and
    deterministic; the model may still set its own on a `remember`, where it is already
    paying for the turn.
  - *Recency decays per minute, not per hour.* The paper's `0.995^h` ran over **sandbox**
    hours, where a simulated day passed in minutes of real time. Applied to real hours the
    same constant barely decays at all (0.89 after a full day). Per minute puts the
    half-life near two hours — the scale a play session actually runs on.
  - *Relevance is tag + keyword overlap, behind a swappable seam.* No embedding provider
    exists in this stack (Zen's catalog is chat models). Tags carry most of the weight on
    purpose: "what do I know about room 3054" is an exact-match question and a join
    answers it better than a similarity search. `rune.memory.set_relevance(fn)` replaces
    the whole factor when embeddings show up, with no caller changes — the boundary that
    let a second LLM provider land inside `86_llm.lua` untouched by anything above it. A
    relevance function that throws is caught and scores zero rather than killing the turn
    (tested).
- **Retrieval:** `rune.memory.recall{limit, text, tags, weights}` scores every record —
  `w.recency·recency + w.importance·importance + w.relevance·relevance`, each factor
  min-max normalized as the paper specifies (a constant vector maps to all-1, which
  leaves ranking untouched instead of dividing by zero). Tags default to
  `context_tags()`, built from the current room/area/enemy in `perception.snapshot()`.
- **Two weight sets, which the implementation forced.** With the paper's all-1.0
  weights, a *perfect* relevance hit is worth exactly as much as being the single most
  important memory — so a high-importance non-match ties with the record that literally
  contains the search term, and the newer-wins tiebreak then puts the wrong one first.
  Caught in a smoke test where `/memory search smithy` ranked an unrelated reflection
  above "the smithy buys weapons". Equal weights are right for the *automatic* retrieval
  feeding each turn ("what should be on your mind"); an explicit question is a different
  query ("what do you know about X"), so the `recall` tool and `/memory search` pass
  `rune.memory.query_weights` (recency 0.5, importance 0.5, relevance 2.0) instead.
  Pinned by a regression test.
- **Reflection — the point of the exercise.** Fires from `agent_turn_end` once
  importance accumulated since the last one crosses 50 (the paper's 150 assumed an agent
  recording every observation; ours records far fewer, denser ones, so 150 would mean
  reflecting roughly never). Feeds the last 40 records to the model, parses one
  conclusion per line, and stores each as a `kind="reflection"` record with `refs` back
  to its sources. This is what turns three separate "killed by the cityguard"
  observations into "cityguards are fatal at this level".
  - Runs **outside** the turn loop with its own single-flight flag: `87_agent.lua`'s
    `thinking` guard does not cover this call, and two concurrent requests is exactly
    what that guard exists to prevent.
  - The counter resets when the request goes **out**, not when it returns — otherwise a
    reflection that errors leaves the threshold tripped and retries on every subsequent
    turn end. Both properties tested.
  - Eviction never drops a reflection to make room for an observation: a reflection is
    the compressed form of the observations it cites, so that would be backwards. Falls
    back to the oldest record of any kind if the stream somehow holds only reflections —
    a cap that cannot be enforced is not a cap.
- **The stream cap is the watchdog guarantee.** `MAX_RECORDS = 400`, and recall scores
  every record, so all work is O(cap) regardless of how long the bot has been running —
  nothing here scales with session length. Measured rather than assumed: a full
  400-record stream with tags and a text query recalls in **~5.4ms**, three orders of
  magnitude under the 5s deadline. If it ever needs to be much larger, scoring moves to
  Go as a primitive the way T12's `LogSearch` did, rather than growing the Lua loop.
- **Tools:** `remember(text, importance?, tags?)` and `recall(query?, limit?)`, described
  to the model as memory rather than search — `recall` returns what it *learned*, where
  T12's `search_log` returns raw lines it saw. Registered through a new
  `rune.agent_tools.register` export from `88_agent_tools.lua` so they get the same
  name/source/quarantine machinery as `send_command`; the registry itself stays private,
  so nothing can enable/disable entries behind `/tools`' back.
- **Prompt:** `build_observation()` gained a `## What you've learned (your memory - these
  did happen)` section, capped at 6 records and omitted entirely when the stream is empty
  (no empty heading in every prompt). Worded to contrast with the goal line directly
  above it, which is explicitly the model's own unverified words — these are not.
- **Accounting:** reflection is a second LLM call outside the turn loop, so it never
  reaches `agent_reply`. It fires `agent_reflection(insights, reply)`, which both
  `91_agent_policy.lua` (budget) and `96_agent_ui.lua` (token display) now pick up
  alongside `agent_reply` via a shared `count_usage`. A budget that ignored them would be
  one the bot could exceed just by remembering a lot. `96_agent_ui.lua` renders insights
  as `[reflect]` in magenta, distinct from ordinary inline reasoning — a conclusion just
  committed to memory is a different kind of event from a plan for the next command.
- **`/memory`** — `[n]`, `search <text>`, `reflect`, `forget`: the same view the agent's
  own tools get, plus the reflection counter, mirroring `/policy` and `/loglines`.
- **Deliberately not doing:** the paper's recursive planning tree (daily chunks → hourly
  → 5-15 minute actions). It presumes a world you control and a clock that matters; a MUD
  is interrupt-driven and an aggro mob invalidates the plan every thirty seconds. §1
  already settled this domain's answer — the LLM sets intent, the scripting layer
  executes tactics. Per-observation creation and LLM-scored importance are not coming
  back either; only embedding relevance left a seam.
- **Tests:** `lua/memory_test.go`, 31 Lua-against-MockHost tests — store round-trip,
  importance defaults/clamping/validation, eviction holding the cap while keeping
  reflections, each retrieval factor isolated (recency with everything else equal,
  importance at equal age, the tag join, query text), the query-weights regression, limit
  clamping, `set_relevance` swapping and restoring, a throwing relevance function, all
  four automatic capture sources including their dedupe, enable idempotence and disable
  leaving no hooks, memory surviving an agent restart, `forget_all`, reflection firing on
  threshold and storing parsed insights with refs, the counter resetting on error,
  single-flight, refusal while stopped, token accounting reaching both governance and the
  bar, both tools registered and driven through the real turn cycle, and the observation
  section appearing and being omitted.
- **Done when:** the agent can act on something it learned well outside the 200-line
  window without reaching for `search_log` ✓; reflections are stored and surfaced ✓;
  retrieval cost is bounded by the cap rather than by uptime ✓ (measured). **Still
  unproven:** whether reflection produces *useful* conclusions on a real bot against
  botmud — the threshold (50), sample size (40), and the reflection prompt itself are all
  first guesses, and tuning them needs a live run, not another test.

## 6. Open decisions

- **`run_lua(code)` codegen tool:** still deferred, and now less likely. The structured
  reflex vocabulary it was meant to follow (`create_trigger` et al.) was itself dropped
  on 2026-07-25 — real runs showed a two-layer agent, part direct and part
  self-installed triggers, confused both the model and the human watching it. If codegen
  ever lands it is a **gated** power tool (config/confirm, restricted env): the watchdog
  + pcall + quarantine sandbox runaway and throwing code, but not code that clobbers
  `rune.*` on purpose.
- **Provider: OpenCode Zen, not Anthropic directly.** One key buys a rotating catalog of
  models, including free/promotional ones; the point is cheap experimentation, not a
  specific vendor. `rune.llm.chat`'s public signature stays provider-shaped
  (system/messages/tools/max_tokens in, a normalized reply out) so swapping model or
  provider doesn't ripple outward. **Bore out as intended:** a second,
  OpenAI-chat-completions-shaped provider (`llama.cpp` et al.) landed behind
  `RUNE_LLM_PROVIDER` entirely inside `86_llm.lua`/`session/lua_llm.go`, with zero
  changes to the agent core, tools, or governance.
- **Zen wire format — confirmed against real 200s, keep this handy.** `x-api-key` (not
  `Authorization: Bearer`) is the auth header, `2023-06-01` is an accepted
  `anthropic-version`, `/v1/models` lists the catalog, and **Zen fronts two
  differently-shaped endpoints, not one**: `/v1/messages` (Anthropic Messages shape) is
  Claude-only, while every other model — deepseek, glm, hy3, mimo, the free/promotional
  ones this provider exists for — needs `/v1/chat/completions` instead. Sending the
  wrong model to `/v1/messages` does not 404; it fails opaquely, which is why the bug
  shipped and ran for a while before being root-caused.

## 7. Suggested commit/PR breakdown

One PR per task, each with its own tests, in dependency order.
