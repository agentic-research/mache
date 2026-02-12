# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

All commands use [Task](https://taskfile.dev) — it sets required CGO flags for fuse-t automatically.

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

**Do not use `go test` directly on macOS** — CGO_CFLAGS/CGO_LDFLAGS for fuse-t are required and only set by `task`.

Run a single test: `task test -- -run TestName` or set the env vars from Taskfile.yml and use `go test -v -run TestName ./internal/graph/`.

Integration tests require real Venturi databases and are gated behind env vars (`MACHE_TEST_KEV_DB`, `MACHE_TEST_NVD_DB`) — they skip automatically when unset.

## Architecture

Mache projects structured data (JSON, SQLite, source code) as a read-only FUSE filesystem driven by declarative JSON schemas.

### Two Data Paths

```
.db files  →  SQLiteGraph (zero-copy, lazy scan, direct SQL)  →  MacheFS (FUSE)
other files →  Engine (walkers + loaders)  →  MemoryStore       →  MacheFS (FUSE)
```

The mount wiring in `cmd/mount.go` selects the path based on file extension.

### Core Abstractions

| Concept | Location | Role |
|---------|----------|------|
| Schema types | `api/schema.go` | `Topology` → `Node` (dirs) → `Leaf` (files) — declarative tree definition |
| Graph interface | `internal/graph/graph.go` | `GetNode`, `ListChildren`, `ReadContent` — backend-agnostic |
| MemoryStore | `internal/graph/graph.go` | Map-based graph with RWMutex + FIFO content cache (1024 entries) |
| SQLiteGraph | `internal/graph/sqlite_graph.go` | Direct SQL backend: `compileLevels()` builds schema tree, `scanRoot()` streams all records using `json_extract()` in SQL, content resolved on-demand via PK lookup + template render |
| Engine | `internal/ingest/engine.go` | Dispatches by extension (.db/.json/.go/.py), recursive schema traversal, dedup via `dedupSuffix()` |
| Walkers | `internal/ingest/json_walker.go`, `sitter_walker.go` | JSONPath (ojg) and tree-sitter AST query — both implement `Walker`/`Match` interfaces from `interfaces.go` |
| MacheFS | `internal/fs/root.go` | FUSE impl: handle-based readdir (auto-mode for fuse-t), FNV-1a inodes, optional write-back |
| Splice | `internal/writeback/splice.go` | Atomic byte-range replacement in source files for write-back |

### Key Design Details

- **SQLiteGraph scan**: Single-pass streaming scan pushes field extraction into SQLite via `json_extract()`, builds directory tree (paths only) in `sync.Map`. Content is never bulk-loaded — resolved on-demand per file read.
- **Template rendering**: `RenderTemplate()` in `engine.go` supports custom funcs: `json` (marshal), `first` (first element), `slice` (substring).
- **ContentRef**: Large content (>4KB) uses lazy `ContentRef` with DBPath/RecordID/Template instead of inline bytes.
- **Write-back pipeline** (on FUSE `Release`): splice source file → run `goimports -w` → re-ingest file to update graph.
- **fuse-t on macOS**: Translates FUSE→NFS. Extended cache timeouts (300s) are critical for performance. Auto-mode readdir required (all results in first pass).

### Example Schemas

`examples/` contains three schemas showing the pattern: `nvd-schema.json` (temporal sharding by year/month), `kev-schema.json` (flat), `go-schema.json` (tree-sitter queries for Go source).

## Conventions

- **Formatter**: gofumpt (stricter than gofmt). Pre-commit hooks enforce it.
- **Test framework**: `github.com/stretchr/testify` (assert/require).
- **Pure-Go SQLite**: Uses `modernc.org/sqlite` (no CGO dependency for SQLite itself — CGO is only needed for fuse-t).
- **CI**: GitHub Actions runs `go test -race ./...` and `golangci-lint` on ubuntu with libfuse-dev.
- Ignore ld warnings about duplicate `-rpath` — pre-existing fuse-t noise.
