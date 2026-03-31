# Mache v2 Decoupling Architecture

## Problem

Mache is a single binary that compiles everything: tree-sitter grammars (CGO),
FUSE (CGO on macOS), ley-line FFI (CGO, optional), NFS, MCP server, schema
engine, FCA inference. Users who only need the MCP server + SQLiteGraph for
JSON/SQLite data still pay the CGO compile cost and the binary includes unused
tree-sitter grammars.

Parallel context: ley-line is refactoring its crate split (`rs/ll/` public vs
`rs/ll-core/` private). LSP, tree-sitter, and net are public tier. Arena,
sheaf, embeddings are sovereign. The mache contract will interface only via
`rs/ll/schema` (protobuf IR) over UDS — no C FFI required.

## Current Dependency Map

```
cmd/serve*.go ──→ internal/ingest ──→ internal/lang ──→ 18 tree-sitter grammars (CGO)
     │                  │                                        │
     │                  └──→ internal/graph (pure Go)            │
     │                                                           │
     ├──→ internal/leyline (UDS: pure Go │ FFI: build-tagged)    │
     │                                                           │
cmd/mount.go ──→ internal/fs ──→ cgofuse (CGO, macOS fuse-t)    │
     │           internal/nfsmount ──→ go-nfs/billy (pure Go)   │
     │                                                           │
     └──→ internal/ingest ──────────────────────────────────────┘
```

### CGO Sources (3 roots)

| Source       | Package                                                              | Build Tag              | Notes               |
| ------------ | -------------------------------------------------------------------- | ---------------------- | ------------------- |
| tree-sitter  | `internal/lang` → 18 grammar packages + `internal/treesitter/elixir` | none (always compiled) | Biggest CGO surface |
| FUSE/fuse-t  | `internal/fs/cgo_darwin.go` + `cgofuse`                              | `darwin` (OS-gated)    | macOS only          |
| ley-line FFI | `internal/leyline/client.go`                                         | `leyline` (opt-in)     | Already isolated    |

### Key Coupling Points

1. **`cmd/serve.go` → `internal/ingest`** — uses `ingest.RenderTemplate`,
   `ingest.NewEngine`, `ingest.NewSQLiteResolver`, `ingest.NewWatcher`.
   This pulls ALL of `internal/lang` (18 grammars) into the MCP server.

1. **`cmd/serve_handlers.go` → `internal/leyline`** — imports for
   `semantic_search` + sheaf MCP tools. Uses UDS socket client (pure Go),
   NOT the FFI. But the package-level import still links `client.go` when
   building with `-tags leyline`.

1. **`internal/graph/sqlite_graph.go`** — accepts `TemplateRenderer` func as
   injection. Does NOT import `internal/ingest` directly. **Already decoupled.**

1. **`RenderTemplate`** — pure Go (`text/template` + custom funcs). Lives in
   `internal/ingest/engine.go` but has zero tree-sitter deps. Trapped in the
   wrong package.

1. **`internal/vfs`** — shared virtual filesystem handlers used by both FUSE
   and NFS. Depends only on `internal/graph`. **Already decoupled.**

## Proposed Layer Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ Layer 0: Core (pure Go, CGO_ENABLED=0)                      │
│                                                              │
│  api/              Schema types (Topology, Node, Leaf)       │
│  internal/graph/   Graph interface, MemoryStore, SQLiteGraph │
│  internal/template/  RenderTemplate (extracted from ingest)  │
│  internal/vfs/     Virtual dir handlers (callers, callees…)  │
│  internal/control/ Control plane                             │
│  internal/refsvtab/ SQLite virtual tables                    │
│  internal/materialize/ SQLite → BoltDB (build-tagged)        │
│                                                              │
│  MCP server (serve.go for .db-only mode)                     │
│  NFS mount (nfsmount/)                                       │
└──────────────────────────┬──────────────────────────────────┘
                           │ optional, additive
┌──────────────────────────▼──────────────────────────────────┐
│ Layer 1: Source Intelligence (CGO: tree-sitter)              │
│                                                              │
│  internal/lang/         Language registry (18 grammars)      │
│  internal/ingest/       Engine, walkers, watcher             │
│  internal/writeback/    Splice + validate                    │
│  internal/linter/       AST linting                          │
│  internal/lattice/      FCA schema inference                 │
│                                                              │
│  Source code mounting, live re-ingestion, write-back          │
└──────────────────────────┬──────────────────────────────────┘
                           │ optional, additive
┌──────────────────────────▼──────────────────────────────────┐
│ Layer 2: Platform (CGO: FUSE | optional: ley-line UDS)       │
│                                                              │
│  internal/fs/           FUSE backend (macOS fuse-t)          │
│  internal/leyline/      UDS client (semantic, sheaf, LSP)    │
│                         FFI client (build-tagged, optional)  │
│                                                              │
│  FUSE mounting, ley-line daemon integration                   │
└─────────────────────────────────────────────────────────────┘
```

## Refactor Steps

### Step 1: Extract `internal/template/` from `internal/ingest/`

Move `RenderTemplate`, `RenderTemplateWithFuncs`, `tmplFuncs`, `tmplCache`
from `engine.go` into `internal/template/render.go`. This is pure Go — no
tree-sitter, no CGO.

**Why:** `SQLiteGraph` needs `TemplateRenderer` but currently the only
implementation lives in `internal/ingest` which transitively imports 18
tree-sitter grammars. Extracting it breaks the chain.

**Files:**

- Create: `internal/template/render.go`
- Modify: `internal/ingest/engine.go` — re-export or alias
- Modify: `internal/graph/sqlite_graph.go` — already uses func injection, no change needed
- Modify: `cmd/serve.go` — import `internal/template` instead of `ingest.RenderTemplate`

### Step 2: Split `cmd/serve.go` into two data paths

The MCP server currently creates BOTH `SQLiteGraph` (for .db) and `Engine`
(for source code) in the same function. Split:

- `serve_sqlite.go` — .db data path: `graph.OpenSQLiteGraph` + `template.RenderTemplate`
  - `SQLiteResolver`. **No ingest import, no tree-sitter.**
- `serve_source.go` — source code path: `ingest.NewEngine` + `ingest.NewWatcher`
  - `internal/lang`. Build-tagged or runtime-gated.

**Why:** This is the main decoupling. After this, `mache serve --db foo.db`
compiles and runs without CGO.

### Step 3: Gate ley-line MCP tools

Move `semantic_search` and `sheaf` tool registration out of
`serve_handlers.go` into `serve_leyline.go` with a build tag or runtime check
(`leyline.Available()`).

**Files:**

- Create: `cmd/serve_leyline.go` (or use runtime `leyline.DiscoverOrStart` errors)
- Modify: `cmd/serve_handlers.go` — remove leyline import

**Alternative:** Since the UDS socket client is pure Go, the leyline package
compiles without CGO when the `leyline` build tag is absent. The `client.go`
FFI file is already tagged. So the real question is: do we want the MCP
server to even attempt ley-line discovery when running lightweight? Probably
not — make it opt-in via `--leyline` flag or auto-discover only when daemon
is running.

### Step 4: Gate FUSE behind build tag

FUSE (`internal/fs/`) is already darwin-gated for CGO. Add a `fuse` build tag
so Linux builds can also exclude it. NFS (`internal/nfsmount/`) is pure Go
and stays in Layer 0.

**Files:**

- Modify: `cmd/mount.go` — split FUSE vs NFS backend selection
- Modify: `internal/fs/*.go` — add `//go:build fuse` constraint

### Step 5: Verify CGO_ENABLED=0 build

After steps 1-4, verify:

```bash
CGO_ENABLED=0 go build -tags 'nofuse' -o mache-lite ./cmd/...
```

This should produce a static binary with: MCP server, NFS mount, SQLiteGraph,
schema types — but no tree-sitter, no FUSE, no ley-line.

## What This Enables

| Build              | CGO | Features                                      | Use Case                           |
| ------------------ | --- | --------------------------------------------- | ---------------------------------- |
| `mache` (full)     | yes | All: source parsing, FUSE, NFS, MCP, ley-line | Developer workstation              |
| `mache-lite`       | no  | SQLiteGraph, NFS, MCP server                  | CI/CD, containers, data projection |
| `mache serve --db` | no  | MCP server only                               | MCP tool for agents                |

## Ley-line Alignment — The Pure Go Path

**Revised design goal**: mache + ley-line should require **zero CGO**. CGO is only
needed for standalone mache (without ley-line) as a fallback.

### How ley-line eliminates CGO from mache

Ley-line's `rs/ll-open/ts` crate (public tier) does tree-sitter parsing in Rust
and projects results into SQLite `nodes` + `_source` + `_ast` tables — the same
schema that mache's `SQLiteGraph` already reads.

```
Source code parsing (ley-line present):
  source file → UDS request → leyline-ts parse_with_source()
  → serialized SQLite bytes (nodes + _source + _ast tables)
  → mache SQLiteGraph.Open() → MCP tools

Source code parsing (standalone, no ley-line):
  source file → Go tree-sitter (CGO) → internal/ingest/sitter_walker
  → MemoryStore → MCP tools
```

The `Walker`/`Match` interfaces (`internal/ingest/interfaces.go`) are the seam:

- `SitterWalker` — current: Go tree-sitter CGO bindings
- `JSONWalker` — current: ojg JSONPath (pure Go)
- `LeylineWalker` — future: sends parse + query requests to ley-line over UDS

### What ley-line provides (no CGO needed in mache)

| Capability           | Ley-line crate      | Mache consumer                  | CGO? |
| -------------------- | ------------------- | ------------------------------- | ---- |
| Tree-sitter parsing  | `ll-open/ts`        | SQLiteGraph (reads nodes table) | No   |
| Bidirectional splice | `ll-open/ts/splice` | write-back pipeline             | No   |
| LSP enrichment       | `ll-open/lsp`       | `_lsp`/`_lsp_hover` tables      | No   |
| Semantic search      | `ll/embed`          | UDS socket client               | No   |
| Sheaf cache          | `ll/sheaf`          | UDS socket client               | No   |

### Target build matrix

| Build                      | CGO    | Ley-line | Features                            |
| -------------------------- | ------ | -------- | ----------------------------------- |
| `mache` (full standalone)  | yes    | optional | All: Go tree-sitter, FUSE, NFS, MCP |
| `mache` + `leyline` daemon | **no** | required | All: ley-line parsing, NFS, MCP     |
| `mache serve --db`         | **no** | optional | SQLiteGraph, MCP server only        |

### Implementation path

1. **Done**: Extract `internal/template`, move `SQLiteResolver` to graph, add `ASTWalker` (PR #143)
1. **Pending**: Split `cmd/serve.go` into `serve_sqlite.go` + `serve_source.go` (Step 2)
1. **Pending**: Gate ley-line MCP tools behind flag or runtime check (Step 3)
1. **Pending**: Gate FUSE behind `fuse` build tag (Step 4)
1. **Pending**: Verify `CGO_ENABLED=0` build (Step 5)
1. **Future**: Add `LeylineWalker` that delegates parsing to ley-line via UDS
1. **Future**: Build-tag `SitterWalker` + `internal/lang` behind `sitter` tag
1. **Future**: Delete `internal/leyline/client.go` (C FFI) — UDS-only interop
1. **Future**: mache ships as pure Go binary alongside leyline Rust binary (kiln)

## Non-Goals (This Branch)

- Implementing `LeylineWalker` — design only, impl is ley-line-side work too
- Full ley-line removal — just isolation/gating, not deletion
- API changes — all existing CLI behavior preserved
- New CLI commands — just build tag flexibility
