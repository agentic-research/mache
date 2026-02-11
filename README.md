# Mache

**The Universal Semantic Overlay Engine**

Mache projects structured data and source code into navigable, read-only filesystems using declarative schemas. Point it at a JSON feed or a codebase, define a topology, and mount a FUSE filesystem you can explore with `ls`, `cat`, `grep`, and friends.

## Status

Mache is in **Phase 0** — a working proof-of-concept. The core pipeline (schema + ingestion + FUSE mount) works end-to-end. See the [Feature Matrix](#feature-matrix) below for what's implemented, stubbed, and on the roadmap.

## Feature Matrix

| Feature | Status | Notes |
|---------|--------|-------|
| FUSE Bridge (read-only) | **Implemented** | macOS via fuse-t + cgofuse, Linux via libfuse |
| Declarative Topology Schemas | **Implemented** | JSON schema with Go `text/template` rendering |
| JSON Ingestion (JSONPath) | **Implemented** | Powered by [ojg/jp](https://github.com/ohler55/ojg) |
| Tree-sitter Code Parsing | **Implemented** | Go and Python source files |
| In-Memory Graph Store | **Implemented** | `sync.RWMutex`-backed map, suitable for small datasets |
| Content-Addressed Storage (CAS) | **Ideated** | Described in ADR-0003; no code exists |
| Layered Overlays (Docker-style) | **Ideated** | Composable data views; no code exists |
| SQLite Virtual Tables | **Ideated** | Complex queries beyond fs navigation; described in ADR-0004 |
| MVCC Memory Ledger | **Ideated** | Wait-free reads, mmap-backed; described in ADR-0004 |
| Self-Organizing Learned FS | **Ideated** | ML-driven directory reorganization; described in ADR-0003 |

### Legend

- **Implemented** — Working code with tests
- **Stubbed** — Interface/types exist but implementation is partial or placeholder
- **Ideated** — Described in an ADR or design doc; no code yet

## How It Works

```
 Schema (JSON)         Data Source            Source Code
 ┌─────────────┐      ┌──────────────┐       ┌──────────────┐
 │ topology:    │      │ data.json    │       │ *.go / *.py  │
 │   nodes:     │      │ (JSONPath)   │       │ (tree-sitter)│
 │     ...      │      └──────┬───────┘       └──────┬───────┘
 └──────┬───────┘             │                      │
        │              ┌──────┴──────────────────────┘
        ▼              ▼
 ┌─────────────────────────────┐
 │     Ingestion Engine        │
 │  Walker interface:          │
 │   - JsonWalker (JSONPath)   │
 │   - SitterWalker (AST)     │
 └──────────────┬──────────────┘
                ▼
 ┌─────────────────────────────┐
 │   Graph (MemoryStore)       │
 │   Node { ID, Mode, Data,   │
 │          Children }         │
 └──────────────┬──────────────┘
                ▼
 ┌─────────────────────────────┐
 │   FUSE Bridge (cgofuse)     │
 │   ls / cat / grep / find    │
 └─────────────────────────────┘
```

1. A **Topology Schema** declares the directory structure using selectors (JSONPath or tree-sitter queries) and Go template strings for names/content.
2. The **Ingestion Engine** dispatches to the appropriate **Walker** by file extension (`.json` → JSONPath, `.go`/`.py` → tree-sitter).
3. Walkers query the data, extract captures, and the engine renders templates to build a **Graph** of nodes.
4. The **FUSE Bridge** projects the graph as a read-only filesystem.

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
# Mount with a schema and data source
./mache --schema schema.json --data data.json /path/to/mountpoint

# Flags:
#   -s, --schema   Path to topology schema (default: ~/.agentic-research/mache/mache.json)
#   -d, --data     Path to data source file or directory (default: ~/.agentic-research/mache/data.json)
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
  "version": "v1alpha1",
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
- **`Graph` interface** — Read-only access to the node store (`GetNode`, `ListChildren`). Currently backed by `MemoryStore`; designed to be swappable (SQLite, mmap, etc.).
- **`Engine`** — Drives ingestion: walks files, dispatches to walkers, renders templates, builds the graph.
- **`MacheFS`** — FUSE implementation via cgofuse. Translates `Open`/`Getattr`/`Readdir`/`Read` to graph lookups. No heuristics — all decisions come from the graph.

### ADRs

| ADR | Status | Summary |
|-----|--------|---------|
| [0001: User-Space FUSE Bridge](docs/adr/0001-user-space-fuse-bridge.md) | Accepted | fuse-t + cgofuse for macOS (no kexts) |
| [0002: Declarative Topology Schema](docs/adr/0002-declarative-topology-schema.md) | Accepted | Schema-driven ingestion with Go templates |
| [0003: Self-Organizing Learned FS](docs/adr/0003-self-organizing-learned-filesystem.md) | Proposed | ML-driven directory reorganization (ideated) |
| [0004: MVCC Memory Ledger](docs/adr/0004-mvcc-memory-ledger.md) | Proposed | ECS + mmap + RCU for 10M+ entities (ideated) |

## Roadmap

These are directional goals, not commitments:

- **Persistent storage** — Replace `MemoryStore` with something durable (SQLite, mmap-backed arena)
- **Content-addressed storage** — Store data by hash, reference via hard links for deduplication
- **Layered overlays** — Docker-style composable layers for versioned data views
- **Additional walkers** — YAML, TOML, HCL, more tree-sitter grammars
- **Write support** — Bidirectional sync between filesystem mutations and source data
- **SQLite virtual tables** — SQL queries over the projected filesystem

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

This is an early-stage research project. Contributions welcome, but expect rapid API changes.

## Related Work

- **BREAD Paper** — Theoretical foundation for graph-to-filesystem projection
- **fuse-t** — Userspace FUSE implementation for macOS
- **cgofuse** — Cross-platform FUSE binding for Go
- **ojg** — JSON processing and JSONPath for Go
- **go-tree-sitter** — Tree-sitter bindings for Go
