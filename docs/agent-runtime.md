# Agent runtime plan

This is the next engine work. Platform work already listed in the README (notifications, iOS/Android, watchOS) stays on that list; it does not fix whether a member can actually finish a job.

The loop, the approval gate, MCP, skills, and the group protocol are in place. What fails on real tasks is more specific: the model cannot edit a file without rewriting it, cannot search without a shell, forgets the assignment after sixty messages, and has its whole diary stuffed into every prompt. The changes below close those gaps without a new plugin format, a new package split, or a pretend sandbox.

## Constraints

These are the same rules as `docs/architecture.md`. They decide the shape of every phase.

- Workers, the bus, and the toolbox stay in `engine`. Split large files inside that package; do not invent a `tools` package to hold four functions.
- HTTP/SSE remains additive. New event fields are optional. Old clients must still render a conversation.
- Every new capability that writes, runs, or leaves the workspace goes through `Toolbox.gate`. A tool that forgets the gate is a bug, not a shortcut.
- `bash` stays a heuristic. New file tools must use `resolve` / `inBounds`, not a second path checker.
- Skills stay two-stage: one line in the prompt, `read_skill` for the body.
- Do not add a vector database, a tokenizer dependency, or an OS sandbox in order to ship the first four phases.
- Source strings stay English and go through `i18n.T`.

## What this is not

Leave these alone until the runtime above is in:

- Slash commands and plugin hooks (`commands/`, `hooks/`).
- Nested sub-agents. `message_bot` is a peer message, not a child process; replacing it is a different design.
- Computer use, a real browser, or a PTY.
- Git as its own tool. `grep` plus `bash` is enough once file tools exist.
- A shared group workspace object. The user still names a directory; that grant path stays.
- Embeddings. Memory search in v1 is substring / token overlap over the markdown the user can already open.
- Scraping Google. Engine-side search must use a provider we can stand behind, or a key the user already stored.

## Order

Each phase is one pull request. Tests for that phase land in the same PR, not a follow-up. Later phases may assume earlier tools exist.

```text
1. File tools          edit / ranged read / grep / glob + approval diffs
2. Context             char-aware compact, new conversation, one auto-continue
3. Plan in a DM        personal todos + a plan card; do not reuse the group board
4. Memory              two-stage memory, forget, recall, search this chat
5. Web search          one engine tool every provider can call
6. Eval suite          scripted trajectories in CI (grows from phase 1)
7. Audit and allowlist append-only audit log, fetch domain list, edit-the-command
```

Phase 6 is listed late because a dedicated harness is extra machinery. Every earlier PR still adds scripted-provider tests for the behavior it introduces. Do not wait for a harness to assert that `edit_file` fails when `old_string` is not unique.

---

## Phase 1 — File tools

This is the largest jump in task success. A coding member today has `read_file` (whole file, 20k cap) and `write_file` (overwrite). That is how files get destroyed and how large files become invisible.

### Tools

Keep `write_file` for new files and whole-file rewrites. Add:

| Tool | Arguments | Gate |
|---|---|---|
| `read_file` | existing `path`; optional `offset` (1-based line), `limit` (line count) | read, no approval |
| `edit_file` | `path`, `old_string`, `new_string`, optional `replace_all` | `ActWrite` |
| `grep` | `pattern`, optional `path`, optional `glob`, optional `max` | read, no approval |
| `glob` | `pattern` | read, no approval |

`read_file` with offset/limit returns numbered lines. The cap stays `ToolOutputLimit` characters. Offset past EOF is an error that says how many lines the file has, not an empty success.

`edit_file` replaces exactly one occurrence unless `replace_all` is set. Zero matches or more than one match (and not `replace_all`) is an error that quotes a short context. It never writes a file that was not already there; new files stay `write_file`.

`grep` and `glob` walk `inBounds` only (workspace, granted roots, `/tmp`). Skip binaries. Cap the match list. Implement in Go; do not shell out to `rg` so a machine without ripgrep still searches.

System prompt: for an existing file prefer `edit_file`; for finding code prefer `grep` / `glob` over `bash`. `bash` remains for running tests, git, and anything that is actually a command.

### Approval diffs

`Approval` gains an optional `Diff` field. `RequestApproval` passes it through the SSE extra. The card in `app/renderer/views/chat.js` renders it in a `<pre>` when present. Telegram gets the first lines of the same text.

`edit_file` and `write_file` both attach a unified diff (empty file → new file for writes). The 400-character preview in the action string is no longer the only thing the user sees.

A tiny unified-diff helper lives in `engine` (or `textutil` if both sides need it). Do not add a diff library for this.

### `Execute` return value

`Toolbox.lastImages` is written after `Execute` and is not safe to share across goroutines. Change `Execute` to return `(content string, images []model.ResultImage, err bool)` and stop using `TakeImages` at the call site. That is the prerequisite for any later parallel batch, and it is a behavior-preserving refactor: do it first in this PR, then add the tools.

### Parallel batches (same PR if small, otherwise 1b)

When one `StepResult` contains several calls, run those whose `ToolAct.ReadOnly` is true concurrently; keep writes, bash, and non-read-only plugin calls in order. MCP tools that return images already return them from `Execute`, so the race is gone.

Do not parallelize anything that goes through `gate` and blocks on a human.

### Files

- `backend/internal/engine/files.go` — ranged read, edit, write (move `runReadFile` / `runWriteFile` out of `tools.go`)
- `backend/internal/engine/search.go` — grep / glob
- `backend/internal/engine/diff.go` — unified diff for the card
- `backend/internal/engine/tools.go` — defs, switch, `Execute` signature
- `backend/internal/engine/bot.go` — `describeToolCall`, prompt, call site
- `backend/internal/engine/bus.go` — `Approval.Diff`
- `app/renderer/views/chat.js` — approval card
- `backend/internal/i18n/locales/zh.json`, `app/renderer/locales/zh.js`
- tests beside each new file; extend `permissions_test.go` so `edit_file` follows `ActWrite`

### Acceptance

- Unique `old_string` edits a function without rewriting the file.
- Two identical `old_string`s without `replace_all` refuse to write.
- `read_file` of a 2k-line file with `offset`/`limit` returns that window and a line count.
- `grep` in a granted root does not return hits from a sibling directory outside bounds.
- Approval for an edit shows a diff the test can look for.
- Existing bash / permission / spin tests still pass.

---

## Phase 2 — Context

`HistoryLimit = 60` cuts whole turns. A long coding session loses the original assignment and does not say so. `max_tokens` asks the user to type "continue". There is no way to start a fresh conversation without deleting the member.

### Compaction

Keep turn-boundary cuts. Change the budget from message count to characters (chars/4 is a good enough token estimate; do not take a tokenizer dependency).

When history exceeds the budget:

1. Never cut inside the current turn (the `cutTracker` already knows the mark).
2. Drop the oldest complete turns.
3. Insert one synthetic note at the new head: what was dropped, in extractive form (user text first lines, assistant conclusions, tool names). No second model call in v1.
4. Persist the compact note in the session snapshot so a restart does not resurrect the dropped turns.

`config.HistoryLimit` can stay as a backstop count. The character budget is the one that fires first on real traces.

### New conversation

`POST /api/session/reset` with `{ "bot", "chat" }`. Archive `sessions.json` to `sessions-<unix>.json` in that workspace. Put a new empty session in memory. `MEMORY.md`, the workspace, and granted roots stay. UI: conversation menu item "New conversation", next to pin.

This is the user-facing compact. It must not archive `MEMORY.md`.

### One auto-continue

On `max_tokens`, if this turn has not already continued, append a synthetic user line that says the reply was cut off and the model should continue, then loop. After one automatic continue, stop as today. Emit a tool-trace line so the UI is not silent.

### Streaming

Not in this phase. Anthropic still uses `Messages.New`; Codex already folds a stream into one object. Token-level bubbles want an SSE `delta` kind and renderer mutation. That is a separate PR after compact works, because it touches every provider and the chat view.

### Files

- `backend/internal/config/config.go` — character budget, max auto-continues
- `backend/internal/model/providers.go` — `Trim` uses the budget; snapshot stores the compact note
- `backend/internal/engine/bot.go` — auto-continue; reset entry point
- `backend/internal/api/routes_chat.go` — reset endpoint
- `app/renderer/views/sidebar.js` / `context-menu.js` — menu item
- tests: trim keeps the current turn; reset archives; auto-continue runs once

### Acceptance

- A session of many large tool results keeps the latest user assignment and a note that earlier turns were dropped.
- Reset clears model context and leaves `MEMORY.md` in place.
- A truncated reply continues once without a user message, then stops.

---

## Phase 3 — Plan in a DM

The group task board is for division of labor (`assign_task` / one owner). A member working alone in a DM has nothing like it, so multi-step work has no checklist and no moment where the human accepts a plan.

Do not put DM work on the group board. A personal list is a different object.

### Personal todos

Tool `todo_write`: replace the member's current list with a JSON array of `{ "id", "content", "status": "pending"|"done" }`. Persist in `data/workspaces/<bot>/TODO.md` (markdown, so a human can open it). Inject the current list into the system prompt only when it is non-empty — same disappearing-section rule as skills.

Available in DM and in group. It never assigns work to someone else.

### Plan card

Tool `submit_plan`: title plus body. Emits an SSE event the UI renders like an approval: Approve / Reject (with reason). On approve, the same turn continues with a synthetic line that the plan was accepted. On reject, the tool result is the reason and the member revises.

This is the mechanical stand-in for "plan mode". A composer toggle can come later; the tool is enough for the model to stop before a multi-file edit when the permission tier is `ask` or `edit`.

Prompt rule: if the work will touch more than one file, call `todo_write`, then `submit_plan`, and wait. Do not rely on the rule alone — the card is the gate.

### Default skills

Ship three skills in the repo (`assets/skills/` or `backend/internal/skill/bundled/`) and copy them into `data/skills/` when that directory is empty:

- `edit-code` — grep, ranged read, `edit_file`, then verify.
- `verify` — run the project's tests (`go test`, `npm test`, …) after edits.
- `research` — `web_search` (phase 5) or `fetch_url`, then `remember` only what should survive.

An existing `data/skills/` with user files is never overwritten. Bundle-provided skills already merge through `SyncSkillRoots`; these three are the local library's starter set.

### Files

- `backend/internal/engine/todos.go`
- `backend/internal/engine/tools.go`, `bot.go` prompt
- `backend/internal/api` — no extra route if todos only move through tools and `/api/state`
- `app/renderer/views/tasks.js` — a personal list under the DM, or reuse the side pane with a clear heading
- bundled skill markdown
- tests: todo injects only when non-empty; rejected plan does not apply edits

### Acceptance

- In a DM, a multi-file request produces a plan card before `edit_file`.
- Approving continues the same turn; rejecting returns a reason the model sees.
- A member with an empty todo list does not get an empty `# Todos` heading.

---

## Phase 4 — Memory

`remember` only appends. The whole file goes into every prompt. There is no forget, no search, and no way to look up an old chat line.

### Two-stage memory

Treat memory like skills.

- On disk, keep markdown a person can edit. Each bullet gets a stable id: `- [a1b2] [2026-08-18] text`.
- The system prompt lists `id: first clause` lines, capped, not the bodies.
- `recall` loads matching bodies by id or by substring. `remember` inserts or replaces by id. `forget` deletes by id.
- Duplicate detection: if `remember` is called with a note that already matches a body, return the existing id and do not append.

Team and personal memory stay two files. `recall` takes `scope=self|team|both` (default `both`).

### Chat search

Tool `search_history`: keyword, this conversation only, over `events.jsonl` through the existing log scan. Return a short list of `{id, from, text}`. Read-only. Cap hits. This is how a member looks up "what did we decide about the API" after compact has dropped the turn.

Do not build a global RAG index in this phase.

### Files

- `backend/internal/engine/memory.go` — parse, roster, upsert, delete, search
- `backend/internal/engine/tools.go` — `recall`, `forget`; `remember` grows an optional `id`
- `backend/internal/engine/bot.go` — prompt lists the roster
- `backend/internal/engine/eventlog.go` — a scan helper filtered by chat
- tests: old `MEMORY.md` files without ids still load (assign ids on next write); roster omits bodies; `forget` removes a line

### Acceptance

- A 200-line `MEMORY.md` adds a few dozen short lines to the prompt, not 200 bodies.
- `recall` of a keyword returns the stored note.
- `forget` of that id stops it appearing in the roster after the next turn.
- `search_history` finds a user message that compaction has already dropped from the live session.

---

## Phase 5 — Web search

Non-Claude members are told they have no search engine. `fetch_url` needs a URL they already know. That is a team-wide capability hole, not a vendor quirk.

### Tool

`web_search` with `query`, returning up to eight `{ title, url, snippet }` rows. Then the model uses `fetch_url` on a URL it chose.

Use the same `fetchClient` dialer as `fetch_url` (public IPs only, no loopback, no metadata). Parse a search HTML page against `httptest` fixtures; do not hit the network in unit tests.

Provider: a DuckDuckGo-style HTML endpoint is acceptable as a default if it stays fetch-only and SSRF-safe. A stored Brave/Tavily key in the key store, when present, wins. Do not scrape Google.

Claude keeps server-side `web_search` / `web_fetch`. The prompt lists both: engine `web_search` is always there; Anthropic's tools remain extra.

### Domain allowlist (can share this PR or ride with phase 7)

`fetch_url` and `web_search` consult an optional per-bot or global host list. Empty list means today's public-internet policy. Non-empty means only those hosts. The comment in `fetch.go` already names this as the way to tighten GET-by-URL.

### Files

- `backend/internal/engine/searchweb.go` (name it so it does not collide with file `search.go`)
- `backend/internal/engine/fetch.go` — allowlist hook
- `backend/internal/engine/bot.go` — `webLine` for every provider
- settings UI if the allowlist is user-facing in this PR
- tests with fixtures; SSRF cases reused from `fetch_test.go`

### Acceptance

- An OpenAI-compatible member can call `web_search` without bash and without approval.
- A query that would resolve to `127.0.0.1` is refused.
- Claude members still have the server-side tools.

---

## Phase 6 — Eval suite

Unit tests prove the gate and the parsers. They do not prove a member reads a skill before starting, or that it greps instead of `cat` of the whole tree.

### Shape

A scripted `Provider` (already used in `engine` tests) plus a fixture workspace. Each case is:

1. Files on disk.
2. A user message.
3. A script of `StepResult`s (what the model *would* call), **or** a small policy check on the tools the fake model is *allowed* to see.
4. Assertions on disk, tool names used, and messages emitted.

CI runs these with `go test`. No live API in the default job.

Start with cases that lock phase 1–4:

- Edit a function → `edit_file`, not `write_file` of the whole path.
- Find a symbol → `grep`, not `bash grep`.
- Matching skill in the roster → `read_skill` before `edit_file`.
- Group background message → no `end_turn` text reply (already covered in `spin_test.go`; keep it).
- `remember` then a new session → `recall` sees the note; the prompt roster does not include the body.

A live-eval job behind `BOTBUREAU_LIVE_EVAL=1` can wait. It does not belong on `pull_request` until it is cheap and deterministic.

### Files

- `backend/internal/engine/eval_test.go` or `testdata/eval/...` loaded by tests
- fixture trees under `backend/internal/engine/testdata/`

---

## Phase 7 — Audit and a tighter gate

Do this after the tools exist, or the log will only record `write_file` blasts and bash.

- `data/audit.jsonl`: one line per bash, write, edit, non-read-only MCP call, and approval decision (id, bot, act, path or command, allowed/denied, time). Never compacted. `events.jsonl` may still drop old tool chatter.
- `/api/approve` accepts optional `command` for bash: the user edits the line, the engine runs that string, not the original. Empty means the original. Telegram can skip edit and keep binary approve/reject.
- Fetch/search host allowlist if it did not land in phase 5.
- OS sandbox (seatbelt, landlock, container) is explicitly **not** this phase. When it happens, it is a new `ToolAct` dimension, not a rewrite of `shellscan.go`.

### Acceptance

- After a rejected write, `audit.jsonl` contains the deny and the path.
- Approving a bash card with a changed command runs the changed command.

OS sandbox (Seatbelt, bubblewrap, Landlock) is not part of this numbered plan. Approval and isolation are independent knobs; see [`docs/sandbox.md`](sandbox.md).

---

## Compatibility

- `bots.yaml`, `mcp.yaml`, and `data/` stay valid. New session archives are extra files.
- `MEMORY.md` without ids remains readable; ids are added on the next `remember` / rewrite.
- Old Electron clients ignore unknown SSE fields (`diff`, plan cards). New fields must be optional extras, not a new event kind that crashes a switch.
- `write_file` and `bash` remain. Prompt language changes what the model prefers; it does not remove the escape hatch.

## How to see that it worked

Point a member at a real repository (this one is fine), grant the directory, and ask for a small behavioral change. Success looks like:

1. `grep` / `glob` to find the site.
2. `read_file` with a line window.
3. A plan card or a todo list for anything that spans files.
4. `edit_file` hunks, a diff on the approval card, tests run in `bash`.
5. `remember` only of a durable preference, not of the patch.

If the trace is still `read_file` of a whole package followed by `write_file` of 400 lines, phase 1 is not done.
