---
title: "ADR-0022: Streamable HTTP is the canonical MCP transport; stdio is an escape hatch"
status: Accepted
date: 2026-06-26
tags: [architecture, mcp, transport, http, stdio]
---

**Bead:** `mache-60dc86` (thread `mcp-transport-canonical/onboarding`)
**Relates to:** ADR-0006 (pure-go-mcp-first), ADR-0010 (hosted mache architecture)
**Breaking:** shipped in **v0.10.0**

## Context

`mache serve` grew two MCP transports — stdio (`--stdio`) and Streamable HTTP (`--http`, default `localhost:7532`). The default flipped from stdio to HTTP (HTTP shares one daemon across sessions and avoids per-client FD leaks), but the migration was never finished, leaving three loose ends that read as "mache MCP feels broken":

1. **Onboarding still wired stdio.** `mache init` registered a `type: stdio` client entry whose command was a bare `mache serve` — which now starts an *HTTP listener* and never speaks JSON-RPC on stdout. `mache serve` branches purely on the `--stdio` bool (`cmd/serve.go`), with no stdin/TTY autodetection, so the stdio handshake could not complete. The only setup that actually worked was the *manual* `claude mcp add --transport http …`, i.e. `mache init` was not the working path.
1. **No daemon lifecycle.** Nothing started or kept the HTTP daemon alive, so a registered `http://localhost:7532/mcp` returned `connection refused` until someone ran `mache serve` by hand in a spare terminal.
1. **Two registration paths, no canonical one** — and the "official" one (`init`) was the broken one.

The instinct "just get rid of stdio" is half-right: the mess is not that two transports exist, it is the half-finished migration. stdio is still the correct transport for CI, sandboxes, and headless/cron agents where no shared daemon should run (an interactively-authenticated HTTP daemon may be absent there).

The HTTP model is sound, not a hack: a single daemon resolves each client's project per session via the MCP **roots** protocol (`cmd/serve_registry.go::resolveSession` calls `ListRoots`, caching session → workspace-root), plus hosted (`?repo=`) and repo-clone modes. One daemon genuinely serves every project.

## Decision

**Streamable HTTP is the canonical MCP transport. stdio is demoted to an explicit escape hatch.** "Get rid of stdio" means remove it as a *path/default*, not as a *capability*.

### D.1 — Onboarding registers HTTP only

`mache init` registers the shared endpoint, never a stdio command:

- Claude Code CLI: `claude mcp add --scope user --transport http mache http://localhost:7532/mcp`.
- File-based clients (`.claude/mcp.json`, Cursor, Windsurf, Gemini, Zed, VS Code): `{ "type": "http", "url": "http://localhost:7532/mcp" }`.

The mache binary path is no longer part of any client entry — the daemon is shared, not spawned per client.

### D.2 — `mache init --global` installs a keepalive supervisor

So the registered endpoint is answerable without anyone running `mache serve`:

- macOS: a `~/Library/LaunchAgents/com.agentic-research.mache.plist` LaunchAgent (`RunAtLoad`, `KeepAlive` on failure, `ThrottleInterval` to avoid crash-loops).
- Linux: a `~/.config/systemd/user/mache.service` unit (`Restart=on-failure`).

Both run `mache serve --http localhost:7532`. Install is best-effort and never fails `init`; the supervisor *load* step is test-overridable (`daemonAgentAutoload`) so unit tests exercise file generation without a real side effect.

This is the achievable slice of the lifecycle work. Full **on-demand socket activation** (spawn only on first connect) and the daemon-reliability hardening (stale `~/.mache` state, singleton-socket clobber) remain in `mache-605d08` / `mache-823d91`.

### D.3 — `--stdio` stays, demoted

The flag is unchanged in behavior but reframed in `serve --help`, README, and ARCHITECTURE as the CI/sandbox/headless escape hatch. It is never emitted by any registration path. `server.json` still advertises stdio as an available transport because it remains a real capability.

## Consequences

- **Breaking (v0.10.0):** existing stdio-based mache registrations stop matching how `mache init` writes config; re-run `mache init --global`. Anyone who pointed a client at `mache serve` as a stdio command must switch to the HTTP endpoint (or pass `--stdio` explicitly for a genuine subprocess use).
- One canonical onboarding path; `init` ↔ `serve` defaults no longer contradict.
- mache is now a managed per-user daemon on the dev box, matching the "one shared daemon" intent of ADR-0010.

## Not decided here

On-demand socket activation and the `~/.mache` daemon-reliability rot (`mache-823d91`) are tracked separately in decade `mcp-transport-canonical`. This ADR ships the canonicalization + a keepalive supervisor, not the full on-demand lifecycle.
