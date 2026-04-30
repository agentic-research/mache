# Mache

Mache turns code and structured data into a navigable graph — functions, types, cross-references, and call chains — exposed as MCP tools or a mounted filesystem.

Point it at a codebase. It parses the code, discovers the structure, and lets your agent (or you) explore by following call chains, jumping to definitions, and reading context — instead of grepping through flat files.

> *Mache* (/mɑʃe/ *mah-shay*): from *papier-mâché* — raw material, crushed and remolded into shape.

![Mache Demo](demo.gif)

## Quick start

```bash
git clone https://github.com/agentic-research/mache.git
cd mache && task build && task install
mache serve .
claude mcp add --transport http mache http://localhost:7532/mcp
```

That's the 30-second path. For the full first-run flow — install on Linux/macOS, Claude Desktop / stdio configs, mount as filesystem, write-back, schema inference, troubleshooting — see [GETTING-STARTED.md](GETTING-STARTED.md).

## What it gives an agent

Sixteen MCP tools wrap the projected graph. Thirteen work standalone; three (`semantic_search`, `get_type_info`, `get_diagnostics`) require [ley-line-open](https://github.com/agentic-research/ley-line-open) enrichment. `find_smells` covers nine structural code-smell rules (`dead_code`, `cyclomatic_complexity`, `god_file`, `fan_out_skew`, `untested_function`, …); four of those require an LLO-built `.db`.

For the full tool inventory and capability matrix (which tools need which tables), see [ARCHITECTURE.md § MCP Server](docs/ARCHITECTURE.md#core-abstractions) and [§ Interplay with ley-line-open](docs/ARCHITECTURE.md#interplay-with-ley-line-open).

## How it works

1. **Parse** — tree-sitter parses source into AST nodes (28 languages)
1. **Infer** — schema inference (FCA + greedy entropy) discovers the natural groupings (`functions/`, `types/`, `classes/`)
1. **Link** — cross-reference extraction builds a call graph from identifiers and imports
1. **Project** — the graph is exposed as MCP tools (primary) or a mounted filesystem (optional)

The graph is the same on either path; MCP and the filesystem are two ways to talk to it.

## Status

| Capability                              | Status                                                                        |
| --------------------------------------- | ----------------------------------------------------------------------------- |
| Tree-sitter parsing (28 langs)          | Stable                                                                        |
| MCP server (16 tools, stdio + HTTP)     | Stable                                                                        |
| Cross-references (callers/callees)      | Stable                                                                        |
| `find_smells` (9 structural rules)      | Stable                                                                        |
| NFS mount + write-back                  | Stable                                                                        |
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

[ADR-0011](docs/adr/0011-pointer-abstraction.md) takes this further: every navigable thing in mache (path, token, SHA, range, record, ref) is a Pointer; the graph is a network of pointers; mache resolves them on demand.

See [Architecture](docs/ARCHITECTURE.md) for the full picture.

</details>

## Docs

- [Getting started](GETTING-STARTED.md) — install + first run
- [Architecture](docs/ARCHITECTURE.md) — graph backends, write pipeline, virtual directories, LLO interplay
- [Roadmap](docs/ROADMAP.md) — what's landed, near-term, long-term
- [ADRs](docs/adr/) — Architectural Decision Records
- [Prior art & landscape](docs/PRIOR_ART.md) — what mache builds on, how it compares
- [Example schemas](examples/README.md) — NVD, KEV, Go source, MCP registry
- [Contributing](CONTRIBUTING.md)

## License

Apache 2.0
