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

## Default branch protection

`main` is protected by the repository ruleset in [`.github/rulesets/main.json`](.github/rulesets/main.json). Open a pull request instead of pushing to `main`. Merges wait until conversation threads are resolved and these GitHub Actions checks pass against the latest `main`:

- Go backend
- Electron unit tests
- Electron smoke test

Force-pushes and deleting `main` are blocked. Approving reviews are not required, so a solo maintainer can merge their own pull request after CI is green. Repository admins may bypass the rules only when merging a pull request.

Creating or updating the live ruleset needs repository admin access (cloud-agent tokens cannot change it):

```bash
./scripts/protect-main.sh
```

The same rules can be created in the GitHub UI under Settings → Rules → Rulesets.

## Code areas

- `backend/internal/engine/`: resident workers, event bus, tools, memory, tasks, and routines.
- `backend/internal/api/`: HTTP/SSE transport and request-to-engine wiring.
- `backend/internal/model/`: model provider implementations and model catalogs.
- `backend/internal/plugin/`: MCP transports, bundles, and installed-plugin management.
- `app/renderer/`: the native DOM client.

Please do not include real provider credentials, private workspaces, chat history, or third-party secrets in issues or pull requests.
