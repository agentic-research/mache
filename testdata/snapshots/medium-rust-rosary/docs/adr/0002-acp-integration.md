# ADR-002: ACP Integration for Agent Dispatch

## Status
Accepted

## Context

Rosary shells out to `claude -p` for agent dispatch. This is opaque (no streaming),
creates orphan processes, requires CLI flag hacking for permissions, and doesn't
support cross-model review (gemini reviewing claude's work).

The `agent-client-protocol` Rust crate is already a dependency. `RosaryClient`
implements the ACP `Client` trait with permission approval logic.

## Decisions

### 1. Rosary is ACP client only, not server
Work intake stays via MCP/Dolt/Linear. ACP is exclusively for dispatching agents.

### 2. Dedicated tokio runtime per ACP session
The ACP `Client` trait is `!Send`. Bridge via `std::thread` with single-threaded
runtime + `LocalSet`. Reconciler communicates via `mpsc` channels (which ARE Send).

### 3. Streaming output via per-bead JSONL logs
`SessionNotification` updates written to `~/.rsry/sessions/{bead_id}.jsonl`.
`rsry logs --bead <id>` tails the specific session.

### 4. Two-tier dispatch: DispatchHandle enum
```rust
enum DispatchHandle {
    Cli(AgentHandle),      // claude -p, gemini -p
    Acp(AcpAgentHandle),   // ACP protocol
}
```

### 5. MCP servers configured in NewSessionRequest
Rosary passes mache + rsry as `McpServer::Stdio` in `session/new`. The
orchestrator owns the tool surface — agents don't configure their own MCP.

### 6. Permissions are protocol-native
`--allowedTools` CLI flag eliminated. ACP agent asks rosary "may I use Edit?",
rosary answers via `RosaryClient::request_permission()` based on bead's
`PermissionProfile`. Dynamic, not advisory.

## Implementation Phases

1. ACP Session Bridge (`acp.rs` — the `!Send` boundary)
2. ACP Agent Handle (`dispatch.rs` — `DispatchHandle` enum)
3. Reconciler Integration (`reconcile.rs` — active map type change)
4. Notification Streaming (`rsry logs --bead`)
5. Provider Unification (ACP becomes default, CLI becomes fallback)
6. Review Agent Migration (staging-agent via ACP)

## Consequences

- Agents become observable in real-time (streaming)
- Cross-model review is possible (dispatch gemini to review claude's work)
- No orphan processes (ACP session lifecycle is managed)
- Permission model is dynamic and enforceable
- ~7 days implementation, critical path is Phase 1
