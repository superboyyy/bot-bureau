# Architecture

Bot Bureau is a local-first desktop application with one long-lived Go engine and one Electron client.

```text
Electron main process
  ├── starts or connects to the Go backend
  ├── discovers remote engines over mDNS
  └── exposes only engine-switch actions through preload

Electron renderer ── HTTP + SSE ── Go backend
                                      ├── api: HTTP/SSE transport
                                      ├── engine: workers, bus, tools, memory, tasks, routines
                                      ├── sandbox: OS isolation for bash (Seatbelt / bubblewrap / Landlock)
                                      ├── model: provider and session implementations
                                      ├── plugin: MCP transports and bundles
                                      ├── secret: key stores and OAuth
                                      ├── config/i18n: configuration and messages
                                      └── netx/bridge: networking and external chat adapters
```

## Source of truth

The device running the Go engine owns team configuration and runtime state. `bots.yaml` and `mcp.yaml` describe team and connector configuration; `data/` stores keys, tokens, event history, workspaces, memory, tasks, groups, and routines. Other clients are views over that engine and do not maintain a second synchronized copy.

## Process boundary

The renderer does not receive Node.js or filesystem access. It calls the backend over HTTP and consumes incremental events over SSE. The Electron main process owns the backend child process, packaged binary lookup, remote-engine selection, and certificate pinning.

## Refactoring rule

Keep the engine package cohesive: workers and the event bus deliberately share runtime state. Prefer splitting large files within the same package before introducing new package boundaries. Keep API handlers transport-focused and move repeated business workflows into services only when the behavior is genuinely shared.

The next engine work is the agent-runtime plan in [`docs/agent-runtime.md`](agent-runtime.md): file tools, context compaction, DM planning, two-stage memory, engine-side search, evals, and an audit log. OS isolation for `bash` is a separate knob from that plan; see [`docs/sandbox.md`](sandbox.md). It does not change this layout.
