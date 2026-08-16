# Electron E2E tests

The smoke test launches the actual Electron client and its Go backend. It is skipped by default so a
normal unit-test run does not open a desktop window.

```bash
npm run build:backend
BOTBUREAU_RUN_E2E=1 npm run test:e2e
```

The test uses a temporary `BOTBUREAU_DATA_DIR`, starts the backend in local-only mode, and never calls a
real model or external connector. CI should run it on a desktop-capable runner (or under `xvfb-run` on
Linux).
