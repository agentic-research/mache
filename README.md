# 🗂️ Mache

**The Universal Semantic Overlay Engine**

Mache projects structured data and source code into navigable, read-only filesystems using declarative schemas. Point it at a JSON feed or a codebase, define a topology, and mount a FUSE filesystem you can explore with `ls`, `cat`, `grep`, and friends.

## Table of Contents

- [Status](#status)
- [Feature Matrix](#feature-matrix)
- [How It Works](#how-it-works)
- [Quick Start](#quick-start)
- [Usage](#usage)
  - [Write-Back Mode](#write-back-mode)
- [Architecture](#architecture)
  - [Write-Commit-Reparse Pipeline](#write-commit-reparse-pipeline)
- [Roadmap](#roadmap)
- [Development](#development)
- [License](#license)
- [Contributing](#contributing)
- [Related Work](#related-work)

## Status

Mache is in **early development**. The core pipeline (schema + ingestion + FUSE mount) works end-to-end across multiple data sources. See the [Feature Matrix](#feature-matrix) below for current status.

## Feature Matrix

| Feature | Status | Notes |
|---------|--------|-------|
| FUSE Bridge (read-only) | **Implemented** | macOS via fuse-t + cgofuse, Linux via libfuse; handle-based readdir with auto-mode |
| Declarative Topology Schemas | **Implemented** | JSON schema with Go `text/template` rendering; supports arbitrary nesting depth |
| JSON Ingestion (JSONPath) | **Implemented** | Powered by [ojg/jp](https://github.com/ohler55/ojg) |
| SQLite Direct Backend | **Implemented** | Zero-copy: mounts `.db` files instantly, reads records on demand via primary key lookup |
| SQLite Ingestion (MemoryStore) | **Implemented** | Bulk-loads `.db` records into in-memory graph for smaller datasets |
| Tree-sitter Code Parsing | **Implemented** | Go and Python source files; captures functions, methods, types, constants, variables, imports |
| In-Memory Graph Store | **Implemented** | `sync.RWMutex`-backed map, suitable for small datasets |
| NVD Schema (`examples/nvd-schema.json`) | **Included** | 323K CVE records sharded by year/month over the SQLite direct backend |
| KEV Schema (`examples/kev-schema.json`) | **Included** | CISA Known Exploited Vulnerabilities catalog |
| Go Source Schema (`examples/go-schema.json`) | **Included** | Functions, methods, types, constants, variables, imports; same-name dedup (e.g. multiple `init()`) |
| Write-Back (FUSE writes) | **Implemented** | `--writable` flag; splice edits into source, run goimports, re-ingest. Tree-sitter sources only |
| Content-Addressed Storage (CAS) | **Ideated** | Described in ADR-0003; no code exists |
| Layered Overlays (Docker-style) | **Ideated** | Composable data views; no code exists |
| SQLite Virtual Tables | **Ideated** | Complex queries beyond fs navigation; described in ADR-0004 |
| MVCC Memory Ledger | **Ideated** | Wait-free reads, mmap-backed; described in ADR-0004 |
| Self-Organizing Learned FS | **Ideated** | ML-driven directory reorganization; described in ADR-0003 |

### Legend

- **Implemented** — Working code with tests
- **Included** — Ready-to-use example schema in `examples/`
- **Stubbed** — Interface/types exist but implementation is partial or placeholder
- **Ideated** — Described in an ADR or design doc; no code yet

## How It Works

```
 Schema (JSON)         Data Source
 ┌─────────────┐      ┌──────────────────────────────────────┐
 │ topology:    │      │ .db (SQLite)  │ .json   │ .go / .py │
 │   nodes:     │      └───────┬───────┴────┬────┴─────┬─────┘
 │     ...      │              │            │          │
 └──────┬───────┘              │     ┌──────┴──────────┘
        │              ┌───────┘     │
        ▼              ▼             ▼
 ┌──────────────┐  ┌─────────────────────────────┐
 │ SQLiteGraph  │  │     Ingestion Engine        │
 │ (zero-copy)  │  │  Walker interface:          │
 │ Direct SQL   │  │   - JsonWalker (JSONPath)   │
 │ queries on   │  │   - SitterWalker (AST)      │
 │ source DB    │  │   - SQLite loader           │
 └──────┬───────┘  └──────────────┬──────────────┘
        │                         ▼
        │          ┌─────────────────────────────┐
        │          │   Graph (MemoryStore)       │
        │          │   Node { ID, Mode, Data,   │
        │          │          Children }         │
        │          └──────────────┬──────────────┘
        │                        │
        └────────┬───────────────┘
                 ▼
      ┌─────────────────────────────────┐
      │   FUSE Bridge (cgofuse)         │
      │   ls / cat / grep / find        │
      └─────────────────────────────────┘
```

There are two data paths depending on the source:

1. **SQLite direct (`.db` files)** — `SQLiteGraph` queries the source database directly. A one-pass scan builds the directory tree (~4s for 323K records), then content is resolved on demand via primary key lookup. No data is copied.
2. **Ingestion (`.json`, `.go`, `.py`)** — The `Engine` dispatches to the appropriate `Walker`, renders templates, and bulk-loads nodes into `MemoryStore`.

Both paths are fronted by the same `Graph` interface and `FUSE Bridge`. A **Topology Schema** declares the directory structure using selectors and Go template strings for names/content.

## Quick Start

### Prerequisites

**macOS (Apple Silicon/Intel):**
```bash
# Install fuse-t (userspace FUSE, no kernel extensions required)
brew install --cask fuse-t

# Install Task (build tool)
brew install go-task
```

**Linux:**
```bash
# Install FUSE development headers
# Ubuntu/Debian:
sudo apt-get install libfuse-dev

# Fedora/RHEL:
sudo dnf install fuse-devel

# Install Task
brew install go-task
# or: go install github.com/go-task/task/v3/cmd/task@latest
```

### Building

```bash
git clone https://github.com/agentic-research/mache.git
cd mache

# Build (checks for fuse-t on macOS, builds and codesigns)
task build

# Run tests
task test

# See all available tasks
task --list
```

### Using Plain Go Commands

If you prefer not to use Task, set CGO flags manually on macOS:

```bash
# macOS only
export CGO_CFLAGS="-I/Library/Frameworks/fuse_t.framework/Versions/Current/Headers"
export CGO_LDFLAGS="-F/Library/Frameworks -framework fuse_t -Wl,-rpath,/Library/Frameworks"

go build
go test ./...
```

## Usage

```bash
# Mount a SQLite database (instant — zero-copy, direct SQL queries)
./mache --schema examples/nvd-schema.json --data results.db /tmp/nvd

# Mount a JSON file (ingests into memory)
./mache --schema schema.json --data data.json /tmp/mount

# Flags:
#   -s, --schema     Path to topology schema (default: ~/.mache/mache.json)
#   -d, --data       Path to data source file or directory (default: ~/.mache/data.json)
#   -w, --writable   Enable write-back (splice edits into source files)
```

### Write-Back Mode

With `--writable`, file nodes backed by tree-sitter source code become editable. When you write to a file and close it, mache:

1. **Splices** the new content into the original source file at the exact byte range
2. **Runs `goimports`** to fix imports and formatting (failure-tolerant)
3. **Re-ingests** the source file so all graph nodes get fresh byte ranges

```bash
# Mount Go source with write-back enabled
./mache -w -s examples/go-schema.json -d . /tmp/mache-src

# Read a function
cat /tmp/mache-src/ingest/functions/NewEngine/source

# Edit it (via echo, editor, or AI agent)
echo 'func NewEngine(schema *api.Topology, store IngestionTarget) *Engine {
    return &Engine{Schema: schema, Store: store}
}' > /tmp/mache-src/ingest/functions/NewEngine/source

# The source file is now updated
grep -A3 'func NewEngine' internal/ingest/engine.go
```

Only tree-sitter-backed nodes (`.go`, `.py`) support writes. JSON and SQLite nodes remain read-only. Nodes without write support report `0444` permissions; writable nodes report `0644`.

### Example: NVD Vulnerability Database

Mount 323K NVD CVE records as a browsable filesystem, sharded by year and month:

```bash
./mache --schema examples/nvd-schema.json \
        --data /path/to/nvd/results.db \
        /tmp/nvd
```

```
/tmp/nvd/
  by-cve/
    2024/
      01/
        CVE-2024-0001/
          description   # "A buffer overflow in FooBar..."
          published     # "2024-01-15T00:00:00Z"
          status        # "Analyzed"
          raw.json      # Full JSON record
        CVE-2024-0002/
        ...
      02/
      ...
    2023/
      ...
```

The schema that produces this structure (`examples/nvd-schema.json`) uses `slice` to extract year and month from the published date:

```json
{
  "name": "{{slice .item.cve.published 0 4}}",
  "selector": "$[*]",
  "children": [{
    "name": "{{slice .item.cve.published 5 7}}",
    "selector": "$",
    "children": [{
      "name": "{{.item.cve.id}}",
      "selector": "$",
      "files": [...]
    }]
  }]
}
```

### Example: Projecting JSON Data

Given a `data.json`:
```json
{
  "users": [
    {"name": "Alice", "role": "admin"},
    {"name": "Bob", "role": "user"}
  ]
}
```

And a `schema.json`:
```json
{
  "version": "v1",
  "nodes": [
    {
      "name": "users",
      "selector": "$",
      "children": [
        {
          "name": "{{.name}}",
          "selector": "users[*]",
          "files": [
            {
              "name": "role",
              "content_template": "{{.role}}"
            }
          ]
        }
      ]
    }
  ]
}
```

Produces the filesystem:
```
/mountpoint/
  users/
    Alice/
      role        # contains "admin"
    Bob/
      role        # contains "user"
```

### Example: Projecting Source Code

The ingestion engine auto-detects `.go` and `.py` files and uses tree-sitter for parsing. A schema can use tree-sitter query syntax as selectors:

```json
{
  "nodes": [
    {
      "name": "{{.name}}",
      "selector": "(function_declaration name: (identifier) @name body: (block) @scope)"
    }
  ]
}
```

Captures named `@scope` define the recursion context for child nodes.

## Architecture

### Core Abstractions

- **`Walker` interface** — Abstracts over query engines. `JsonWalker` uses JSONPath; `SitterWalker` uses tree-sitter AST queries. Both return `Match` results with captured values and optional recursion context.
- **`Graph` interface** — Read-only access to the node store (`GetNode`, `ListChildren`, `ReadContent`). Two implementations:
  - **`MemoryStore`** — In-memory map for small datasets (JSON files, source code).
  - **`SQLiteGraph`** — Direct SQL backend for `.db` sources. One-pass parallel scan builds the directory tree; content resolved on demand via primary key lookup and template rendering. No data copied.
- **`Engine`** — Drives ingestion: walks files, dispatches to walkers, renders templates, builds the graph. Tracks source file paths for origin-aware nodes. Deduplicates same-name constructs (e.g. multiple `init()`) by appending `.from_<filename>` suffixes.
- **`MacheFS`** — FUSE implementation via cgofuse. Handle-based readdir with auto-mode for fuse-t compatibility. Extended cache timeouts (300s) for NFS performance. Supports both read-only and writable mounts.

### Write-Commit-Reparse Pipeline

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

### ADRs

| ADR | Status | Summary |
|-----|--------|---------|
| [0001: User-Space FUSE Bridge](docs/adr/0001-user-space-fuse-bridge.md) | Accepted | fuse-t + cgofuse for macOS (no kexts) |
| [0002: Declarative Topology Schema](docs/adr/0002-declarative-topology-schema.md) | Accepted | Schema-driven ingestion with Go templates |
| [0003: Self-Organizing Learned FS](docs/adr/0003-self-organizing-learned-filesystem.md) | Proposed | ML-driven directory reorganization (ideated) |
| [0004: MVCC Memory Ledger](docs/adr/0004-mvcc-memory-ledger.md) | Proposed | ECS + mmap + RCU for 10M+ entities (ideated) |

## Roadmap

These are directional goals, not commitments:

- **Content-addressed storage** — Store data by hash, reference via hard links for deduplication
- **Layered overlays** — Docker-style composable layers for versioned data views
- **Additional walkers** — YAML, TOML, HCL, more tree-sitter grammars
- **Write support** — ~~Bidirectional sync~~ Basic write-back landed for tree-sitter sources; next: create/delete constructs, rename/move, multi-file batch
- **SQLite virtual tables** — SQL queries over the projected filesystem
- **Go NFS server** — Replace fuse-t's NFS translation layer for full control over caching behavior

## Development

```bash
task test              # Run all tests
task test-coverage     # Generate coverage report
task fmt               # Format code (gofumpt)
task vet               # Run go vet
task lint              # Run golangci-lint
task check             # Run all checks (fmt, vet, lint, test)
task clean             # Remove build artifacts
task tidy              # Tidy go modules
```

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.

## Contributing

This is an early-stage research project. Contributions welcome, but expect rapid API changes. See [CONTRIBUTING.md](CONTRIBUTING.md) for details.

## Related Work

- **BREAD Paper** — Theoretical foundation for graph-to-filesystem projection
- **fuse-t** — Userspace FUSE implementation for macOS
- **cgofuse** — Cross-platform FUSE binding for Go
- **ojg** — JSON processing and JSONPath for Go
- **go-tree-sitter** — Tree-sitter bindings for Go
