---
title: "ADR-0001: User-Space FUSE Bridge (fuse-t)"
status: Superseded
date: 2026-02-10
tags: [architecture, fuse, macos, mount, superseded]
superseded_by: ADR-0006
---

> **Status note:** Superseded — FUSE was removed in v0.7.0; the pure-Go, NFS-only,
> MCP-first direction is set by [ADR-0006](0006-pure-go-mcp-first.md).

## Context

Running FUSE filesystems on macOS Silicon (M1/M2/M3) is difficult because:

1. Apple has deprecated Kernel Extensions (kexts).
1. `macFUSE` (the standard driver) requires disabling System Integrity Protection (SIP) or lowering security settings in Recovery Mode, which is unacceptable for a developer tool.
1. We need a solution that runs entirely in userspace without requiring "System Extension" approval.

## Decision

We will use **fuse-t** as the FUSE bridge.

- **Mechanism:** `fuse-t` exposes a local NFSv4 server. It translates FUSE calls (from our Go app) into NFS calls (consumed by the macOS Kernel).
- **Library:** We will use `winfsp/cgofuse` instead of `hanwen/go-fuse`.
  - `cgofuse` supports the `fuse-t` bridge natively.
  - `hanwen/go-fuse` is "Pure Go" and strictly requires `/dev/fuse`, which `fuse-t` does not provide.

## Consequences

- **Positive:** No kernel extensions required. Works on M3 Max out of the box. Zero security compromises.
- **Negative:** Requires `CGO_ENABLED=1`. The binary must be ad-hoc signed with `com.apple.security.network.client` and `com.apple.security.network.server` entitlements because the OS sees the mount as a "Network Drive" (see `entitlements.plist`).
- **Performance:** Slightly higher latency than raw kexts due to NFS overhead, but negligible for our "Semantic Projection" use case.
