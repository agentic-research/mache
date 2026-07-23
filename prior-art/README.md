# Prior art

How mache relates to adjacent code-intelligence systems. Each claim cites a primary source; this index is a cross-cutting matrix, with per-system notes in sibling files.

## Cross-cutting matrix

| System                                                   | Semantic source                                                                                | Live vs precomputed                              | Degrades without a language server?                                                      | Surface                  |
| -------------------------------------------------------- | ---------------------------------------------------------------------------------------------- | ------------------------------------------------ | ---------------------------------------------------------------------------------------- | ------------------------ |
| **mache**                                                | tree-sitter base **+** LSP enrichment tier (rust-analyzer / gopls / … via the ley-line daemon) | live (daemon, on-demand) **and** pre-baked `.db` | **Yes** — tree-sitter tier answers `find_callers`/`search`/`find_definition` with no LSP | MCP tools + mountable FS |
| [Serena](https://github.com/oraios/serena)               | language servers (via [multilspy](https://github.com/microsoft/multilspy))                     | live, per-query                                  | No — LSP-or-nothing                                                                      | MCP tools                |
| [entire-graph](https://github.com/entireio/entire-graph) | tree-sitter/provider heuristics with typed relation records                                    | precomputed streamed snapshots                   | Yes — many relations are explicitly heuristic                                            | CLI graph/search         |
| Sourcegraph / GitHub code-nav                            | [SCIP](https://github.com/sourcegraph/scip) / LSIF indexes                                     | precomputed (batch-baked)                        | n/a (no live server)                                                                     | web + API                |
| ctags / tree-sitter MCP servers                          | syntactic only                                                                                 | n/a                                              | yes (no semantic tier at all)                                                            | varies                   |

## Serena — the closest "does the LSP thing" comparison

Serena is LSP-native: it drives real language servers through multilspy and answers symbol / definition / reference / hover requests **live, per call** ([Serena README](https://github.com/oraios/serena)). That is the same primitive mache's `get_type_info` / `get_diagnostics` now use (rust-analyzer + gopls verified end-to-end, mache-6584a0).

Where mache differs:

1. **LSP is one tier, not the floor.** mache projects a tree-sitter graph first; LSP enrichment layers compiler-grade hover/defs/refs **on top**. With no language server present, `find_callers` / `find_definition` / `search` still work off the tree-sitter tier — Serena returns nothing without a server.
1. **Projected, not just proxied.** mache writes LSP output into a queryable SQLite/graph (`_lsp_hover`, `_lsp_defs`, `_lsp_refs`, …) and a content-addressed `.db`, so results are cacheable, portable (`mache cache`), and joinable against structural data — rather than a live pass-through per request.
1. **Enrichment is pluggable.** LSP sits beside other ley-line passes (tree-sitter, HDC, embeddings); the same daemon/op surface adds tiers without changing the consumer contract.

Net: the *LSP capability* is not novel (Serena predates it); the *tiering + projection + graceful degradation* is the distinguishing frame.

## SCIP / LSIF systems

Sourcegraph and GitHub code navigation consume **precomputed** semantic indexes ([SCIP](https://github.com/sourcegraph/scip)) — the same class of data an LSP server produces, serialized and batch-baked rather than queried live. mache can ingest a pre-baked `.db` (the precomputed mode) **or** enrich live (the Serena mode); it spans both.
