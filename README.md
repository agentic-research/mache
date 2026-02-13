# 🗂️ Mache

[![CI](https://github.com/agentic-research/mache/actions/workflows/ci.yml/badge.svg)](https://github.com/agentic-research/mache/actions/workflows/ci.yml)

**The Universal Semantic Overlay Engine**

Mache projects structured data and source code into navigable, read-only filesystems using declarative schemas. Point it at a JSON feed or a codebase, define a topology (or let mache infer one), and mount a FUSE filesystem you can explore with `ls`, `cat`, `grep`, and friends.

## Table of Contents

- [Status](#status)
- [Feature Matrix](#feature-matrix)
- [Quick Start](#quick-start)
- [Usage](#usage)
  - [Example: NVD Vulnerability Database](#example-nvd-vulnerability-database)
  - [Example: Projecting JSON Data](#example-projecting-json-data)
  - [Example: Projecting Source Code](#example-projecting-source-code)
  - [Write-Back Mode](#write-back-mode)
- [How It Works](#how-it-works)
- [Documentation](#documentation)
- [Contributing](#contributing)
- [License](#license)

## Status

Mache is in **early development**. The core pipeline (schema + ingestion + FUSE mount) works end-to-end across multiple data sources.

## Feature Matrix

| Feature | Status | Notes |
|---------|--------|-------|
| FUSE Bridge (read-only) | **Implemented** | macOS via fuse-t + cgofuse, Linux via libfuse |
| Declarative Topology Schemas | **Implemented** | JSON schema with Go `text/template` rendering |
| JSON Ingestion (JSONPath) | **Implemented** | Powered by [ojg/jp](https://github.com/ohler55/ojg) |
| SQLite Direct Backend | **Implemented** | Zero-copy: mounts `.db` files instantly |
| Tree-sitter Code Parsing | **Implemented** | Go and Python source files |
| Schema Inference (FCA) | **Implemented** | `--infer` flag; builds concept lattice from data |
| Cross-Reference Indexing | **Implemented** | Roaring bitmap inverted index |
| Write-Back (FUSE writes) | **Implemented** | `--writable` flag; splice edits into source |

## Quick Start

### Prerequisites

- **macOS:** `brew install --cask fuse-t` and `brew install go-task`
- **Linux:** `apt-get install libfuse-dev` and [install Task](https://taskfile.dev/installation/)

### Building

```bash
git clone https://github.com/agentic-research/mache.git
cd mache

# Build (checks for fuse-t on macOS, builds and codesigns)
task build

# Run tests
task test
```

## Usage

```bash
# Mount a SQLite database (instant — zero-copy, direct SQL queries)
./mache --schema examples/nvd-schema.json --data results.db /tmp/nvd

# Mount with zero-config schema inference (no schema authoring needed)
./mache --infer --data results.db /tmp/nvd

# Mount a JSON file (ingests into memory)
./mache --schema schema.json --data data.json /tmp/mount
```

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
          raw.json      # Full JSON record
```

### Example: Projecting JSON Data

Given a `data.json` with users, you can project it into a `users/` directory where each file contains specific fields.

### Example: Projecting Source Code

Mache auto-detects `.go` and `.py` files. Use tree-sitter queries in your schema to map AST nodes (functions, types) to directories.

### Write-Back Mode

With `--writable`, file nodes backed by tree-sitter source code become editable.

```bash
# Mount Go source with write-back enabled
./mache -w -s examples/go-schema.json -d . /tmp/mache-src
```

When you edit a file in the mount, Mache splices the content back into the original source file and runs `goimports`.

## How It Works

Mache uses a **Topology Schema** to map data from SQLite, JSON, or source code into a filesystem structure.

1. **Direct Mode:** For SQLite, it queries the DB on-demand (zero-copy).
2. **Ingest Mode:** For JSON/Code, it loads data into an in-memory graph.
3. **Inference:** With `--infer`, it uses Formal Concept Analysis to guess the best folder structure.

See [Architecture](docs/ARCHITECTURE.md) for details.

## Documentation

- [Architecture & Design](docs/ARCHITECTURE.md) - Deep dive into internals, pipelines, and abstractions.
- [Roadmap](docs/ROADMAP.md) - Future plans and known limitations.
- [Example Schemas](examples/README.md) - Details on the included examples.
- [ADRs](docs/adr/) - Architectural Decision Records.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for details.

## License

Apache License 2.0. See [LICENSE](LICENSE).
