---
status: current
covers-version: v0.14.0
last-verified: 2026-07-03
sources-of-truth:
  - CHANGELOG.md
  - docs/ROADMAP.md
  - internal/lang/lang.go
  - cmd/serve_handlers.go
audience: [contributors, maintainers, evaluators]
supersedes: []
---

# Competitive Landscape: Code Intelligence Tools (May 2026)

## Purpose

This document surveys nine tools that overlap with mache's territory — code intelligence, codebase understanding, and AI-agent context engineering. For each tool we capture: what it does, how it works, what it does that mache doesn't, what mache does that it doesn't, and feature gaps worth closing.

## What "Mache" Means Here

The unit of comparison is **mache paired with [ley-line-open](https://github.com/agentic-research/ley-line-open)**, not standalone mache. This pairing is the supported configuration shipped together as a coordinated release wave (v0.8.0 ↔ ley-line-open v0.2.0), and the architectural framing was made explicit in [ADR-0014](adr/0014-mache-in-constellation.md) and the CGO-removal path of [ADR-0012](adr/0012-cgo-removal-migration.md).

- **mache** is the projection engine, MCP server (17 tools), NFS expression layer, and write-back pipeline. Pure Go on the paired path; falls back to a CGO tree-sitter walker when leyline-built `.db` artifacts aren't available.
- **ley-line-open** produces the substrate mache consumes: the AST tables, the capnp binding event log, LSP outputs (`_lsp_defs` / `_lsp_refs` / `_lsp_hover` / `_lsp`), and fastembed embeddings (`all-MiniLM-L6-v2`, the same model Continue.dev ships).

A few mache tools only light up when a ley-line-open `.db` is present: `semantic_search`, `get_type_info`, `get_diagnostics`, and 5 of the 14 `find_smells` rules (`magic_int_in_comparison`, `cyclomatic_complexity`, `long_function`, `long_file`, `duplicate_code` — the `_ast`-dependent subset). The other thirteen MCP tools work standalone. Throughout this doc, "mache" means the paired configuration unless a row or comparison is explicitly called out as standalone-only.

Mache is an **AI-native projection engine**: declarative JSON schemas turn structured data (JSON, SQLite, source code) into a navigable graph, exposed equally as a primary MCP server (17 tools) or an in-process NFS server (`go-nfs` + `billy` — an embedded server, not an OS export). The filesystem is one expression layer; the graph engine is the product. It supports AST decomposition (28 languages via tree-sitter or ley-line-open `leyline parse`), identity-preserving write-back with tree-sitter validation, cross-references (`callers/`, `callees/`, address refs, capnp-backed bindings via the ley-line-open event log), `find_smells` structural rules (qualifier-aware), Louvain community detection, schema inference via FCA + greedy entropy, semantic search over `all-MiniLM-L6-v2` embeddings (via ley-line-open), pre-baked LSP results (defs/refs/hover/diagnostics) without a runtime daemon, an `fsnotify` file watcher for incremental re-ingest (`cmd/serve.go`), and content-addressable substrate identity (`current_root`) for hot-swap.

______________________________________________________________________

## Comparison Matrix

| Capability                       |    Mache (+ leyline)    |      Serena      |  Augment Code   | Sourcegraph Cody |     Cursor      |    Continue.dev    |      Aider       | CodeRabbit |  Greptile  | codebase-memory-mcp |
| -------------------------------- | :---------------------: | :--------------: | :-------------: | :--------------: | :-------------: | :----------------: | :--------------: | :--------: | :--------: | :-----------------: |
| **Real FS mount**                |     NFS (embedded)      |        --        |       --        |        --        |       --        |         --         |        --        |     --     |     --     |         --          |
| **Schema-driven projection**     |           Yes           |        --        |       --        |        --        |       --        |         --         |        --        |     --     |     --     |         --          |
| **Write-back**                   |           Yes           |    Yes (LSP)     |       --        |        --        |       IDE       |         --         |    Git diffs     |     --     |     --     |         --          |
| **AST decomposition**            |        28 langs         |    30+ (LSP)     |       --        |  Graph context   |   AST chunks    | tree-sitter chunks | tree-sitter tags |  ast-grep  |     --     |      64 langs       |
| **Semantic search (embeddings)** | Yes (fastembed via .db) |        --        |  Yes (custom)   |   Yes (hybrid)   | Yes (vector DB) |   Yes (LanceDB)    |        --        |  LanceDB   |     --     |         --          |
| **LSP-grade defs/refs/hover**    | Yes (pre-baked via .db) |  Yes (live LSP)  |       --        |    Yes (SCIP)    |       --        |         --         |        --        |     --     |     --     |         --          |
| **File watcher / live reindex**  |  fsnotify (serve mode)  |    Live (LSP)    |  Yes (seconds)  |    Continuous    | Merkle diffing  |         --         |        --        |     --     | Continuous |      Git-based      |
| **Cross-references**             |     callers/callees     | find_referencing |       --        |    Code graph    |       --        |         --         |  PageRank refs   |  GraphRAG  | Code graph |    25 edge types    |
| **Change impact analysis**       |       get_impact        |        --        |       --        |        --        |       --        |         --         |        --        |  GraphRAG  | Multi-hop  |   detect_changes    |
| **First-contact orientation**    |    get_architecture     |        --        |       --        |        --        |       --        |         --         |     Repo map     |     --     |     --     |  get_architecture   |
| **Structural rules**             | find_smells (14 rules)  |        --        |       --        |        --        |       --        |         --         |        --        |  ast-grep  |     --     |         --          |
| **Multi-repo**                   |           --            |        --        |       Yes       | Yes (@-mentions) |       --        |         --         |        --        |     --     |    Yes     |         --          |
| **PR/diff review**               |           --            |        --        | review_git_diff |        --        |       --        |     CI checks      |        --        | Yes (core) | Yes (core) |   detect_changes    |
| **Community detection**          |         Louvain         |        --        |       --        |        --        |       --        |         --         |        --        |     --     |     --     |       Louvain       |
| **Data-format agnostic**         |    JSON/SQLite/code     |    Code only     |    Code only    |    Code only     |    Code only    |    Code + docs     |    Code only     | Code only  | Code only  |      Code only      |
| **MCP server**                   |           Yes           |       Yes        |       Yes       |        --        |       --        |     MCP client     |        --        |     --     |     --     |         Yes         |
| **Open source**                  |           Yes           |       Yes        |       No        |    Partially     |       No        |        Yes         |       Yes        |     No     |     No     |         Yes         |

______________________________________________________________________

## Detailed Analysis

### 1. Serena (Oraios)

**What it is.** An open-source coding agent toolkit that turns LLMs into full coding agents via LSP-powered semantic code navigation and editing. Exposes an MCP server with symbol-level tools.

**How it works.** Serena built "Solid-LSP", extending Microsoft's multilspy library to provide synchronous LSP calls. It launches actual language servers for each supported language, giving it live, always-current symbol resolution. An alternative backend uses JetBrains IDE analysis via plugin.

**Language support.** 30+ languages via LSP (Python, JS/TS, Rust, Go, Java, C/C++, C#, Haskell, Kotlin, Scala, Zig, etc.).

**Key MCP tools.** `find_symbol`, `find_referencing_symbols`, `insert_after_symbol` -- symbol-level navigation and surgical editing.

**What Serena does that mache doesn't:**

- Live LSP backend — query-time always reflects current disk state. Mache's LSP results are pre-baked into the `.db` at ley-line-open build time, so changes between builds are stale until the next leyline run.
- Symbol-level editing with scope/indentation awareness (mache writes back at byte ranges with tree-sitter validation, not symbol-aware indentation rules).
- JetBrains plugin alternative leverages full IDE analysis.

**What mache does that Serena doesn't:**

- Schema-driven projection — topology is configurable, not fixed.
- Real filesystem mount (NFS) — works with any Unix tool.
- Data-format agnostic — handles JSON, SQLite alongside source code.
- Community detection (Louvain clustering).
- FCA-based schema inference.
- Semantic search via embeddings (`semantic_search` MCP tool, fastembed-backed via ley-line-open).
- `callees/` virtual directories (forward call graph) and the `callers/` self-gating virtual dirs.
- `find_smells` structural rules (9, qualifier-aware).

**Gaps worth closing:**

- Live refresh on the leyline-paired path: mache's `_lsp_*` tables are pre-baked, so query-time correctness depends on rebuild cadence. The fsnotify watcher (`cmd/serve.go:411`) covers source-directory ingestion; closing the gap on the `.db` path needs either an incremental leyline rebuild or a Serena-style on-demand LSP fallback.
- Symbol-level editing primitives. Mache's `write_file` operates at the projected-file level (which is symbol-scoped for AST nodes), but Serena's `insert_after_symbol` style of targeted edit is more ergonomic for some flows.

**Sources:** [GitHub](https://github.com/oraios/serena), [SmartScope review](https://smartscope.blog/en/generative-ai/claude/serena-mcp-coding-agent/)

______________________________________________________________________

### 2. Augment Code

**What it is.** A commercial AI coding assistant whose core differentiator is the "Context Engine" -- a semantic indexing and retrieval system purpose-built for large codebases (400K+ files).

**How it works.** Custom embedding models (not generic OpenAI embeddings) trained specifically for code. Real-time indexing updates within seconds of file changes. Architecture uses Google Cloud (PubSub, BigTable, AI Hypercomputer) with a custom inference stack. As of February 2026, the Context Engine is available as a standalone MCP server usable by any MCP-compatible agent.

**Context Engine MCP tools.** `index_workspace`, `codebase_retrieval` (JSON output), `semantic_search` (markdown output), `get_context_for_prompt`, `review_git_diff`, `review_diff`.

**Performance claims.** 70%+ improvement in agentic coding performance across Claude Code, Cursor, and Codex when Context Engine is added. 200K-token context window, initial indexing of 450K-file monorepo.

**What Augment does that mache doesn't:**

- Code-specific embedding model trained for code retrieval (mache uses ley-line-open's general-purpose `all-MiniLM-L6-v2` — solid for symbol/comment proximity, not custom-tuned for code semantics).
- Real-time incremental re-indexing at seconds-level latency (mache has fsnotify watching for source dirs but ley-line-open `.db` artifacts rebuild on a coarser cadence).
- Multi-repo awareness.
- Git diff review tooling.
- Enterprise certifications (ISO 42001, SOC 2 Type II).

**What mache does that Augment doesn't:**

- Real filesystem mount — no API needed, any tool works.
- Schema-driven projection — user defines topology.
- AST decomposition into navigable directory trees.
- Identity-preserving write-back.
- Cross-reference virtual directories (callers/callees).
- Data-format agnostic (JSON, SQLite, not just code).
- Open source.
- Community detection, `find_smells` structural rules.

**Gaps worth closing:**

- Code-specific embeddings. Worth evaluating whether ley-line-open should ship a code-tuned model alongside the general one, or whether the general model + a re-ranking pass closes the gap cheaper.
- Sub-second incremental indexing on the leyline-paired path (see Serena entry — same gap).

**Sources:** [Augment Code](https://www.augmentcode.com), [Context Engine](https://www.augmentcode.com/context-engine), [Context Engine MCP](https://www.augmentcode.com/product/context-engine-mcp), [SiliconANGLE](https://siliconangle.com/2026/02/06/augment-code-makes-semantic-coding-capability-available-ai-agent/)

______________________________________________________________________

### 3. Sourcegraph Cody

**What it is.** An AI coding assistant built on top of Sourcegraph's decade of code search infrastructure. Uses RAG with context windows up to 1M tokens (Claude Sonnet 4).

**How it works.** Hybrid dense-sparse vector retrieval system tailored for code and documentation. Graph context from Sourcegraph's code graph captures semantic understanding (types, function signatures). "Deep Search" uses a dedicated subagent that explores the codebase broadly and returns a compressed summary, saving tokens in the main agent's context window. Multi-repository awareness via @-mentions.

**What Cody does that mache doesn't:**

- Hybrid dense-sparse vector retrieval (mache's `semantic_search` is dense-only via fastembed).
- Multi-repository @-mention context aggregation.
- Deep Search subagent for broad codebase exploration.
- Decade of SCIP indexers — compiler-grade precision for cross-language navigation.
- 1M-token context windows.

**What mache does that Cody doesn't:**

- Real filesystem mount.
- Schema-driven projection with user-defined topology.
- Identity-preserving write-back.
- Data-format agnostic (JSON, SQLite).
- Community detection.
- Open source (Cody client is open; server infrastructure is proprietary).
- MCP server exposure (Cody is an MCP client, not server).

**Gaps worth closing:**

- Hybrid sparse retrieval. mache's `semantic_search` is dense embeddings only; adding a BM25 / ripgrep-style sparse leg with re-ranking would match Cody's hybrid approach and is local-friendly.
- Multi-repo support: mache currently operates on single-mount targets.
- SCIP-grade precision. ley-line-open's `_lsp_*` tables already give us per-language LSP results; surfacing them through `find_definition` as the default path (rather than tree-sitter heuristics) would close most of the precision gap.

**Sources:** [Sourcegraph Cody docs](https://sourcegraph.com/docs/cody), [Cody GA blog](https://sourcegraph.com/blog/cody-is-generally-available), [Cody in 2026 review](https://devapps.uk/reviews/sourcegraph-cody-in-2026-the-ai-assistant-for-big-code-problems/)

______________________________________________________________________

### 4. Cursor

**What it is.** An AI-native code editor (VS Code fork) valued at $9.9B. Codebase indexing is a core feature that provides semantic understanding for completions, chat, and agent mode.

**How it works.** Files are chunked locally using tree-sitter AST parsing (falls back to regex-based splitting for unsupported languages). Chunks are sent to Cursor's server for embedding (OpenAI or custom model). Embeddings stored in Turbopuffer (remote vector database). Query-time nearest-neighbor search retrieves semantically similar chunks. A Merkle tree of file hashes enables efficient incremental sync. Cross-org deduplication exploits 92% similarity between team members' repos.

**What Cursor does that mache doesn't:**

- Remote vector DB (Turbopuffer) and cross-user deduplication — mache's embeddings live in the ley-line-open `.db`, locally.
- Real-time incremental indexing via Merkle tree diffing — finer-grained than mache's per-file fsnotify watcher.
- IDE-integrated completions, inline editing, agent mode (a different product category).
- Multi-step reasoning with dependency tracking (v3.2 context engine, 2026).

**What mache does that Cursor doesn't:**

- Real filesystem mount — usable outside any IDE.
- Schema-driven projection.
- Identity-preserving write-back through the filesystem.
- Data-format agnostic.
- Cross-reference virtual directories.
- Community detection, `find_smells` structural rules.
- MCP server (Cursor is a client, not server).
- Open source.
- Self-hosted / no cloud dependency.

**Gaps worth closing:**

- Merkle-tree-style change detection. mache's fsnotify watcher re-ingests per-file; a content-hash-based diff would let it skip unchanged files inside a touched directory.

**Sources:** [Cursor](https://cursor.com/), [How Cursor Indexes Codebases (Engineer's Codex)](https://read.engineerscodex.com/p/how-cursor-indexes-codebases-fast), [Cursor Deep Dive 2026](https://dasroot.net/posts/2026/02/cursor-ai-deep-dive-technical-architecture-advanced-features-best-practices/)

______________________________________________________________________

### 5. Continue.dev

**What it is.** An open-source AI coding assistant for VS Code and JetBrains that lets you choose your own model and customize everything. Recently pivoted to emphasize "source-controlled AI checks, enforceable in CI" via the Continue CLI.

**How it works.** Codebase indexing combines three approaches: tree-sitter AST parsing for structurally aware chunking, embeddings via transformers.js (default: all-MiniLM-L6-v2, runs locally), and keyword search via ripgrep. Embeddings stored locally in LanceDB (embedded TypeScript vector DB). Re-ranking refines initial retrieval (25 candidates down to 5). Supports MCP as a client for external tool integration. `.continue/rules/` directory enables team-shared AI configuration.

**What Continue does that mache doesn't:**

- Re-ranking pipeline for retrieval refinement (25 → 5 candidates).
- IDE integration (VS Code, JetBrains).
- CI-enforceable AI checks via CLI.
- Configurable model selection (local or cloud) — mache's embedding model is whatever ley-line-open shipped.
- MCP client integration with external services (GitHub, Sentry, Linear).

**What mache does that Continue doesn't:**

- Real filesystem mount.
- Schema-driven projection.
- Identity-preserving write-back.
- Data-format agnostic.
- Cross-reference virtual directories (`callers/`, `callees/`).
- Community detection, `find_smells`, `get_impact`, `get_architecture`.
- MCP **server** (Continue is a client).
- Deeper AST decomposition (full function/type directory trees vs chunks).

> Note: mache and Continue both use `all-MiniLM-L6-v2` as the local embedding model (Continue via transformers.js, mache via ley-line-open's fastembed crate). The retrieval surface differs (mache exposes a `semantic_search` MCP tool over the projected graph; Continue serves chunks into an in-IDE chat context), but the underlying embeddings are the same.

**Gaps worth closing:**

- Re-ranking. A two-stage retrieve-then-rerank pipeline would improve `semantic_search` quality without changing the embedding model.
- Configurable embedding model. Today the model is fixed by what ley-line-open ships; letting users plug in code-tuned models would be a small ley-line-open change.

**Sources:** [Continue.dev](https://www.continue.dev/), [GitHub](https://github.com/continuedev/continue), [Codebase indexing docs](https://docs.continue.dev/walkthroughs/codebase-embeddings), [LanceDB integration blog](https://lancedb.com/blog/the-future-of-ai-native-development-is-local-inside-continues-lancedb-powered-evolution/)

______________________________________________________________________

### 6. Aider

**What it is.** An open-source, terminal-based AI pair programming tool that works directly with Git. Known for its "repo map" -- a compact, ranked representation of the codebase that fits in the LLM's context window.

**How it works.** The repo map uses tree-sitter to extract definitions and references from all source files, builds a NetworkX MultiDiGraph where nodes are files and edges are cross-file dependencies, then runs PageRank with personalization to rank symbols by importance. A binary search algorithm packs the highest-value symbols into a configurable token budget (default 1K tokens, user-adjustable via `--map-tokens`). The map shows function signatures, class definitions, and file structure -- not full source code. Aider proposes changes as Git diffs.

**What Aider does that mache doesn't:**

- PageRank-based importance ranking for context selection.
- Token-budget-aware context packing (binary search for optimal fill).
- Git-native workflow (changes as tracked diffs/commits).
- 100+ language support (tree-sitter).
- Efficient context utilization: 4.3-6.5% of context window vs 54-70% for iterative search agents.

**What mache does that Aider doesn't:**

- Real filesystem mount.
- Schema-driven projection.
- Identity-preserving write-back (surgical byte-range replacement, not whole-file diffs).
- Data-format agnostic.
- Cross-reference virtual directories with navigable content.
- Community detection, `find_smells`, semantic search via embeddings.
- MCP server exposure.
- On-demand content resolution (lazy loading).

**Gaps worth closing:**

- Importance ranking: Aider's PageRank over the reference graph is a simple, effective heuristic for "what matters most." Mache's `node_refs` table already has the data; adding a PageRank-based ranking to `search` or `get_overview` would improve context selection. This is the highest-leverage open gap in this survey.
- Token-budget-aware output: mache's MCP tools return full results; a `--max-tokens` knob with priority-packing would help low-context-budget agents.
- Language breadth: 28 → 100+. Adding more tree-sitter grammars is mechanical; ley-line-open's per-language LSP pipeline is the harder side to extend.

**Sources:** [Aider](https://aider.chat/), [Repo map docs](https://aider.chat/docs/repomap.html), [Building a better repo map](https://aider.chat/2023/10/22/repomap.html), [GitHub](https://github.com/Aider-AI/aider)

______________________________________________________________________

### 7. CodeRabbit

**What it is.** An AI-first pull request reviewer that runs automatically on every PR. Combines AST analysis (ast-grep), GraphRAG for dependency tracking, and generative AI feedback. Reviews 2M+ repositories, 13M+ PRs processed.

**How it works.** Multi-layered analysis: AST evaluation via ast-grep, SAST (static analysis), and LLM-generated feedback. Builds a code graph (GraphRAG) by traversing repositories to identify cross-file dependencies. LanceDB integration for semantic search at scale (sub-second latency at 50K+ daily PRs). The bot learns from team review patterns. Expanding context with runtime traces, CI/CD data, and observability signals (2026 roadmap).

**What CodeRabbit does that mache doesn't:**

- Automated PR review with line-by-line comments
- GraphRAG for cross-file dependency impact analysis ("this breaks 15 callers across 8 files")
- ast-grep pattern matching for code smell detection
- Learning from team review patterns
- Integration with project management (Jira, Linear, GitHub Issues)
- Runtime/CI/CD/observability context (2026 roadmap)
- Semantic search via LanceDB

**What mache does that CodeRabbit doesn't:**

- Real filesystem mount.
- Schema-driven projection.
- Identity-preserving write-back.
- Data-format agnostic.
- Navigable directory-tree decomposition.
- Community detection.
- MCP server exposure.
- Open source.

**Gaps worth closing:**

- PR-review productization. mache exposes the primitives — `get_impact` (already shipped) traces change blast radius through `callers/callees`, and `find_smells` flags structural issues. What's missing is the GitHub PR workflow on top: a bot that reads diffs, calls `get_impact` + `find_smells`, and posts inline review comments. (`.github/workflows/find-smells.yml` exists for advisory PR comments — a starting point.)
- ast-grep integration: ast-grep's pattern-matching could complement tree-sitter queries for more expressive structural search beyond the 9 `find_smells` rules.

**Sources:** [CodeRabbit](https://www.coderabbit.ai/), [CodeRabbit docs](https://docs.coderabbit.ai/), [Architecture blog](https://learnwithparam.com/blog/architecting-coderabbit-ai-agent-at-scale), [Google Cloud blog](https://cloud.google.com/blog/products/ai-machine-learning/how-coderabbit-built-its-ai-code-review-agent-with-google-cloud-run)

______________________________________________________________________

### 8. Greptile

**What it is.** An AI codebase understanding platform focused on code review. Indexes entire repositories into a code graph, uses multi-hop investigation for reviews. YC W24, $25M Series A at $180M valuation (Sep 2025).

**How it works.** Builds a complete graph of every code element in your repository -- functions, variables, classes, files, directories, and their connections. Continuously updated as code changes. Reviews use multi-hop investigation: trace dependencies, check git history, follow leads across files. Version 3 (late 2025) uses Anthropic Claude Agent SDK for autonomous investigation. Supports multi-repo indexing. SOC2 Type II compliant, self-hosting available.

**Performance.** 82% catch rate in independent benchmarks (vs Cursor's 58%), though with higher false positive rate (11 vs CodeRabbit's 2).

**What Greptile does that mache doesn't:**

- Full code graph with continuous updates
- Multi-hop investigation (traces across files and git history)
- Automated PR review as primary use case
- Multi-repo indexing
- Agent-based autonomous investigation (Claude Agent SDK)
- Git history analysis

**What mache does that Greptile doesn't:**

- Real filesystem mount.
- Schema-driven projection.
- Identity-preserving write-back.
- Data-format agnostic.
- Community detection, semantic search via embeddings, `find_smells` structural rules.
- MCP server exposure.
- Open source.
- Self-hostable without enterprise plan.

**Gaps worth closing:**

- Git history as a first-class data source. mache treats git as out of band today; a `git/` virtual directory exposing commits/blame per construct would let agents reason about change history without leaving the filesystem. ADR-0007 sketches this; not yet implemented.
- Multi-hop investigation. `get_impact` traces blast radius one hop today; a transitive `trace/` virtual directory (mache-ok2, queued in [ROADMAP.md](ROADMAP.md)) would match Greptile's depth.

**Sources:** [Greptile](https://www.greptile.com/), [Graph-based context docs](https://www.greptile.com/docs/how-greptile-works/graph-based-codebase-context), [YC profile](https://www.ycombinator.com/companies/greptile), [Benchmarks](https://www.greptile.com/benchmarks)

______________________________________________________________________

### 9. codebase-memory-mcp (DeusData)

**What it is.** An open-source, high-performance code intelligence MCP server that indexes codebases into a persistent SQLite-backed knowledge graph. Single static binary, zero dependencies. The closest direct competitor to mache.

**How it works.** 18-pass indexing pipeline using tree-sitter: structure, definitions, decorators, registry, inheritance, imports, return types, calls, usages, type refs, throws, reads/writes, configures, flush, tests, communities, HTTP links, config linking. Produces a property graph with 13 node labels and 25 edge types. Custom Cypher-like query executor (200-row cap). Auto-syncs via git-based file change detection. Background watcher for ongoing updates.

**Language support.** 64 languages via vendored tree-sitter C grammars (CGO required).

**Key MCP tools.** 12 tools including `get_architecture` (10 architectural aspects), `manage_adr`, `detect_changes` (git diff impact), `find_definition`, `find_callers`, `get_communities`. 4 auto-triggering skills (exploring, tracing, quality, reference).

**Latest (v0.4.10, March 2026).** Fixed OOM on startup (watcher opened every indexed project's DB). Wolfram Language support added in v0.4.6.

**Growth context.** 628 stars in 16 days (created Feb 24, 2026). Solo developer project. Reddit-driven viral growth (284 stars in one day). Star-to-watcher ratio of 125:1 suggests star-and-forget pattern.

**What codebase-memory-mcp does that mache doesn't:**

- 64 languages vs mache's 28.
- Risk-labeled call traces.
- `detect_changes` — explicit git-diff → blast-radius MCP tool (mache has the building blocks via `get_impact` but no git-diff entry point yet; queued as `mache-bsq` in [ROADMAP.md](ROADMAP.md)).
- Custom Cypher-like query language. mache exposes SQL directly against the SQLite-backed graph (via the `mache_refs` vtab) — different ergonomics; Cypher is more readable for graph traversals.
- 25 edge types (CALLS, HTTP_CALLS, IMPORTS, READS, WRITES, THROWS, etc.). mache's refs graph is single-edge (token → node), with edge semantics implicit in the projection schema.
- ADR management (`manage_adr`).
- 4 auto-triggering skills based on conversation context.

**What mache does that codebase-memory-mcp doesn't:**

- Real filesystem mount (NFS).
- Schema-driven projection — user-defined topology, not hardcoded graph schema.
- Identity-preserving write-back.
- Data-format agnostic (JSON, SQLite, not just code).
- On-demand content resolution (lazy loading, not bulk indexing).
- FCA-based schema inference.
- `callees/` forward call graph (not just callers).
- Configurable AST queries per language.
- Doc comment extraction.
- Context files (imports/globals per scope).
- Semantic search via embeddings (`semantic_search`).
- LSP-grade defs/refs/hover (via ley-line-open `_lsp_*` tables).
- `find_smells` structural rules with capnp event-log readthrough.

Note on tool parity: mache has `get_architecture` (first-contact orientation), `get_impact` (change blast radius), `get_communities` (Louvain), and a background fsnotify watcher in serve mode. These were sometimes treated as gaps in older comparisons; they shipped in v0.7.x–v0.8.0.

**Gaps worth closing:**

- `detect_changes`-style git-diff entry point. mache has `get_impact`; wiring a git-diff parser in front of it would match codebase-memory-mcp's UX.
- Multi-edge-type refs graph. The single-token-edge model is simple but loses information (a `CALLS` edge and an `IMPORTS` edge collapse into the same token resolution). Schema-driven edge typing is a plausible extension.
- Language breadth: 28 → 64.

**Sources:** [GitHub](https://github.com/DeusData/codebase-memory-mcp), [Documentation site](https://deusdata.github.io/codebase-memory-mcp/)

______________________________________________________________________

### 10. stack-graphs (GitHub)

**What it is.** GitHub's framework for declaratively building "stack graphs" — a tree-sitter-based name resolution model that composes across files (and in principle across languages). Powers GitHub's code-navigation precision tier for the languages they ship support for.

**How it works.** Per-language scope/path rules expressed in a tree-sitter query DSL. Each source file is independently lowered to an "open" partial graph; name resolution is then a pushdown-automaton walk over the composed graph. Polyglot in principle because composition is a graph operation, not a compiler invocation. Languages shipped today: Python, JavaScript, TypeScript, Java.

**Why it's noted here separately.** Two independent reviews of mache's planned cross-language references work (May 2026) identified stack-graphs as the **closest neighbor** mache hasn't been explicitly comparing itself to. The earlier per-tool matrix focuses on AI-agent context engineering tools; stack-graphs is a structural code-intelligence library — different category, adjacent slice.

**What stack-graphs does that mache doesn't:**

- Provably-correct *incremental* composition of partial graphs under name resolution. Mache's `CompositeGraph` mounts subgraphs under path prefixes (a coproduct); stack-graphs composes scope graphs over name overlaps (closer to a fiber product). The math is more rigorous.
- Pure-structural cross-file resolution without an LSP or compiler. Mache reaches binding-fidelity by consuming ley-line-open's `_lsp_*` tables; stack-graphs reaches similar fidelity from tree-sitter alone via the scope-graph formalism.
- A declarative rule language for *scope* (open/closed scopes, push/pop symbol stack edges) — analogous to mache's `RegisterAddressRefQuery` but for intra-language structural resolution rather than cross-system address refs.

**What mache (+ leyline) does that stack-graphs doesn't:**

- Fidelity stratification (ADR-0013 poset). Stack-graphs is monolithic — every binding is the same kind of binding.
- Data-format agnostic (JSON, SQLite, source). Stack-graphs is code only.
- Real filesystem mount (NFS). Stack-graphs is a library, not an agent surface.
- Schema-driven projection. Stack-graphs gives you name resolution, not a configurable directory topology.
- Identity-preserving write-back.
- MCP server.
- Cross-*system* address refs (`mod:`, `npm:`, `git:`, OCI, OpenAPI `$ref`) — stack-graphs resolves names within the source code universe; mache's intended cross-language refs work spans to non-source artifacts.

**Honest positioning vs stack-graphs.** Where mache's planned cross-language refs work overlaps with stack-graphs is the *intra-source polyglot* case — and stack-graphs occupies that slice cleanly with a stronger formal model. Mache's defensible adjacent slice is **graph-integrated address-level cross-system refs** (Terraform module → Go repo, `package.json` → npm package, `Dockerfile FROM` → OCI image), where the references cross artifact boundaries rather than just file boundaries within one language family. Polyglot at the address layer, not the type layer — see ADR-0016 (proposed).

**Sources:** [stack-graphs GitHub](https://github.com/github/stack-graphs), [stack-graphs paper / blog](https://github.blog/2021-12-09-introducing-stack-graphs/).

______________________________________________________________________

## Cross-Cutting Themes

### 1. PageRank-style Importance Ranking

Aider's PageRank over the reference graph ranks symbols by importance and packs the highest-value ones into a configurable token budget. Token efficiency is 4.3–6.5% of context window versus 54–70% for iterative search agents — a 10x gap. Mache's `node_refs` table has the graph; ranking is missing.

**Recommendation:** Add a PageRank pass over `node_refs` and expose it as a sort key on `search` / `get_overview`. Highest-leverage open gap in this survey.

### 2. Hybrid Sparse + Dense Retrieval

`semantic_search` is dense embeddings only (fastembed `all-MiniLM-L6-v2` via ley-line-open). Cody, Continue, and CodeRabbit all blend dense with sparse (BM25, keyword) and re-rank. Pure dense retrieval misses exact-match queries that sparse trivially solves.

**Recommendation:** Add a ripgrep-style sparse leg to `semantic_search` and a re-ranking step. Both are local-friendly.

### 3. Incremental Indexing on the Leyline-Paired Path

Mache's fsnotify watcher (`cmd/serve.go:411`) covers source-directory ingestion. The leyline-paired path is rebuilt on a coarser cadence — `_lsp_*` and embedding tables are stale until the next `leyline parse` run. Augment claims sub-second; Cursor uses Merkle-tree diffing.

**Recommendation:** Either an incremental `leyline parse` mode (tracked upstream), or an on-demand LSP fallback in mache when the `.db` is stale relative to source mtime.

### 4. Multi-Repo Awareness

Augment, Cody, and Greptile support cross-repository context. Mache operates on single mount targets, though it can mount multiple sources simultaneously via composed schemas.

**Recommendation:** Lower priority. The `--mount` / cross-repo serve path (ARCHITECTURE.md § Cross-repo serve) is the seed for this; making it a first-class agent-facing primitive is queued.

### 5. Language Breadth

Aider supports 100+ via tree-sitter; codebase-memory-mcp claims 64; Serena offers 30+ via LSP. Mache supports 28 ([`internal/lang/lang.go`](../internal/lang/lang.go)).

**Recommendation:** Adding tree-sitter grammars is mechanical (one entry in the language registry); the harder side is extending ley-line-open's per-language LSP pipeline to match. Prioritize by user demand.

### 6. Git History as a Data Source

Greptile threads git history into investigations; codebase-memory-mcp surfaces commit context. Mache treats git as out of band. ADR-0007 sketches "git object graph as FS projection" but it isn't implemented.

**Recommendation:** A `git/` virtual directory projecting commits/blame per construct would let agents reason about change history through the same filesystem interface. Natural extension of the schema model.

### 7. Code-Tuned Embeddings

Augment ships custom code-trained embeddings; mache uses ley-line-open's general-purpose `all-MiniLM-L6-v2` (same model Continue uses). Code-tuned models tend to do better on symbol/identifier proximity.

**Recommendation:** Worth evaluating whether ley-line-open should ship a code-tuned model alongside, or whether re-ranking closes the gap cheaper.

______________________________________________________________________

## Positioning Summary

Mache (+ ley-line-open) occupies a unique position: it is the **only tool** that combines schema-driven projection, real filesystem mount, AST decomposition, identity-preserving write-back, and data-format agnosticism. No competitor offers even three of these five properties together.

The primary remaining gaps:

1. **PageRank-style importance ranking** — Aider's biggest win; the graph data is already in mache.
1. **Hybrid sparse + dense retrieval with re-ranking** — `semantic_search` is dense-only today.
1. **Incremental indexing on the leyline-paired path** — the source-dir watcher works; the `.db` rebuild is coarser.
1. **Multi-repo first-class support** — the primitive exists (`--mount`); not yet a first-class agent surface.
1. **Language breadth** — 28 vs 30–100+.
1. **Git history as a data source** — ADR-0007 sketched, not implemented.

Strengths competitors cannot easily replicate:

1. **Real filesystem mount** — requires deep OS integration (NFS), not an API wrapper.
1. **Schema-driven projection** — topology is configurable, not hardcoded.
1. **Data-format agnosticism** — no competitor handles JSON + SQLite + source code through one engine.
1. **Identity-preserving write-back** — validate, format, splice, update — through the filesystem.
1. **FCA schema inference** — automatic topology derivation from data structure.
1. **Substrate-identity hot-swap** — `current_root` (BLAKE3 of the arena payload) gives readers cache-aware swap detection without coordinator state, paired with ley-line-open as a coordinated release wave.

## What Was Wrong in Earlier Versions of This Doc

For honesty's sake: prior revisions of this document treated several capabilities as gaps when they actually existed. Recording the corrections here so the pattern doesn't recur:

- `semantic_search` was listed as missing. It exists ([`cmd/serve_handlers.go:85`](../cmd/serve_handlers.go), `all-MiniLM-L6-v2` via ley-line-open fastembed).
- LSP support was framed as a Serena-only advantage. Mache reads `_lsp_defs` / `_lsp_refs` / `_lsp_hover` / `_lsp` from the ley-line-open `.db`; the tradeoff is build-time vs query-time, not absent vs present.
- `get_impact` and `get_architecture` were recommended as future tools. Both ship today.
- The file watcher was listed as a recommendation. `internal/ingest/watcher.go` exists and is wired in `cmd/serve.go:411`.
- "Mache supports 8 languages." <!-- docs-lint:ignore --> Today: 28, per [`internal/lang/lang.go`](../internal/lang/lang.go).

The underlying reason: the doc was written before ley-line-open was fully paired in, and it evaluated mache as a standalone product instead of as the projection layer in a paired system. The constellation framing ([ADR-0014](adr/0014-mache-in-constellation.md)) is the correction.
