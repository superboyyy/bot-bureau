English | [中文](README.zh-CN.md)

# Bot Bureau — different AI agents, work together for you

**Bot Bureau** is an "agency of AI members" running on your own machine. You hand out work as if messaging a team member; they finish multi-step tasks on their own, come back to you for approval when human judgment is needed, remember your preferences, turn recurring work into scheduled routines, and divide labor among themselves in a group chat.

Bot Bureau combines a **Go engine with a cross-platform Electron desktop client**, wired to whichever model APIs you like — data, memory, and workspaces all stay local.

```
┌────────────────────────┐   HTTP + SSE   ┌─────────────────────────────┐
│  Electron desktop app  │ ◄────────────► │  Go backend engine (backend/)│
│  app/                  │   127.0.0.1    │  resident bot goroutines ·   │
│  group chat/DMs ·      │                │  agent loop · approval gate ·│
│  approvals · routines ·│                │  memory · routines ·         │
│  new-bot dialog        │                │  Anthropic / OpenAI-compat   │
└────────────────────────┘                └─────────────────────────────┘
```

## Features

| Capability | How it works |
|---|---|
| Message bots like team members | **Group chat** (call a bot by plain name or @mention it; if nobody is named, the first member in the group handles it; collaboration stays visible to the whole room) + **DMs** (one-on-one independent work) |
| Each bot has its own workspace | Each bot starts with its own workspace `data/workspaces/<bot>/`, plus any existing directories the user explicitly names; file I/O is bounds-checked and bash stays permission-gated |
| Autonomous multi-step tasks, always-on | Each bot is a resident goroutine running its own agent loop, never blocking the others |
| Comes back when human judgment is needed | Non-read-only commands suspend for approval; one-click approve / reject in the sidebar (with optional reason) |
| — | **Starts empty**: no bots and no group chat out of the box; the first launch runs a three-step tour (you are the boss → what you can do → hire your first member), skippable |
| — | **Long-running work reports progress**: while a bot works, a typing bubble sits in the conversation with a small "Working · N steps" line beside it that expands to show each step |
| — | **No @ required**: calling a bot by name in a group (`scout take a look`) is the same as `@scout`; it will not match longer words like `scouting` |
| — | **Light and dark**: follows the system setting, or can be pinned |
| — | **Permission tiers**: ask every time / can edit files / work unattended / no approvals — one team-wide default, overridable per bot; actions leaving the workspace and plugin calls skip approval only under "no approvals" |
| Remembers your preferences | Per-bot `MEMORY.md` long-term memory, injected across sessions |
| Routines | Say "every 30 minutes…" and it's saved as a routine; survives restarts, deletable in the sidebar |
| Multi-bot collaboration | Bots hand tasks to each other in group chat (`message_bot`); other bots "overhear" the room |
| Works across the web | Claude-based bots ship with server-side `web_search` / `web_fetch` |
| — | **Create your own bots**: click "＋ New Bot" in the UI, fill in name/persona/model; persisted to `bots.yaml` |
| — | **Edit any bot**: hover a conversation and click the pencil (or click the header avatar) to change its display name, avatar, persona, model and API; saving restarts that bot with the new config |
| — | **The group is editable too**: set its name and avatar; unset it falls back to "Group chat" plus the first two members' avatars stacked |
| — | **Multiple group chats**: create one with "＋" at the top of the sidebar; each has its own name, avatar, members and context. The default group cannot be deleted |
| — | **Subscriptions instead of API keys**: sign in to ChatGPT Plus/Pro or SuperGrok directly (OAuth device code — no local callback port, tokens stored 0600 on this machine) and skip buying API credit |
| — | **Models are fetched, not typed**: pick a vendor and the engine asks it for the currently available models, which you choose from a dropdown. If the list cannot be fetched it says so and lets you type the name |
| — | **Messages render as Markdown**: headings, lists, quotes, fenced code and links; built as DOM only, with links restricted to http/https and handed to the system browser |
| — | **Interrupt anytime**: a bot that has run away can have its current turn aborted instead of being waited out |
| — | **Nothing is lost on restart**: message history and per-bot conversation context are persisted (`data/events.jsonl`, `data/workspaces/<bot>/sessions.json`) |
| — | **Multi-model**: native Anthropic + OpenAI-compatible endpoints (OpenAI / xAI Grok / DeepSeek / Kimi / local Ollama / custom…), from a catalog served by the engine |
| — | **API key management**: paste a key right in the New Bot dialog (or manage them all in Settings). Stored in `data/keys.json`, 0600; the UI shows masked values only; takes precedence over env vars |
| — | **Group membership**: add/remove bots in a group's settings; non-members receive none of that group's messages, can't be @mentioned or assigned; DMs unaffected |
| — | **Task board + division-of-labor protocol**: when decomposing work, bots use `assign_task` to give each subtask exactly one owner and `update_task` to claim/deliver publicly; the board is visible in the sidebar — duplicate work is prevented by mechanism |
| — | **Shared team memory**: `remember scope=team` writes to team-wide `data/TEAM_MEMORY.md` (shared across model providers) |
| — | **Bilingual UI (中文 / English)**: follows the system language by default; switch between "Follow system / 中文 / English" in Settings. The choice applies to the UI, backend messages, and the bots' prompt language as well |
| — | **Plugins (MCP)**: the plugins panel connects local plugins (stdio commands such as `npx @modelcontextprotocol/server-*`) or remote connectors (Streamable HTTP + Bearer); implemented at the engine level so **bots on any provider can use them**; each bot subscribes as needed; non-read-only plugin calls always go through approval (`readOnlyHint`-annotated ones run directly); config lives in `mcp.yaml` |

## Quick start

Requirements: Node.js ≥ 22.12 (to run Electron) and Go ≥ 1.26.6. No backend binary is committed, so the engine is built from source. `npm start` and the packaging scripts run `npm run build:backend` for you.

```bash
cd bot-bureau/app
npm install
npm start
```

**The first launch starts empty** — no bots, no group chat. Onboarding walks three steps: it frames who you are (you run the bureau; the bots are staff you hire), points at where things are created and managed, and then helps you create your first member (pick a vendor → sign in or paste a key → choose a model from the list it fetched). Skipping is fine; the ＋ in the sidebar is always there.

Try it: ask a member in a DM to write a script (running it triggers an approval); create a group, add a few bots to it and assign work by name (`scout look up today's AI news` — no @ needed); say "check the HN front page every 60 minutes".

## Automated testing

The repository keeps Go tests beside their package (`*_test.go`), while Electron tests live under `app/test/` and `app/e2e/`:

```bash
# Go backend
cd backend
go test ./...
go test -race ./...
go vet ./...
go test ./... -coverprofile=/tmp/botbureau.cover
go tool cover -func=/tmp/botbureau.cover

# Electron unit tests and coverage
cd ../app
npm ci
npm run test:unit
npm run test:coverage

# Real Electron + Go backend smoke test
npm run build:backend
BOTBUREAU_RUN_E2E=1 npm run test:e2e
```

The Electron E2E test uses a temporary data directory and never contacts a real model, Telegram account, OAuth provider, or MCP server. It is skipped unless `BOTBUREAU_RUN_E2E=1` is set. CI runs backend tests, the race detector, static checks, frontend coverage, and the Electron smoke test.

Architecture and contributor workflows are documented in [`docs/architecture.md`](docs/architecture.md), [`docs/development.md`](docs/development.md), and [`CONTRIBUTING.md`](CONTRIBUTING.md). From the repository root, `make test` runs the fast checks and `make test-e2e` runs the desktop smoke test.

## Where data is stored

**In development** (`npm start` inside the repo): everything sits in the repo root — `bots.yaml`, `mcp.yaml`, `data/`, `connect.json` — right where you are working. All four are gitignored, since they hold your keys, your bots and your chat history. Copy the templates to get started, or just create a bot in the UI and let the app write the file:

```bash
cp bots.example.yaml bots.yaml && cp mcp.example.yaml mcp.yaml
```

**Once installed**: everything moves to the platform's user-data directory, follows the account, and survives uninstalling the app:

| Platform | Location |
|---|---|
| macOS | `~/Library/Application Support/Bot Bureau/` |
| Windows | `%APPDATA%\Bot Bureau\` |
| Linux | `~/.config/Bot Bureau/` |

What lives there:

```
bots.yaml              team definition (written by the app as you create bots)
mcp.yaml               plugin / connector definitions
connect.json           remembered remote engine address and cert fingerprint
data/
  keys.json            API keys (0600, readable only by you)
  xai_oauth.json       subscription tokens (0600)
  chatgpt_oauth.json   subscription tokens (0600)
  token                LAN pairing code (0600)
  events.jsonl         append-only message history (survives restarts)
  workspaces/<bot>/    each bot's own working directory + MEMORY.md + sessions.json
  TEAM_MEMORY.md       team-wide shared memory
  tasks.json           task board
  routines.json        scheduled routines
  groups.json          group chats and membership
  settings.json        language, permission tier, group metadata
```

**Downloading only the client, with no source, works fine** — the backend binary ships inside the package, and an empty configuration is created on first launch: no bots, no groups, nothing seeded. (Read-only assets and mutable data used to share one path, so anyone who installed only the app hit a write failure into the read-only `app.asar` on startup. They are separate now, and nothing from the build machine's own setup rides along in the package.)


### Putting it somewhere else

`BOTBUREAU_DATA_DIR` relocates the whole set above. Both the client and the backend binary honour it (an explicit `-data` on the backend still wins):

```bash
# Two unrelated setups on one machine: separate bots, keys and memory
BOTBUREAU_DATA_DIR=~/BotBureau/work     npm start
BOTBUREAU_DATA_DIR=~/BotBureau/personal npm start

# Keep the repo clean in development (the default location is the repo root, and one run
# creates data/ and connect.json and rewrites bots.yaml)
BOTBUREAU_DATA_DIR=/tmp/bb-dev npm start
```

`~` is expanded and relative paths resolve against the current directory; a missing directory is created. The terminal prints where it actually landed:

```
[bot-bureau] data dir: /Users/you/BotBureau/work
```

**Changing the directory changes the whole identity**: the pairing code lives in `data/token`, so other devices have to pair again, and `bots.yaml` is a fresh copy that runs first-launch onboarding.

## Interface language

Bot Bureau **follows the system language** by default (the client reads the Electron locale, the engine reads `LANG`/`LC_ALL`); the Settings panel lets you choose "Follow system / 中文 / English" explicitly. One switch covers three layers:

1. **UI**: every static label and dynamic message (the choice is remembered locally by the client);
2. **Engine messages**: approval prompts, errors, routine triggers, quota alerts and other event text (persisted in `data/settings.json`, kept across restarts);
3. **Bot language**: the whole system prompt switches language and tells bots to "reply in the user's language" — so bots speak Chinese under a Chinese UI and English under an English one.

When the bare backend runs on its own, the language comes from `data/settings.json`, falling back to `LANG` (e.g. `LANG=en_US.UTF-8 ./botbureau-backend …`). Note: `role`/`description` in `bots.yaml` are your own data and are never translated — to make a default team read naturally in both languages, add the optional `role_en` / `description_en` fields (below).

## Permission tiers

There are four tiers that decide which of a bot's actions need your approval. **Every boundary is something the engine can actually decide**, not a difference in wording:

| Tier | Skips approval | Still asks |
|---|---|---|
| **Ask every time** (default) | Read-only commands, file reads, read-only plugins | File writes, commands, writing plugins |
| **Can edit files** | ＋ file writes inside the workspace | Commands, plugins |
| **Work unattended** | ＋ ordinary commands inside the workspace | Anything leaving the workspace, plugins |
| **No approvals** | Everything | Nothing |

Two rules cut across every tier:

- **anything leaving the workspace is always asked**, except under "No approvals" — otherwise "Work unattended" would equal it;
- **non-read-only plugin (MCP) calls likewise skip approval only under "No approvals"**. The engine cannot judge a plugin's blast radius: an fs plugin writing a file and a GitHub plugin opening an issue look identical at the protocol level, yet one touches a local sandbox and the other your live repo.

Here, "the workspace" means the bot's own directory plus any concrete, existing directories the user explicitly names in a message. File reads and writes are path-checked against those roots; bash is a **heuristic**, not a sandbox. Absolute paths inside those roots are allowed, while paths outside them (including relative `..` paths) are caught. Command substitution (`` ` `` and `$(`) always counts as escaping because the target cannot be determined before the command runs. Pipes and redirects do not themselves escape the workspace: the command's working directory is pinned, so relative paths stay inside. That is what lets "Work unattended" run `echo x > note.txt && cat note.txt` without making the tier pointless.

Settings → Permissions sets the **team-wide default**; each bot can pick its own tier in its settings, defaulting to "follow the global setting". Changes take effect immediately, with no restart. An invalid or empty value always falls back to the most conservative tier — never accidentally to "No approvals".

## Connecting a model

Three steps in the UI — no base URL to know, no model name to type:

1. **Pick a vendor** — Anthropic Claude / OpenAI · ChatGPT / xAI Grok / DeepSeek / Kimi / Ollama / custom / Fake (offline trial). The catalog is served by the engine (`GET /api/providers`); the client carries none of it.
2. **Pick how to connect** — OpenAI and xAI support two ways:
   - **Sign in with a subscription**: click "Sign in" and enter the pairing code the dialog shows. This is the OAuth device-code flow (plus PKCE for ChatGPT) and **opens no callback port on your machine**; tokens are stored 0600 under `data/` and refreshed automatically. Uses your ChatGPT Plus/Pro or SuperGrok allowance instead of purchased API credit.
   - **API key**: paste it right there and it is saved to the key store — no detour through Settings.

   Whichever you choose is what gets used. The old rule only fell back to a subscription when no key was stored, so an unrelated saved `OPENAI_API_KEY` silently shadowed the subscription you had just signed into.
3. **Pick a model** — the engine uses your credential to ask the vendor which models exist right now (`POST /api/models`) and you choose from a dropdown. **If the list cannot be fetched it reports the error and lets you type the name**; it never substitutes an invented list, which is exactly how one ends up on a retired model that only fails when a message is sent.

First run walks you through the same panel once: choose once, apply to every member that has no model yet. Each of them can be changed individually afterwards.

## Configuring multiple models (bots.yaml)

Everything done in the UI lands in this file, which you can also write by hand. `bots.example.yaml` in the repo root is a fuller annotated template — `bots.yaml` itself is gitignored, since it holds your own choices:

```yaml
bots:
  - name: chief          # defaults to anthropic + claude-opus-5
    role: Lead
    description: Breaks work down, assigns it, and reports back.

  - name: gpt            # rides a ChatGPT Plus/Pro subscription; no API key needed
    role: Advisor
    description: Offers a second opinion from GPT's perspective.
    provider: openai
    provider_id: openai  # used to repopulate the UI form
    auth: chatgpt        # key (default) / chatgpt / xai / none
    model: gpt-5.1-codex

  - name: grok           # xAI Grok, here with an API key
    role: Researcher
    description: Digs up sources and compares them.
    provider: openai
    provider_id: xai
    auth: key
    model: grok-4
    base_url: https://api.x.ai/v1
    api_key_env: XAI_API_KEY

  - name: local          # local Ollama, no key needed
    role: Local assistant
    description: A small model running on this machine.
    provider: openai
    model: qwen3:14b
    base_url: http://127.0.0.1:11434/v1

  - name: demo           # offline echo, needs no key at all (for trying the UI)
    role: Demo
    description: An echo bot.
    provider: fake
```

`provider: anthropic` (the default) is the only provider with server-side web search and refusal fallback; OpenAI-compatible bots can reach the web via bash `curl` (through approval).
Pre-existing configs with no `auth` keep working under the old rule (guessing from `base_url`).

## Directory layout

```
backend/                 Go engine (HTTP + SSE)
  main.go                wiring only
  internal/
    i18n/                T() + locales/zh.json (leaf package)
    config/              bots.yaml, settings, permission tiers
    secret/              key store + xAI / ChatGPT OAuth
    model/               provider implementations, vendor catalog, model listing
    plugin/              MCP client (stdio / Streamable HTTP)
    engine/              bus, groups, bot workers, tools, board, memory, routines
    bridge/              Telegram
    netx/                mDNS, TLS, engine lock
    httpx/ textutil/     small shared helpers
app/                     Electron client
  main.js                spawns the backend, windows, multi-device discovery
  renderer/              plain DOM, no framework
    locales/zh.js        Chinese translations (the main process reads this same file)
  test/                   Vitest unit/integration tests
  e2e/                    Electron smoke tests
  scripts/               dev-mode identity fix (Dock name and icon)
assets/make_icon.py      mark and icon generation (SVG, dark + light, macOS squircle mask)
bots.example.yaml        team definition template (copy to bots.yaml; that one is gitignored)
```

The files in `engine` are genuinely coupled — the bus holds workers and the workers use the bus — so splitting them would only manufacture interfaces. Keeping them in one package is the design, not laziness.

## Code conventions

**The source is English only.** Every user-facing string is written in English in the code; other languages live in translation files keyed by that English text:

- Go: `i18n.T("Message is empty")` → `backend/internal/i18n/locales/zh.json`
- Renderer: `t("Search")` and HTML `data-i18n="Search"` → `app/renderer/locales/zh.js` (the main process reads the same file, so both sides share one table)
- Values use `%s` placeholders: `t("%s members", n)`. Do **not** interpolate before the lookup — the resulting string differs every time and would never match a table entry.

A missing entry falls back to the English source, so **an untranslated string never renders as a blank**. Adding a language is one more file with the same shape.

**Comments are written in English.** Source comments stay in English so the implementation and maintenance notes have one consistent language; user-facing text is translated through the locale tables above.

## Appearance

The UI follows the system light/dark setting; Settings → General can pin it to "Follow system / Light / Dark". The choice is stored locally rather than in the engine — several devices may share one engine, and how bright a screen is stays that device's own business.

The mark is a pair of stacked Bs — the front one carries a face in its lower bowl, the one behind is its shadow and stays faceless. The vector lives in `assets/logo.svg`, and the letterform is built entirely from rounded rectangles and circles: one set of parameters becomes SVG arc paths and, flattened, polygons for the raster, so the two cannot drift apart. The gap between the two Bs is derived rather than drawn — inflating each piece of the front B and unioning equals dilating the union.

App icons are generated in both appearances (`assets/icon-dark.png` / `icon-light.png`): white ground in light, black in dark, with the mark lightened to the same hue on black, where the original brown is nearly invisible. They are assembled in layers the way iOS 26's Liquid Glass expects rather than painted flat — a ground, the mark, and a specular layer of lit top edge, shaded bottom edge and shadow beneath, all derived from the mark's own mask so they follow the letterform exactly. The macOS build is masked to a superellipse with padding per Apple's icon grid — the system does not round your corners, so a plain square PNG lands in the Dock as a conspicuous rectangle. The package ships the dark build: Dock and taskbar materials skew dark and it holds up on both.

### The icon follows the system appearance

Two layers, each covering what the other cannot:

**While the app runs**, the main process listens to `nativeTheme` and swaps the Dock icon whenever the system changes appearance (on Windows and Linux it swaps the window icon, which the taskbar follows). It follows the *system* appearance rather than the app's own light/dark setting — the icon sits in the Dock among other icons, on the system's material, not on this app's canvas. Both images therefore ship inside the package: `build/icon.png` and `build/icon-light.png`.

**While the app is not running**, Finder and the Dock show the fixed image baked into the package, which the above cannot reach. Making that switch too requires macOS 26's `.icon` format — which electron-builder 25 does not know; it only knows the `.icns` named in the `icon` field. So an afterPack hook (`app/scripts/mac-liquid-icon.js`) adds the asset catalog itself, after packing and before signing:

1. Open Icon Composer (inside Xcode, at `Xcode.app/Contents/Applications/`) and drop in the flat layers from `assets/icon-layers/` — one ground and one mark per appearance
2. Save it as `assets/AppIcon.icon`
3. Package as usual (`npm run dist:mac:arm64` or similar); the hook picks it up on its own

What the hook does: `actool` compiles the `.icon` into an `Assets.car`, which is copied into `Contents/Resources/`, and `CFBundleIconName` is added to Info.plist. **`CFBundleIconFile` is left alone** — that is electron-builder's `.icns`, the fallback for macOS below 26. The two icons coexist, each serving the systems that understand it, so this needs no second package.

The order matters: `afterPack` runs *before* signing, so these changes get sealed into the signature; the same work done in `afterSign` would break the signature that was just applied.

electron-builder accepts a single `afterPack`, so the config points at `app/scripts/after-pack.js`, which chains the steps in order: add the icon catalog (`mac-liquid-icon.js`), then clear extended attributes last (`strip-xattr.js`). The cleanup has to come last — files written by a later step would carry attributes in and codesign would refuse them all the same.

With no `assets/AppIcon.icon`, or on a machine without full Xcode (`actool` ships with Xcode, not the command line tools), the step is skipped entirely and the build still produces a package — just without appearance switching. Appearance switching is optional and should not make the build fail.

> If the Dock still shows the old icon after a change, that is the system icon cache; `killall Dock` clears it.

## Packaging and per-platform plans

### Desktop (buildable today)

```bash
cd app
npm install           # once, to get electron-builder
npm run dist:mac:arm64      # dmg, Apple Silicon
npm run dist:mac:x64        # dmg, Intel
npm run dist:mac:universal  # one dmg for both (roughly twice the size)
npm run dist:win:x64        # nsis installer
npm run dist:win:arm64
npm run dist:linux:x64      # AppImage + deb
npm run dist:linux:arm64
```

All three share one Electron client, with the Go engine started as a child process of the app. The resulting package contains:

```
Bot Bureau.app/Contents/
  MacOS/Bot Bureau                   the Electron executable
  Resources/app.asar                 main.js + preload.js + renderer/ (read-only)
  Resources/app.asar.unpacked/bin/   botbureau-backend (the engine — must live outside the asar)
```

Two common pitfalls:

- **The engine binary has to be unpacked** (`build.asarUnpack`). A file inside an asar can be read but **not executed**, so leaving it in there means the packaged app cannot find its engine.
- **Architecture cannot come from a bare `go build`.** That only produces the host architecture, while the dmg has to run on both kinds of Mac; `scripts/build-backend.js` builds arm64 and amd64 separately, joins them with `lipo`, and verifies both slices are present. Windows and Linux cross-compile (the engine is pure Go, `CGO_ENABLED=0`); pass a different `--arch` and run again for another one.

> **Signing and notarization**: with a `Developer ID Application` certificate in the keychain, electron-builder picks it up and signs automatically (hardened runtime, timestamped) — which is why packaging is slow: every one of several hundred files takes a network round trip for its timestamp.
>
> **Signed is not the same as openable, though.** Since macOS 10.15 an app downloaded from the internet must also be **notarized** by Apple: the package is uploaded for scanning and the receipt stapled back into it. Without notarization, macOS still blocks a downloaded build, and the user has to right-click to open it once or run `xattr -d com.apple.quarantine "Bot Bureau.app"`.
>
> To add it, configure `mac.notarize` for electron-builder with an Apple ID / app-specific password / team id (or an API key). Local copying or sharing the app over a LAN needs none of this — the quarantine flag only comes from a browser download.

> **The Dock name in development**: `npm start` runs the Electron inside `node_modules`, so the Dock shows *that* app's name. `scripts/fix-electron-identity.js` rewrites its `Info.plist` at prestart and re-signs it ad-hoc (without re-signing, macOS refuses to launch on the invalidated signature or re-prompts for permissions). Even then, macOS Launch Services caches app names, so the first change may need a re-login or `killall Dock` to show up. **Packaged builds do not have this problem** — they are their own bundle.

## How cross-provider collaboration and division of labor work

**Bots on different providers collaborate and share memory without friction.** Collaboration happens at the application layer (the Go message bus passes plain text) and is independent of the underlying model: a Claude bot handing a task to a Grok bot, or both reading/writing the task board and `TEAM_MEMORY.md`, is just text flowing in and out of each bot's context. Each bot's conversation history is stored independently in its own provider-native format; they never touch. An API key only decides "whose brain this bot uses" — it does not affect the collaboration surface.

**Four layers prevent duplicate work**: ① routing — in group chat, a bot that isn't @mentioned/assigned never acts (it only listens for context); ② board — `assign_task` gives every subtask exactly one owner, and `update_task` → doing is a public claim; ③ protocol — the system prompt hard-codes the division-of-labor rules (check the board before decomposing broad tasks; never touch someone else's task); ④ visibility — the board is injected into every bot's group-chat prompt and displayed in the sidebar, so humans and bots alike can see who is doing what.

## Multi-device sync (serverless)


The pairing code travels in the `Authorization` header only. The message stream (SSE) is the exception — the browser's `EventSource` API cannot set headers (nor can WebSocket), so its credential has to sit in the URL, and reverse proxies write URLs verbatim into their access logs. That connection therefore uses a **short-lived ticket**: the client exchanges the pairing code over `POST /api/sse-ticket` (which can set headers) for a ticket that expires in ten minutes and works solely on `/api/events`. The pairing code itself never appears in any URL.


> **Local mode requires the pairing code too.** "Bound to 127.0.0.1" is not a security boundary — every
> web page open on the machine can reach localhost. `-listen local` used to require nothing, so any site
> you visited could read the whole team and its history, create bots, set the global permission tier to
> "no approvals" and send messages — which chains into running arbitrary commands on your machine from a
> web page. Both modes now require the pairing code, stored in `data/token` (0600): a page cannot read
> it, the client can.

Within the same LAN, devices connect **peer-to-peer** with no cloud server involved:

- **One device runs the engine** (bots, chats, memory, board all live there); other devices launching Bot Bureau **auto-discover it via mDNS**. Discovery raises a non-blocking hint only after the window opens, and the local engine starts as usual unless the user chooses "Pair". After connecting, this machine is a pure client — chat, approvals, plugin/member management all work, with natural strong consistency (a single copy of state, no sync conflicts).
- **Pairing code**: shown in Settings on the engine device; other devices enter it once on first connect and it's remembered. Every endpoint except the discovery probe requires it (constant-time comparison; `data/token`, 0600).
- **Moving the engine between machines**: put `bots.yaml`, `mcp.yaml`, and `data/` in a synced folder (iCloud/Syncthing) for cold migration. The **engine lock** (`data/engine.lock` + heartbeat) stops two devices from running the engine at once — the second is clearly refused, preventing duplicate bot replies; a stale lock left by a crash expires after 30 seconds.
- Debug/manual override: `BOTBUREAU_BACKEND_URL=http://<ip>:<port> npm start` connects in client mode directly; `BOTBUREAU_LOCAL_ONLY=1` listens on 127.0.0.1 only, with no broadcast.
- On first run macOS may ask to "allow local network access / accept incoming connections" — allow it.

Discovery is **non-blocking**: nothing stands in the way of opening the app, and the local engine starts as usual. The LAN is scanned only after the window is up, and another device raises a hint above the settings button. "Pair" is what asks for the pairing code; "Not now" remembers that device and never asks again.

### Across networks (not on the same LAN)

NAT means direct connections across the internet always need help punching through — a direct, third-party-free hole punch is not possible. The recommended approach is to **turn different networks into one private network**:

1. **Tailscale (recommended)**: install [Tailscale](https://tailscale.com) (free for personal use) on both devices and sign in to the same account; find the engine device's Tailscale IP (in `100.64.0.0/10`); on the other device open Bot Bureau → Settings → "Connect to remote engine" and enter `http://100.x.y.z:8973`, then the pairing code once. The address is remembered (`connect.json`) and reconnects automatically on the next launch; if unreachable you're prompted and can fall back to running locally. Traffic is WireGuard end-to-end encrypted with a peer-to-peer data plane. The engine port is fixed at 8973 (falls back to a random port only if occupied).
2. **SSH tunnel**: with any machine that can SSH to the engine device: `ssh -N -L 8973:127.0.0.1:8973 user@engine-host`, then connect to `http://127.0.0.1:8973`.
3. **Synced-folder cold migration**: put `bots.yaml + mcp.yaml + data/` on Syncthing/iCloud (Syncthing itself syncs P2P across networks); run the engine on whichever device you're at — the engine lock guarantees only one runs at a time.

⚠️ Do not expose port 8973 to the public internet directly (plain HTTP + a pairing code is not suitable there); always go through one of the encrypted channels above.

### Have a server? Direct public-internet connection

Run the engine on your own VPS (cross-compile or `go build` on the server first), and connect clients from any network:

```bash
# Option 1: built-in TLS (no domain needed) — self-signed cert + client-side fingerprint pinning (TOFU)
./botbureau-backend -port 8973 -tls auto -config bots.yaml -mcp mcp.yaml -data data
# The startup log prints the certificate's SHA-256 fingerprint; the client remembers it on the
# first connection to https://<server-ip>:8973, and any later fingerprint change
# (a possible man-in-the-middle) is rejected with a warning

# Option 2: with a domain → reverse proxy with a real certificate (Caddy, automatic HTTPS in one line)
caddy reverse-proxy --from botbureau.example.com --to localhost:8973
# Clients connect to https://botbureau.example.com

# Bring-your-own certificates are also supported: -tls cert.pem:key.pem
```

On the client: Settings → "Connect to remote engine", enter `https://...` plus the pairing code. Security model: TLS-encrypted channel + pairing-code auth + fingerprint pinning against MITM; plain HTTP is for trusted LANs / virtual networks only.

### Telegram integration (access your team from your phone)

1. In Telegram, find **@BotFather** → `/newbot` to create a bot and get its token;
2. In Bot Bureau Settings → save it as `TELEGRAM_BOT_TOKEN` in the API keys section → click "Enable" on the Telegram bridge;
3. Send `/start` to your bot in Telegram — **the first sender gets exclusive binding**; anyone else is rejected.

Then, on your phone:

- **Choose what the conversation connects to**: `/bind group` (default) attaches it to the team group chat; `/bind scout` re-attaches it to a specific bot's DM — plain messages then go straight to that bot, and forwarding covers only the bound conversation (no cross-talk noise). `/bind` alone shows the current binding;
- In group mode `@scout …` mentions work as usual; `/dm coder <text>` sends a one-off message to a bot without changing the binding (its subsequent replies are forwarded too); `/bots` lists the team;
- **Command approvals arrive as inline ✅ approve / ❌ reject buttons** — one tap;
- **Quota alerts always get through**: if any provider's balance/quota runs out, you're notified regardless of the current binding.

The bridge uses the official Bot API with long polling — it works even when the engine sits behind a home NAT, and no public IP is required.

WeChat has no official bot API for personal accounts (third-party reverse-engineered options violate its ToS and get accounts banned), so it is not provided; Discord/Slack/Feishu can be added following the pattern in `backend/internal/bridge/telegram.go` (messages in via `bus.PostGroup`/`Deliver`, events out via `bus.EventsSinceCtx`).

### What happens if the engine goes offline? (availability)

With a single-engine architecture, if the engine is offline, the team is offline too (clients are just views). Three mitigations:

1. **Disconnect banner and quick switch**: after 3 failed reconnects the client shows an "engine offline" banner at the top and keeps retrying automatically; one click switches to the local engine (remote mode → independent local team) or restarts the local engine (if it crashed). When the engine comes back, the client recovers automatically.
2. **Keep the engine always on (recommended)**: the engine does not need Electron — run the bare backend on an always-on device (Mac mini / NAS / Raspberry Pi) and clients discover/connect as usual:
   ```bash
   ./app/bin/botbureau-backend -port 8973 -config bots.yaml -mcp mcp.yaml -data data
   ```
   Pair with `nohup` / `launchd` / `systemd` for boot autostart; for other platforms, `cd backend && go build` on the target machine.
3. **Takeover on another machine**: with the data directory on a synced folder (see above), once the engine lock expires (30 s) any device can "run locally" and take over all persistent state (members/plugins/memory/board/routines/keys). Conversation context only ever lives in engine memory and clears on restart — long-term information relies on the remember mechanism, a deliberate trade-off.

## Plugin (MCP) examples

```yaml
# mcp.yaml — everything here can also be managed from the plugins panel
servers:
  - name: fs                # local plugin: the official filesystem server
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/Users/you/Documents"]
  - name: linear            # remote connector
    url: https://mcp.linear.app/mcp
    bearer_key: LINEAR_TOKEN   # resolved from the key store / environment variables
```

Tools are exposed as `mcp_<plugin>_<tool>` to bots that subscribe to the plugin (`mcp: [fs, linear]` in bots.yaml).
Engine-level MCP (rather than the Anthropic API's server-side MCP connector) was chosen so that bots on every provider — Grok / DeepSeek / Ollama and the rest — share the same plugins; stdio local plugins are only possible at the engine level anyway.

A remote connector authenticates one of two ways: a **static token** (`bearer_key`, pointing at an entry in the key store) or **OAuth** (`auth: oauth`, started by hitting "Authorize" in the plugins panel). The OAuth path runs the whole chain: protected-resource metadata discovery, authorization-server discovery, dynamic client registration (RFC 7591), authorization code with PKCE, and automatic refresh. Connectors that issue no static token — Linear, Notion, Sentry — can only be reached this way.

When a plugin carries a lot of tools (the official GitHub MCP server has ninety-odd), click "Choose tools" in the panel to pick a subset; it becomes the `tools:` list in `mcp.yaml`. Leaving it empty means all of them, so tools added by later updates are picked up automatically.

**A local plugin does not inherit your full environment.** The process receives an allowlist (PATH/HOME, locale, proxy and certificate variables, and so on) plus whatever it declares under `env:`. Something installed by a single click from a panel should not receive `SSH_AUTH_SOCK` — enough to use your SSH keys against any host — or the tokens exported in your shell. **If a plugin needs some other variable, name it under `env:`**; a value starting with `$` is resolved from the key store.

**A local plugin that stops unexpectedly reconnects on its own.** When the process crashes, or a pipe breaks as the machine wakes, the engine marks it unavailable and reconnects with backoff (stopping for a manual retry after a few attempts). The dot used to stay green while the model kept calling a tool list that no longer existed. A server announcing `tools/list_changed` also refreshes automatically, with no reconnect needed.

## Skills (Agent Skills)

A skill writes down **how** a kind of work is done — a different thing from the "what can be done" a plugin supplies. One skill is one directory:

```
data/skills/release-notes/
  SKILL.md          # YAML frontmatter: name + description; the body is written for the model
  build.py          # optional scripts and material alongside it
```

```markdown
---
name: release-notes
description: Turn merged PRs into release notes in the house style. This line has to say when the skill applies.
---

1. Group changes by user-visible impact, not by module.
2. Lead each line with a verb.
```

**Two-stage loading** is the one design decision that matters here: the system prompt carries only a single `name: description` line per skill, and the model calls `read_skill` for the full text once it judges one to apply. Fifty installed skills therefore cost fifty lines of prompt rather than fifty documents. The `description` is the model's only basis for choosing — it has to state when the skill applies, which is the thing most easily missed when writing your own.

Skills are shared by the whole team rather than subscribed per bot (a one-line summary costs nothing, and which skill suits whom is already decided by description matching). Scripts a skill ships are run with bash by full path; that lies outside the workspace, so it goes through approval — which is how third-party code should be treated.

## Plugin packages (the Claude / Codex format)

**Bot Bureau does not invent its own plugin format; it installs `.claude-plugin/plugin.json` packages directly.** The reasoning is practical: a bespoke format would require developers to start from scratch, while an existing format lets them write a plugin once and Claude Code, Codex, and Bot Bureau can all install it. For a developer, then, the cost of "supporting Bot Bureau" is zero — write your Claude plugin as usual.

Install from the plugins panel with a git URL or a folder path on this machine, or just copy a directory into `data/plugins/` (the filesystem is the only source of truth; no separate index is kept).

**Marketplace repositories work too.** Plugins in the wild are often distributed not as "one repository, one plugin" but as "one repository, one marketplace listing several plugins" — those carry `.claude-plugin/marketplace.json` at the root. Across a sample of twenty real repositories **about half are of that kind**, so pasting a marketplace address lists what it contains and lets you pick one, instead of reporting "no plugin.json found". When an entry's `source` points at the repository root and that directory has no `plugin.json` of its own, the listing's name and description stand in — a spelling that does occur in practice.

Hit "Update" on an installed plugin to upgrade it in place: a git source is pulled, a marketplace one is re-fetched by its entry. **An upgrade reconciles rather than rebuilds** — newly shipped MCP servers are added and vanished ones removed, while existing ones are kept exactly as they are, along with the tool subset you picked and the authorization you completed.

### Support matrix — which fields take effect

| What the package ships | What Bot Bureau does with it |
|---|---|
| `mcpServers` (in the manifest) or a root `.mcp.json` | Registered as MCP plugins, scoped by the package name (`notes` inside the `acme` package becomes `acme_notes`). `${CLAUDE_PLUGIN_ROOT}` expands to the real installed path |
| `skills/` | Merged into the skill library, attributed to the package |
| `agents/*.md` | **Becomes a member template.** The frontmatter's name/description fill in the new-bot form, and the body lands in "detailed role instructions" (appended to the system prompt) |
| `commands/` | Not supported — Bot Bureau has no slash commands; ask the bot in chat instead |
| `hooks/` | Not supported — there is no hook system; the four permission tiers cover the safety side |

Whatever is not supported is **listed explicitly** after installing, rather than half the package quietly doing nothing.

That `agents/` row is Bot Bureau's own: the same package elsewhere can only degrade an agent into a subagent, while here it becomes an actual member — with its own workspace, memory, and an entry on the task board, able to be @-mentioned and assigned work.

## Will I be notified when quota runs out?

Yes, on two channels. When a model platform returns a "balance/quota exhausted" class of error (distinct from ordinary 429 rate limiting):

1. A ⚠️ error bubble appears immediately in the conversation where it happened (with the platform's original message);
2. A **global 💳 alert** is emitted at the same time: visible on every chat panel, and pushed through the Telegram bridge regardless of the current binding; each provider alerts at most once per ten minutes so routines/retries can't flood you.

Detection covers Anthropic (credit balance / billing) and OpenAI-compatible providers (insufficient_quota / exceeded your current quota / HTTP 402). Note: routines are not paused automatically when quota runs out — top up and things resume, or delete the routine.

## Implementation notes

- Claude goes through the official `anthropic-sdk-go` (beta endpoint): `claude-opus-5`, adaptive thinking, server-side `fallbacks:"default"` (a safety-classifier refusal is automatically continued by the recommended fallback model), whole-turn rollback on refusal, and automatic paired tool_result repair when `max_tokens` truncates.
- The OpenAI-compatible layer is native `net/http`, with two-way history/tool conversion; point `base_url` at any vendor.
- Recognized read-only shell commands and pipelines run directly, including non-acting `find`. Commands outside that subset, output redirects to real files, command substitutions, and acting `find` predicates such as `-delete` or `-exec` are handed to the permission gate. While waiting for approval the bot blocks — that is how "comes back to you when human judgment is needed" is actually implemented.
- Message history is append-only in `data/events.jsonl`, with only a recent tail cached in memory. Group and DM contexts are independent and persist as each bot's `data/workspaces/<bot>/sessions.json`; long contexts are trimmed at whole-turn boundaries. Ask bots to `remember` anything long-term.
- The bash "sandbox" is only a workspace directory + approval gate, **not** real isolation; read commands carefully before approving.
- Packaged distribution uses electron-builder (`npm run dist:mac:arm64`, `dist:win:x64`, and the other scripts above); `npm start` remains the development entry point.

## Future plans

The next user-facing improvements are:

- **Message notifications** — optional desktop and mobile alerts for new messages, approvals and other events that need your attention.
- **Native mobile apps (iOS / Android)** — support both an independent engine-device mode and a client mode. In engine-device mode, the phone or tablet runs the team locally and handles model calls, bash, plugins, chats, memory, the task board and approvals; in client mode, it connects to an engine running on another device.
- **watchOS companion** — show task progress, receive approval pushes and approve or reject work from the wrist. watchOS remains a client and does not run the engine.

| Platform | Form | Status |
|---|---|---|
| macOS / Windows / Linux | Electron, engine embedded | ✅ shipped |
| Telegram | Bridge; works on anything with Telegram | ✅ shipped (see above) |
| iOS / Android | Native app that can host an engine independently or connect to another engine | Planned |
| watchOS | Companion client for progress and approvals | Planned |

Until the native mobile apps are available, enable the Telegram bridge above to chat with the team and handle approvals from a phone.
