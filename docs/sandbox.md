# OS sandbox

Approval (`ask` / `edit` / `auto` / `full`) decides whether a human is asked. The OS sandbox decides what a `bash` process can touch even when nobody is asked, and even when `shellscan.go` misreads the line. They are two knobs, the same split Codex and Claude Code use. This is not a rewrite of the scanner.

File tools stay on `resolve` / `inBounds`. MCP processes are not sandboxed in this version.

## Knobs

| Setting | Default | Meaning |
|---|---|---|
| `sandbox.enabled` | on | Wrap `bash` in the OS backend when one exists |
| `sandbox.auto_allow_bash` | off | If the command will run inside the sandbox, skip the bash approval prompt (Claude's auto-allow). The permission tier still gates file tools and plugins |
| `sandbox.allow_unsandboxed` | on | Allow `bash` with `unsandboxed=true`, which always needs approval except under `full` |

`full` never sandboxes: it stays "this machine, any command".

When `enabled` is on but the OS backend is missing, commands run on the host and Settings says so. There is no pretend isolation.

## What a sandboxed command can do

- **Read**: the filesystem, so interpreters and `/usr/bin` work. Secrets under `~/.ssh` are still readable until a later credentials pass.
- **Write**: the member's workspace, granted roots, `/tmp`, and a session temp directory (`$TMPDIR`).
- **`.git`**: a `.git` directory that sits directly in a writable root is re-mounted read-only (bubblewrap / Seatbelt). Landlock cannot carve a deny out of an allowed parent, so that backend leaves `.git` writable.
- **Network**: denied on bubblewrap and Seatbelt. Landlock fallback is filesystem-only; Settings reports `network: false`. `fetch_url` / `web_search` stay in-process and keep the fetch host allowlist. A domain-allowlisting proxy for bash is later work, not this pass.

The engine always tries the sandbox first. A denial is returned to the model with the hint to retry `unsandboxed=true`. That retry is the escape hatch (Claude's `dangerouslyDisableSandbox`), not `shellscan` guessing `Escapes` in advance.

## Backends

1. **macOS** — `sandbox-exec` (Seatbelt).
2. **Linux** — `bwrap` if it is installed and a probe spawn succeeds (needs user namespaces; Ubuntu 24.04 may need an AppArmor profile for `bwrap`). Otherwise Landlock via a re-exec of the engine binary (write restriction only).
3. **Windows** — none. Native Claude Code takes the same position (WSL). Commands run on the host; the UI says isolation is unavailable.

## Approval mapping

| Tier | File writes | Bash (sandboxed) | `unsandboxed=true` |
|---|---|---|---|
| `ask` | ask | ask, unless auto-allow is on | ask |
| `edit` | skip in workspace | ask, unless auto-allow is on | ask |
| `auto` | skip in workspace | skip (kernel holds the line) | ask |
| `full` | skip | host, skip | skip |

`shellscan.go` still classifies read-only commands and still fills the approval card. It is not the containment boundary once a backend is active.

## Audit

Each bash line in `data/audit.jsonl` includes `isolate`: `workspace` or `host`.
