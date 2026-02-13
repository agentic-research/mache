# Mache Architecture

## High-Level Design

```mermaid
graph TD
    subgraph Configuration
        Schema["Schema (JSON)"]
    end

    subgraph "Data Sources"
        SQLiteFile[.db (SQLite)]
        FlatFile[.json]
        SourceCode[.go / .py]
    end

    subgraph "Mache Core"
        SQLiteGraph[SQLiteGraph<br/>(Zero-Copy / Direct SQL)]
        Engine[Ingestion Engine]

        subgraph "Walkers"
            JW[JsonWalker]
            SW[SitterWalker]
            SL[SQLite Loader]
        end

        MemoryStore[MemoryStore Graph]
    end

    subgraph "System Interface"
        FUSE[FUSE Bridge<br/>(cgofuse)]
        Tools[User Tools<br/>ls, cat, grep]
    end

    Schema -->|Configures| SQLiteGraph
    Schema -->|Configures| Engine

    SQLiteFile -->|Direct Read| SQLiteGraph
    FlatFile -->|Ingest| Engine
    SourceCode -->|Ingest| Engine

    Engine --> JW
    Engine --> SW
    Engine --> SL

    JW -->|Builds| MemoryStore
    SW -->|Builds| MemoryStore
    SL -->|Builds| MemoryStore

    SQLiteGraph -->|Graph Interface| FUSE
    MemoryStore -->|Graph Interface| FUSE

    FUSE --- Tools
```

There are two data paths depending on the source:

1. **SQLite direct (`.db` files)** — `SQLiteGraph` queries the source database directly. A one-pass scan builds the directory tree (~4s for 323K records), then content is resolved on demand via primary key lookup. No data is copied.
2. **Ingestion (`.json`, `.go`, `.py`)** — The `Engine` dispatches to the appropriate `Walker`, renders templates, and bulk-loads nodes into `MemoryStore`.

Both paths are fronted by the same `Graph` interface and `FUSE Bridge`. A **Topology Schema** declares the directory structure using selectors and Go template strings for names/content.

With `--infer`, the schema itself can be derived automatically: the `lattice` package reservoir-samples records from a SQLite source, builds a Formal Concept Analysis lattice, and projects it into a valid `Topology` — detecting identifier fields, temporal shard levels, and leaf files without any hand-authored schema.

## Core Abstractions

- **`Walker` interface** — Abstracts over query engines. `JsonWalker` uses JSONPath; `SitterWalker` uses tree-sitter AST queries. Both return `Match` results with captured values and optional recursion context.
- **`Graph` interface** — Read-only access to the node store (`GetNode`, `ListChildren`, `ReadContent`). Two implementations:
  - **`MemoryStore`** — In-memory map for small datasets (JSON files, source code).
  - **`SQLiteGraph`** — Direct SQL backend for `.db` sources. One-pass parallel scan builds the directory tree; content resolved on demand via primary key lookup and template rendering. No data copied.
- **`Engine`** — Drives ingestion: walks files, dispatches to walkers, renders templates, builds the graph. Tracks source file paths for origin-aware nodes. Deduplicates same-name constructs (e.g. multiple `init()`) by appending `.from_<filename>` suffixes.
- **`MacheFS`** — FUSE implementation via cgofuse. Handle-based readdir with auto-mode for fuse-t compatibility. Extended cache timeouts (300s) for NFS performance. Supports both read-only and writable mounts.

## Write-Commit-Reparse Pipeline

When `--writable` is enabled, the FUSE layer supports writing to tree-sitter-backed file nodes:

```
Agent opens file → FUSE writeHandle buffers → Agent closes file →
  Splice into source → goimports → Re-ingest file → Graph updated
```

Key types:
- **`SourceOrigin`** (`graph.go`) — Tracks `FilePath`, `StartByte`, `EndByte` for each file node's position in its source.
- **`OriginProvider`** (`interfaces.go`) — Optional interface on `Match` to expose byte ranges from tree-sitter captures.
- **`Splice`** (`writeback/splice.go`) — Pure function: atomically replaces a byte range in a source file (temp file + rename).
- **`writeHandle`** (`fs/root.go`) — Per-open-file buffer. Dirty handles trigger splice → goimports → re-ingest on `Release`.

Re-ingestion after each write ensures all byte offsets stay correct — tree-sitter recalculates everything, so no manual offset math is needed.

## Key File Reference

| Concern | File | Key functions/types |
|---------|------|-------------------|
| CLI + mount wiring | `cmd/mount.go` | `rootCmd`, `--writable`, `--infer` flags |
| Schema types | `api/schema.go` | `Topology`, `Node`, `Leaf` |
| Ingestion orchestration | `internal/ingest/engine.go` | `Engine.Ingest`, `processNode`, `ingestTreeSitter`, `dedupSuffix` |
| JSON queries | `internal/ingest/json_walker.go` | `JsonWalker.Query` |
| Tree-sitter queries | `internal/ingest/sitter_walker.go` | `SitterWalker.Query`, `sitterMatch.CaptureOrigin` |
| Walker/Match contracts | `internal/ingest/interfaces.go` | `Walker`, `Match`, `OriginProvider` |
| SQLite streaming | `internal/ingest/sqlite_loader.go` | `StreamSQLiteRaw` |
| Graph (in-memory) | `internal/graph/graph.go` | `MemoryStore`, `Node`, `SourceOrigin`, `ContentRef` |
| Graph (SQLite direct) | `internal/graph/sqlite_graph.go` | `SQLiteGraph`, `EagerScan` |
| FUSE bridge + writes | `internal/fs/root.go` | `MacheFS`, `writeHandle`, `Open`, `Write`, `Release` |
| Source splicing | `internal/writeback/splice.go` | `Splice` |
| Go schema | `examples/go-schema.json` | functions, methods, types, constants, variables, imports |
| FCA inference | `internal/lattice/` | `FormalContext`, `NextClosure`, `Project`, `Inferrer` |
| Build/test | `Taskfile.yml` | `task build`, `task test`, `task check` |

## Architectural Decision Records (ADRs)

| ADR | Status | Summary |
|-----|--------|---------|
| [0001: User-Space FUSE Bridge](adr/0001-user-space-fuse-bridge.md) | Accepted | fuse-t + cgofuse for macOS (no kexts) |
| [0002: Declarative Topology Schema](adr/0002-declarative-topology-schema.md) | Accepted | Schema-driven ingestion with Go templates |
| [0003: CAS & Layered Overlays](adr/0003-cas-layered-overlays.md) | Proposed | Content-Addressed Storage and Docker-style layers (ideated) |
| [0004: MVCC Memory Ledger](adr/0004-mvcc-memory-ledger.md) | Proposed | ECS + mmap + RCU for 10M+ entities (ideated) |
| [0005: FCA Schema Inference](adr/0005-fca-schema-inference.md) | Proposed | NextClosure on sampled records, bitmap-accelerated lattice → topology |
