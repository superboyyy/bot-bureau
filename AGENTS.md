# AGENTS.md

## Cursor Cloud specific instructions

Bot Bureau is a single product built from two tightly-coupled parts in this repo:

- `backend/` — Go engine (HTTP + SSE server on `127.0.0.1`/LAN). No database; state is flat files.
- `app/` — Electron desktop client. Its main process compiles and spawns the Go backend automatically, so you never start the backend separately when running the app.

Standard build/test/run commands are documented already; use them rather than re-deriving:
- Root `Makefile` (`make test`, `make check`, `make test-e2e`, `make build`).
- `app/package.json` scripts (`npm start`, `npm run test:unit`, `npm run build:backend`, `npm run test:e2e`).
- `README.md`, `docs/development.md`, and `.github/workflows/ci.yml`.

Non-obvious caveats for this environment:

- Go toolchain: `backend/go.mod` requires Go >= 1.26.6. The distro's default `go` (1.22) is too old. Go 1.26.6 is installed at `/usr/local/go` (symlinked into `/usr/local/bin/go`) and is baked into the environment snapshot, so the update script does not reinstall it. If `go version` ever reports < 1.26.6, the snapshot/PATH is wrong.
- Running the desktop app: it needs a display. A VNC X server runs on `DISPLAY=:1`. Launch with `DISPLAY=:1 npm start` from `app/`. On launch you will see harmless `dbus/bus.cc ... Failed to connect to the bus` errors (no system dbus in the container) — ignore them.
- Electron E2E needs a headless display and must be gated by an env flag: `xvfb-run --auto-servernum env BOTBUREAU_RUN_E2E=1 npm run test:e2e` (or `make test-e2e`). It uses a temp data dir and never contacts real models/OAuth/MCP/Telegram.
- Testing chat end-to-end WITHOUT any API key or network: use the built-in `fake` provider (an offline echo model). Configure a bot with `provider: fake` and `model: fake`, then send it a message and it replies with `(fake model echo) <your text>`. This is the reliable way to demo/verify the renderer → backend (HTTP/SSE) → engine pipeline without credentials.
- Dev runtime data lives in the repo root during `npm start` (`bots.yaml`, `mcp.yaml`, `data/`, `app/connect.json`) and is all gitignored. Set `BOTBUREAU_DATA_DIR` to relocate it. Real model providers (Anthropic/OpenAI-compatible), MCP servers, and the Telegram bridge are optional and only needed for real (non-fake) work.
