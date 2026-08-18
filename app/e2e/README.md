# Electron E2E tests

The smoke test launches the actual Electron client and its Go backend. It is skipped by default so a
normal unit-test run does not open a desktop window.

```bash
npm run build:backend
BOTBUREAU_RUN_E2E=1 npm run test:e2e
```

On Linux without a desktop session, wrap the e2e command in `xvfb-run --auto-servernum`. From the
repository root, `make test-e2e` builds the backend and runs the same suite.

The tests launch the real Electron window against a temporary `BOTBUREAU_DATA_DIR`, start the backend in
local-only mode, and never call a real model or external connector. They cover first paint (`#app`,
New) and the chat-pane approval card, including the unified diff added for file edits. CI should run
them on a desktop-capable runner (or under `xvfb-run` on Linux).
