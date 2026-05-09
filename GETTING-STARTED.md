# Getting started with mache

This guide walks you from install to a working MCP server in under five minutes, then through the things you'll likely want next: filesystem mount, write-back, schema inference, troubleshooting.

For the architectural picture, see [ARCHITECTURE.md](docs/ARCHITECTURE.md). For where the project is going, see [ROADMAP.md](docs/ROADMAP.md).

## Install

Build from source. Requires [Go 1.23+](https://go.dev/dl/) and [Task](https://taskfile.dev/installation/):

```bash
git clone https://github.com/agentic-research/mache.git
cd mache
task build
task install   # copies the binary to ~/.local/bin
```

**Platform notes:**

- **macOS** — `brew install go-task` for Task.
- **Linux** — [install Task](https://taskfile.dev/installation/). NFS mount requires `nfs-common` (`apt-get install nfs-common`).

To check the install:

```bash
mache version
```

## First run — MCP server

The simplest way to use mache is as an MCP server pointed at a codebase or `.db` file:

```bash
mache serve .
# starts on localhost:7532, auto-infers a schema, ready for clients
```

**Connect from Claude Code (HTTP, recommended — one server shared across sessions):**

```bash
claude mcp add --transport http mache http://localhost:7532/mcp
```

**Connect from Claude Code (stdio — subprocess per session, useful when the host manages lifecycle):**

`.mcp.json`:

```json
{
  "mcpServers": {
    "mache": {
      "command": "mache",
      "args": ["serve", "--stdio", "."]
    }
  }
}
```

**Connect from Claude Desktop:**

```json
{
  "mcpServers": {
    "mache": {
      "command": "/path/to/mache",
      "args": ["serve", "--stdio", "/path/to/code"]
    }
  }
}
```

Once connected, the agent can call `list_directory`, `read_file`, `find_callers`, `find_callees`, `find_definition`, `search`, `get_communities`, `get_overview`, `get_impact`, `get_architecture`, `get_diagram`, `write_file`, `find_smells`, and `resolve_ref`. With [ley-line-open](https://github.com/agentic-research/ley-line-open) installed, three more tools become available: `semantic_search`, `get_type_info`, `get_diagnostics`.

See the [MCP Server section in ARCHITECTURE.md](docs/ARCHITECTURE.md#core-abstractions) for the full tool inventory and the [Interplay with ley-line-open](docs/ARCHITECTURE.md#interplay-with-ley-line-open) section for which tools require a `.db` built by ley-line-open.

## Cross-repo serve (`--mount`)

To span multiple codebases from one MCP endpoint, use repeatable `--mount NAME=PATH` flags:

```bash
mache serve --mount auth=./auth-svc --mount billing=./billing-svc
```

Each mount becomes a top-level virtual directory. `find_callers Validate` walks both repos and returns results annotated with their mount of origin:

```json
{
  "callers": [
    {"path": "auth/functions/AuthCaller/source", "mount": "auth"},
    {"path": "billing/functions/Charge/source",  "mount": "billing"}
  ]
}
```

Mounts compose any source `mache serve` accepts — directories, `.db` files, or a mix. `--mount` and a positional source are mutually exclusive (use one or the other).

See [ARCHITECTURE.md § Cross-repo serve](docs/ARCHITECTURE.md#cross-repo-serve---mount) for the full design. Cross-mount `find_callees` / `find_definition` / `find_callers` all resolve and annotate today; `search` and `get_impact` still emit the legacy single-string shape on cross-mount results.

## Mount as a filesystem (optional)

Beyond MCP, the graph can be exposed as a mounted directory tree — useful for `ls`, `cat`, shell scripts, or tools that work with files:

```bash
mache --infer -d ./src --writable /tmp/mache-src
```

Layout:

```
/tmp/mache-src/
  functions/
    HandleRequest/
      source        # the function body
      context       # imports, types visible to this scope
      callers/      # who calls this function
      callees/      # what this function calls
    ValidateToken/
      source
  types/
    Config/
      source        # type Config struct { ... }
  _project_files/
    README.md
    go.mod
```

Navigate by function name, not file path. `callers/` and `callees/` are virtual directories that appear only when references exist.

**Other mount examples:**

```bash
# Agent mode — generates PROMPT.txt for LLMs
mache --agent -d ~/my-project

# Mount a SQLite database directly (zero-copy, no ingestion)
mache --schema examples/nvd-schema.json --data results.db /tmp/nvd

# Hot-reload schema while mounted
mache --control /tmp/control.sock --infer -d ./src /tmp/mount
```

NFS is the only mount backend (FUSE was removed in v0.7.0; for FUSE today, use [ley-line-open's `leyline serve`](https://github.com/agentic-research/ley-line-open)).

## Write-back

With `--writable`, edits to `source` files go through a pipeline before touching the actual source on disk:

1. **Validate** — tree-sitter checks syntax
1. **Format** — gofumpt for Go, hclwrite for HCL/Terraform
1. **Splice** — atomic byte-range replacement in the source file
1. **Update** — node content updated in-place, no re-ingest

If validation fails, the write is saved as a draft. The node path stays stable; errors surface via `_diagnostics/`.

For the full pipeline design, see [ADR-0009: AST-Aware Write Pipeline](docs/adr/0009-ast-aware-write-pipeline.md).

## Schema inference

Mache auto-infers a topology from your source if you don't provide a schema (`--infer` is the default for source mounts). The inference pipeline:

1. **FCA** (Formal Concept Analysis) — reservoir-samples records, builds a concept lattice, projects to a `Topology`
1. **Greedy entropy** — information-theoretic field scoring picks identifier columns, temporal shards, leaf files
1. Schema-driven projection mounts the result

To inspect the inferred schema after mount:

```bash
cat /tmp/mache-src/_schema.json
```

Or pass an explicit schema if the inferred one isn't right:

```bash
mache serve --schema examples/go-schema.json -d ./src
```

See [ADR-0005](docs/adr/0005-fca-schema-inference.md) and [ADR-0008](docs/adr/0008-greedy-entropy-schema-inference.md) for the inference design.

## Troubleshooting

**`get_diagnostics` returns "no \_lsp table in database"** — the LSP-enrichment tools require a `.db` built by [ley-line-open](https://github.com/agentic-research/ley-line-open). Either pre-enrich with `ll-open enrich-lsp`, pass a `file` param to trigger live enrichment (requires the ley-line daemon), or skip those three tools — the other 14 work without ley-line-open. See [Interplay with ley-line-open](docs/ARCHITECTURE.md#interplay-with-ley-line-open).

**`find_smells` rule "requires SQL tables [\_ast]" error** — same root cause. The four `_ast`-based smell rules (`magic_int_in_comparison`, `cyclomatic_complexity`, `long_function`, `long_file`) need a `.db` built by ley-line-open. The other five rules (`dead_code`, `untested_function`, `duplicate_definitions`, `god_file`, `fan_out_skew`) run on standalone mache.

**Mount stuck or `umount` complains "device busy"** — see `mache unmount <mountpoint>` and the open-bead ergonomics in `mache-fsi`. macOS sometimes needs `diskutil unmount force`.

**NFS mount fails on Linux with "no such device"** — install `nfs-common` (`apt-get install nfs-common`).

**Build error about CGO** — mache's standalone path uses CGO tree-sitter; either install `gcc` / `clang` or use the ley-line-open-paired path (no CGO required for that).

## Where to next

- [ARCHITECTURE.md](docs/ARCHITECTURE.md) — graph backends, write pipeline, virtual directories, ley-line-open interplay
- [ROADMAP.md](docs/ROADMAP.md) — what's stable, what's next, what's long-term
- [CONTRIBUTING.md](CONTRIBUTING.md) — how to send a PR
- [docs/adr/](docs/adr/) — Architectural Decision Records
- [docs/PRIOR_ART.md](docs/PRIOR_ART.md) — what mache builds on
- [docs/competitive-landscape-2026.md](docs/competitive-landscape-2026.md) — comparison with other tools in the space
