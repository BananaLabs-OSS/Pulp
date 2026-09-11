# AI control and MCP

Pulp's `aicontrol` package owns immutable proposals, evidence, policy,
approval binding, deployment, rollback, and audit. MCP is only one adapter.

`aicontrol/mcp` implements JSON-RPC MCP revision `2025-11-25` with
`initialize`, `tools/list`, `tools/call`, `resources/list`, and
`resources/read`. It exposes inspect, propose, validate, build/test, approve,
deploy, and rollback tools.

The host injects both authentication (`Authorizer`) and the control-plane
`Service`. Deploy and rollback require an approval token at the adapter, and
the service must resolve that token through `aicontrol` so the exact proposal,
evidence, policy, expiry, signature, and base revision are checked again.
Transport is also injected; stdio and network servers use the same JSON-RPC
handler. Do not put deployment credentials or policy decisions in an MCP
transport implementation.
