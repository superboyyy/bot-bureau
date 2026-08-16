# Plugin development

Bot Bureau supports MCP servers over local stdio and remote Streamable HTTP. A plugin may expose tools, resources, authentication flows, or scripts that execute outside the engine's own Go process.

## Trust model

Installing a plugin is an explicit trust decision. Local plugins can start commands on the host; remote connectors can send data to an external service. Read-only hints from an MCP server are advisory metadata, not a security proof. Non-read-only calls remain subject to the engine's approval policy.

## Integration points

- Connector configuration is persisted in `mcp.yaml`.
- Credentials belong in the key store or OAuth store under `data/`, not in source or plugin manifests.
- Plugin tools are normalized by the engine and exposed to bots with an `mcp_<plugin>_<tool>` name.
- Changes to the plugin catalog should emit an engine refresh so connected clients update their tool lists.

When adding a transport or bundle format, add tests for malformed input, reconnect behavior, credential handling, and removal of stale configuration.
