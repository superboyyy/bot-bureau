# Development

## Requirements

- Node.js 22.12 or newer
- Go 1.26.6 or newer
- A desktop-capable environment for Electron E2E tests

## Common commands

```bash
make test          # Go tests and Electron unit tests
make check         # tests plus go vet
make test-e2e      # build the backend and run the Electron smoke test
make build         # build the backend binary into app/bin/
```

The backend can also be tested directly with `cd backend && go test ./...`. The Electron client lives under `app/`; run `npm ci` there before the first frontend test.

## Default branch

Changes land on `main` through pull requests. The ruleset in `.github/rulesets/main.json` requires the CI jobs in `.github/workflows/ci.yml` to pass and blocks force-pushes. Apply or update the live GitHub ruleset with `./scripts/protect-main.sh` (repository admin access required).

## Runtime data

Development defaults place `bots.yaml`, `mcp.yaml`, `data/`, and `connect.json` beside the source tree. They are private runtime state and must remain ignored. Set `BOTBUREAU_DATA_DIR` to a temporary directory when running tests or experimenting with untrusted configurations.

## Refactoring

Refactors should preserve the HTTP/SSE contract and the Electron-to-engine process boundary. Make structural changes separately from behavior changes where possible, and run the affected package tests before moving to the next area.
