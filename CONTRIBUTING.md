# Contributing to Bot Bureau

Thanks for helping improve Bot Bureau. The project is intentionally split into a Go engine and an Electron client; keep that process boundary stable unless a change clearly requires otherwise.

## Before opening a pull request

1. Read [the architecture notes](docs/architecture.md) and [the security policy](SECURITY.md).
2. Keep runtime data, API keys, OAuth tokens, `bots.yaml`, `mcp.yaml`, and coverage output out of commits.
3. Run the relevant checks from the repository root:

   ```bash
   make test
   make check
   ```

   For a desktop smoke test, run `make test-e2e` on a desktop-capable machine.

4. Keep user-facing source strings in English and add translations through the existing locale tables.
5. Prefer small, behavior-preserving commits for refactors. Include a test for a bug fix or new boundary.

## Code areas

- `backend/internal/engine/`: resident workers, event bus, tools, memory, tasks, and routines.
- `backend/internal/api/`: HTTP/SSE transport and request-to-engine wiring.
- `backend/internal/model/`: model provider implementations and model catalogs.
- `backend/internal/plugin/`: MCP transports, bundles, and installed-plugin management.
- `app/renderer/`: the native DOM client.

Please do not include real provider credentials, private workspaces, chat history, or third-party secrets in issues or pull requests.
