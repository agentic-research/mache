# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

All commands use [Task](https://taskfile.dev) — it sets required CGO flags automatically.

```bash
task build          # Build binary + codesign (macOS)
task test           # Run all tests (go test -v ./...)
task lint           # golangci-lint run ./...
task fmt            # gofumpt -w -extra .
task vet            # go vet ./...
task check          # fmt + vet + lint + test + validate (full CI-equivalent)
task validate       # SQLite ingestion tests only
task test-go-schema # Self-hosting smoke test (ingests mache's own source)
task tidy           # go mod tidy
```

**Do not use `go test` directly on macOS** — CGO flags for tree-sitter are required and only set by `task`.

Run a single test: `task test -- -run TestName` or set the env vars from Taskfile.yml and use `go test -v -run TestName ./internal/graph/`.

Integration tests require real SQLite databases and are gated behind env vars (`MACHE_TEST_KEV_DB`, `MACHE_TEST_NVD_DB`) — they skip automatically when unset.

## Architecture

Mache projects structured data (JSON, SQLite, source code) as a filesystem driven by declarative JSON schemas.

### Three Presentation Paths

```
.db files  →  SQLiteGraph (zero-copy, lazy scan, direct SQL)  →  GraphFS (NFS) or MCP
other files →  Engine (walkers + loaders)  →  MemoryStore       →  GraphFS (NFS) or MCP
any source →  Engine → MemoryStore/SQLiteGraph                 →  MCP (Streamable HTTP or stdio)
```

The `.db` path (SQLiteGraph + internal/template) is **pure Go** — no CGO, no tree-sitter. The source code path (Engine + SitterWalker) requires CGO for tree-sitter grammars. When ley-line pre-parses source into `.db` files with `_ast` tables, ASTWalker replaces SitterWalker and the entire pipeline is pure Go.

The `mache serve` command exposes the graph as MCP tools without mounting a filesystem.
Two transports: Streamable HTTP (`--http :PORT`, default, always-on) and stdio (`--stdio`, subprocess mode).
HTTP mode uses stateful sessions — each client gets its own session ID mapped to the projected graph.
Prefer HTTP over stdio to share one daemon across all sessions (avoids per-client FD leaks).

The mount wiring in `cmd/mount.go` selects the data path based on file extension. NFS is the only mount backend (FUSE was removed in v0.7.0 — see ADR-0006). For FUSE mounts, use ley-line-open's `leyline serve`.

### Core Abstractions

| Concept           | Location                                                              | Role                                                                                                                                                                                                                                   |
| ----------------- | --------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Schema types      | `api/schema.go`                                                       | `Topology` → `Node` (dirs) → `Leaf` (files) — declarative tree definition                                                                                                                                                              |
| Graph interface   | `internal/graph/graph.go`                                             | `GetNode`, `ListChildren`, `ReadContent`, `GetCallers` — backend-agnostic                                                                                                                                                              |
| MemoryStore       | `internal/graph/graph.go`                                             | Map-based graph with RWMutex + FIFO content cache (1024 entries)                                                                                                                                                                       |
| SQLiteGraph       | `internal/graph/sqlite_graph.go`                                      | Direct SQL backend: `compileLevels()` builds schema tree, `scanRoot()` streams all records using `json_extract()` in SQL, content resolved on-demand via PK lookup + template render                                                   |
| Language registry | `internal/lang/lang.go`                                               | Single source of truth for all 28 supported languages — extensions, grammars, presets, sentinel files. Every consumer derives from `lang.Registry`                                                                                     |
| Engine            | `internal/ingest/engine.go`                                           | Dispatches by extension via `lang.ForExt()`, recursive schema traversal, dedup via `dedupSuffix()`                                                                                                                                     |
| Walkers           | `internal/ingest/json_walker.go`, `sitter_walker.go`, `ast_walker.go` | JSONPath (ojg), tree-sitter AST query (CGO), and SQL-backed AST query (pure Go) — all implement `Walker`/`Match` interfaces from `interfaces.go`. `SelectWalker` auto-selects ASTWalker vs SitterWalker based on `_ast` table presence |
| Template renderer | `internal/template/render.go`                                         | Pure Go template rendering with mache func map (`json`, `first`, `slice`, `dig`, `dict`, etc.). Extracted from engine.go to break the graph→ingest→tree-sitter CGO chain                                                               |
| SQLiteResolver    | `internal/graph/sqlite_resolver.go`                                   | Resolves `ContentRef` entries by fetching records from SQLite and re-rendering via injected `TemplateRenderer` func                                                                                                                    |
| GraphFS           | `internal/nfsmount/graphfs.go`                                        | NFS backend via go-nfs/billy, `callers/` virtual dir, write-back support                                                                                                                                                               |
| NodesTableReader  | `internal/graph/nodes_table_reader.go`                                | Shared SQL queries for nodes-table schema (GetNode, ListChildren, ReadContent, GetCallers). Used by SQLiteGraph fast path and WritableGraph.                                                                                           |
| Splice            | `internal/writeback/splice.go`                                        | Atomic byte-range replacement in source files for write-back                                                                                                                                                                           |

### Key Design Details

- **SQLiteGraph scan**: Single-pass streaming scan pushes field extraction into SQLite via `json_extract()`, builds directory tree (paths only) in `sync.Map`. Content is never bulk-loaded — resolved on-demand per file read.
- **Template rendering**: `Render()` in `internal/template/render.go` (pure Go, no CGO). `engine.go` re-exports as `RenderTemplate()` for backward compat. Funcs: `json`, `first`, `slice`, `dig`, `dict`, `lookup`, `default`, `replace`, `lower`, `upper`, `title`, `split`, `join`, `unquote`, `hasPrefix`, `hasSuffix`, `trimPrefix`, `trimSuffix`.
- **ContentRef**: Large content (>4KB) uses lazy `ContentRef` with DBPath/RecordID/Template instead of inline bytes.
- **Write-back pipeline**: validate (tree-sitter) → format (gofumpt for Go, hclwrite for HCL/Terraform) → splice → surgical node update + `ShiftOrigins`. No re-ingest.
- **Draft mode**: Invalid writes save as drafts; node path stays stable. Errors via `_diagnostics/ast-errors`.
- **Virtual dirs**: `_schema.json` (root), `_diagnostics/` (writable), `context` (per-dir), `.query/` (SQL → symlinks), `callers/` (cross-refs, self-gating).
- **NFS mount**: Only mount backend (FUSE removed in v0.7.0). Pure Go via `go-nfs`.
- **LSP enrichment**: When a `.db` has `_lsp*` tables (produced by ley-line's `ll-open/lsp` crate), `find_definition` falls back to `_lsp_defs` and `find_callers` supplements with `_lsp_refs`. `get_type_info` reads `_lsp_hover`, `get_diagnostics` reads `_lsp`. No runtime daemon needed — all pre-baked at build time by ley-line.

### Language Registry (`internal/lang`)

All supported languages are defined in a single `Registry` slice in `internal/lang/lang.go`. Adding a new language means adding ONE entry — extensions, grammar, preset schema, sentinel files, and display name. Derived lookups (`ForExt`, `ForName`, `ForPath`, `IsSourceExt`, `Extensions`) are built at init time. All consumers (engine, watcher, writeback validation, schema presets, project detection, mount infer) use these lookups — no hardcoded switch statements elsewhere.

### Example Schemas

`examples/` contains three schemas showing the pattern: `nvd-schema.json` (temporal sharding by year/month), `kev-schema.json` (flat), `go-schema.json` (tree-sitter queries for Go source).

## Conventions

- **Formatter**: gofumpt (stricter than gofmt). Pre-commit hooks enforce it.
- **Test framework**: `github.com/stretchr/testify` (assert/require).
- **Pure-Go SQLite**: Uses `modernc.org/sqlite` (no CGO dependency for SQLite).
- **CI**: GitHub Actions runs `go test -race ./...` and `golangci-lint` on ubuntu.
