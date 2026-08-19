# Security policy

Bot Bureau runs an AI engine on the user's machine. It can execute shell commands, read and write files inside configured workspaces, call model providers, and start MCP plugins. Treat those capabilities as a local automation boundary. When an OS backend exists, `bash` is wrapped (writes stay in working directories and `/tmp`; network is blocked on bubblewrap and Seatbelt). That is still not a complete operating-system sandbox: reads may touch the rest of the machine, Landlock cannot block network, and MCP processes are not wrapped. See [`docs/sandbox.md`](docs/sandbox.md).

## Important boundaries

- Permission tiers and shell inspection are approval controls. The OS sandbox, when available, is the write boundary for `bash`; file tools still use path checks, and plugins are unsandboxed.
- MCP servers and installed plugin bundles are third-party code. Installing one may start local processes or give a bot access to an external service.
- The backend requires a pairing token even in local mode. Do not expose the plain HTTP port to the public internet.
- Use TLS or a trusted private network for remote connections.
- Runtime files under `data/` may contain keys, OAuth tokens, chat history, attachments, and workspaces. Never commit them.

## Reporting a vulnerability

Please report security issues privately through a GitHub Security Advisory for this repository. Include the affected version or commit, reproduction steps, impact, and any safe mitigation. Do not publish credentials, private workspace contents, or an exploit before a fix or coordinated disclosure has been agreed.
