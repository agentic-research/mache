# Mache

Mache turns code and structured data into a navigable graph — functions, types, cross-references, and call chains — exposed as MCP tools or a mounted filesystem.

Point it at a codebase. It parses the code, discovers the structure, and lets your agent (or you) explore by following call chains, jumping to definitions, and reading context — instead of grepping through flat files.

> *Mache* (/mɑʃe/ *mah-shay*): from *papier-mâché* — raw material, crushed and remolded into shape.

![Mache Demo](demo.gif)

## Quick start

### Install

Build from source (requires [Go 1.23+](https://go.dev/dl/) and [Task](https://taskfile.dev/installation/)):

```bash
git clone https://github.com/agentic-research/mache.git
cd mache
task build
task install   # copies to ~/.local/bin
```

**macOS:** `brew install go-task` for Task. FUSE mount needs `brew install --cask fuse-t`.
**Linux:** `apt-get install libfuse-dev` for FUSE mount support.

### Use with Claude Code

Start the server, then register it:

```bash
mache serve .                    # starts on localhost:7532
claude mcp add --transport http mache http://localhost:7532/mcp
```

Mache auto-infers the schema from your codebase. One server shared across all sessions.

## MCP tools

12 tools work out of the box. 3 optional tools provide LSP type info and semantic search when [ley-line-open](https://github.com/agentic-research/ley-line-open) enrichment is available — without it, they return a message explaining how to enable them.

| Tool               | What it does                                                   |
| ------------------ | -------------------------------------------------------------- |
| `get_overview`     | Top-level structure, node counts, entry points                 |
| `list_directory`   | Browse the graph by path                                       |
| `read_file`        | Read source content (supports batch reads)                     |
| `find_definition`  | Jump to where a symbol is defined                              |
| `find_callers`     | Who calls this?                                                |
| `find_callees`     | What does this call?                                           |
| `search`           | Pattern match across symbols                                   |
| `get_communities`  | Find clusters of tightly-coupled code                          |
| `get_impact`       | Blast radius of changing a symbol                              |
| `get_architecture` | Entry points, abstractions, dependency layers                  |
| `get_diagram`      | Mermaid diagram of system structure                            |
| `write_file`       | Edit through the splice pipeline: validate, format, splice     |
| `semantic_search`  | Natural-language search via embeddings *(optional — ley-line)* |
| `get_type_info`    | LSP type info and hover data *(optional — ley-line)*           |
| `get_diagnostics`  | LSP errors and warnings *(optional — ley-line)*                |

## How it works

1. **Parse** — tree-sitter parses source into AST nodes (28 languages)
1. **Infer** — schema inference (FCA) discovers the natural groupings (`functions/`, `types/`, `classes/`)
1. **Link** — cross-reference extraction builds a call graph from identifiers and imports
1. **Project** — SQL maps the graph into a navigable tree

## Mount as a filesystem

Mache can also mount your data as a real directory tree:

```bash
mache --infer -d ./src --writable /tmp/mache-src
```

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

<details>
<summary>More mount examples</summary>

```bash
# Mount with agent mode (generates PROMPT.txt for LLMs)
mache --agent -d ~/my-project

# Mount a SQLite database (zero-copy)
mache --schema examples/nvd-schema.json --data results.db /tmp/nvd
```

</details>

<details>
<summary>Write-back</summary>

With `--writable`, edits to `source` files go through a pipeline before touching your actual source:

1. **Validate** — tree-sitter checks syntax
1. **Format** — gofumpt (Go), hclwrite (HCL)
1. **Splice** — atomic byte-range replacement in the source file
1. **Update** — node content updated in-place, no re-ingest

If the syntax is wrong, the write is saved as a draft. The node path stays stable. Errors show up in `_diagnostics/`.

</details>

## MCP server options

```bash
# HTTP (recommended) — one server, multiple clients
mache serve .
mache serve --http localhost:9000 -s examples/nvd-schema.json results.db

# stdio — for tools that manage the subprocess lifecycle
mache serve --stdio .
```

<details>
<summary>Claude Code setup (detailed)</summary>

**HTTP (recommended)** — register once, shared across sessions:

```bash
mache serve /path/to/data &
claude mcp add --transport http mache http://localhost:7532/mcp
```

**stdio** — `.mcp.json` (spawns a subprocess per session):

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

</details>

<details>
<summary>Claude Desktop setup</summary>

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

</details>

## Status

| Capability                              | Status                                                                        |
| --------------------------------------- | ----------------------------------------------------------------------------- |
| Tree-sitter parsing (28 langs)          | Stable                                                                        |
| MCP server (15 tools, stdio + HTTP)     | Stable                                                                        |
| Cross-references (callers/callees)      | Stable                                                                        |
| NFS/FUSE mount + write-back             | Stable                                                                        |
| Schema inference (FCA)                  | Beta                                                                          |
| Community detection (Louvain)           | Beta                                                                          |
| LSP enrichment (type info, diagnostics) | Optional — [ley-line-open](https://github.com/agentic-research/ley-line-open) |
| Semantic search (embeddings)            | Optional — [ley-line-open](https://github.com/agentic-research/ley-line-open) |

<details>
<summary>Why this exists</summary>

Agents operate without topology. They see flat files, grep for strings, build a mental model, forget it next turn, rebuild it. The structure is *in* the data — functions call other functions, types reference types, configs depend on configs — but nothing exposes it.

Mache does. Point it at data, it figures out the shape. Source code gets parsed by tree-sitter. JSON and YAML get walked. Schema inference discovers the natural groupings without config. The agent can then explore the topology directly: follow call chains, find definitions, read context, write back.

Built for agents first. The design choices — stable node paths across edits, identity-preserving write-back — exist because agents need to reference things reliably across turns. The outputs are human-discernible because the representations are filesystems and SQL, but the topology is the point.

</details>

<details>
<summary>The graph isomorphism argument</summary>

Both structured data and filesystems are graphs. Your JSON object has nodes and edges (containment). Your filesystem has nodes and edges (parent-child). They're isomorphic.

Operating systems never formalized this mapping. Mache does:

- SQL is the graph operator — queries define projections from one topology to another
- Schema defines topology — the formal specification of how source nodes map to filesystem nodes
- The filesystem exposes traversal primitives: `cd` traverses an edge, `ls` enumerates children, `cat` reads node data

See [Architecture](docs/ARCHITECTURE.md) for the full picture.

</details>

## Docs

- [Architecture](docs/ARCHITECTURE.md)
- [Prior Art & Landscape](docs/PRIOR_ART.md)
- [Example Schemas](examples/README.md)
- [ADRs](docs/adr/)
- [Contributing](CONTRIBUTING.md)

## License

Apache 2.0
