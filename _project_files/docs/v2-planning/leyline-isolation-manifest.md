# Leyline Isolation Manifest (v2)

This document catalogues the current tight coupling between `mache` and the `leyline` daemon.
As part of the v2 workspace microkernel refactor, these "novelty" components will be broken apart
and isolated so that the core `mache` graph and schemas do not depend on them.

## 1. Daemon Discovery & IPC

**Location:** `internal/leyline/socket.go`
**Impact:** Mache attempts to discover or auto-spawn the `leyline` daemon via Unix Domain Sockets (`DiscoverOrStart`, `DialSocket`).
**Current Usage:** `cmd/serve.go` (LSP tables), `cmd/mount.go` (Embedding Trigger), `cmd/serve_handlers.go` (Semantic Search, Sheaf).

## 2. LSP Enrichment (Hover & Diagnostics)

**Location:** `cmd/serve_lsp.go`
**Impact:** The `mcp_mache_get_type_info` and `mcp_mache_get_diagnostics` tools depend on `leyline lsp` to populate `_lsp` and `_lsp_hover` tables in the SQLite database. If missing, it uses the UDS client to trigger the daemon to fetch them.

## 3. Semantic Search & Embeddings

**Locations:**

- `internal/leyline/semantic.go` (SemanticClient)
- `internal/leyline/trigger.go` (TriggerEmbedding)
- `cmd/serve_handlers.go` (Registers `semantic_search` MCP tool)
- `cmd/serve_registry.go` (Fires async embedding job on startup)
  **Impact:** Mache relies on the leyline daemon to perform MiniLM embeddings and nearest neighbor searches (`sqlite-vec`). The trigger runs in the background of `mache serve`.

## 4. Sheaf Theory & Topological Caching

**Locations:**

- `internal/leyline/sheaf.go`
- `cmd/serve_handlers.go` (SheafClient)
  **Impact:** Advanced caching and topological reasoning (Sheaf operations) are offloaded to the rust daemon.

## 5. Virtual File Materialization

**Location:** `cmd/leyline.go`
**Impact:** The `mache leyline` command exists solely to materialize virtual nodes into the `.db` so that the Rust daemon can read them.

## 6. FFI & CGO Hooks

**Location:** `internal/leyline/client.go`
**Impact:** Contains `import "C"` bindings. While currently somewhat contained, it is part of the `internal/leyline` package that is compiled into the main `mache` binary.

## Refactor Strategy (v2)

1. **Quarantine:** All `internal/leyline` code will be moved into `pkg/adapters/leyline-client`.
1. **Remove Auto-Spawn:** Mache core commands (`serve`, `mount`) will no longer attempt to `DiscoverOrStart` the daemon automatically.
1. **Plugin Registration:** Leyline capabilities (Semantic Search, LSP auto-enrichment) will be registered as optional plugins in the MCP server rather than hardcoded into `cmd/serve_handlers.go`.

## 7. The Leyline IR Contract (Public vs Sovereign)

**Context:** The `ley-line` rust repository has been split into two tiers:

- `rs/ll/`: The "Data Plane" (Public Utility/AGPL-ready). Contains fs, net, lsp, sign, ts, vcs, cli.
- `rs/ll-core/`: The "Secret Sauce" (Sovereign/Private). Contains core, embed, infer, schema, sheaf.

**The Mache Contract:**
Mache will **only** interface with `rs/ll/schema`. This new public crate acts as the Intermediate Representation (IR) via Protobuf/Flatbuffers. The old, tight coupling where `mache` relied directly on `ll-core` definitions (or sent raw SQL strings to the daemon) is deprecated. All IPC between `mache` (Go Workspace) and `ley-line` (Rust Daemon) will be typed via this new `ll/schema` IR.
