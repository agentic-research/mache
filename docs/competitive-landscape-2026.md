# Competitive Landscape: Code Intelligence Tools (March 2026)

## Purpose

This document surveys nine tools that overlap with mache's territory -- code intelligence, codebase understanding, and AI-agent context engineering. For each tool we capture: what it does, how it works, what it does that mache doesn't, what mache does that it doesn't, and feature gaps worth closing.

Mache is a **projection engine**: declarative JSON schemas drive how structured data (JSON, SQLite, source code) is mounted as a real POSIX filesystem (NFS/FUSE) or exposed via MCP. It supports AST decomposition (8 languages via tree-sitter), identity-preserving write-back, cross-references (`callers/`, `callees/`), community detection, and schema inference via FCA.

______________________________________________________________________

## Comparison Matrix

| Capability                       |      Mache       |      Serena      |  Augment Code   | Sourcegraph Cody |     Cursor      |    Continue.dev    |      Aider       | CodeRabbit |  Greptile  | codebase-memory-mcp |
| -------------------------------- | :--------------: | :--------------: | :-------------: | :--------------: | :-------------: | :----------------: | :--------------: | :--------: | :--------: | :-----------------: |
| **Real FS mount**                |     NFS/FUSE     |        --        |       --        |        --        |       --        |         --         |        --        |     --     |     --     |         --          |
| **Schema-driven projection**     |       Yes        |        --        |       --        |        --        |       --        |         --         |        --        |     --     |     --     |         --          |
| **Write-back**                   |       Yes        |    Yes (LSP)     |       --        |        --        |       IDE       |         --         |    Git diffs     |     --     |     --     |         --          |
| **AST decomposition**            |     8 langs      |    30+ (LSP)     |       --        |  Graph context   |   AST chunks    | tree-sitter chunks | tree-sitter tags |  ast-grep  |     --     |      64 langs       |
| **Semantic search (embeddings)** |        --        |        --        |  Yes (custom)   |   Yes (hybrid)   | Yes (vector DB) |   Yes (LanceDB)    |        --        |  LanceDB   |     --     |         --          |
| **Cross-references**             | callers/callees  | find_referencing |       --        |    Code graph    |       --        |         --         |  PageRank refs   |  GraphRAG  | Code graph |    25 edge types    |
| **Multi-repo**                   |        --        |        --        |       Yes       | Yes (@-mentions) |       --        |         --         |        --        |     --     |    Yes     |         --          |
| **PR/diff review**               |        --        |        --        | review_git_diff |        --        |       --        |     CI checks      |        --        | Yes (core) | Yes (core) |   detect_changes    |
| **Community detection**          |     Louvain      |        --        |       --        |        --        |       --        |         --         |        --        |     --     |     --     |       Louvain       |
| **Data-format agnostic**         | JSON/SQLite/code |    Code only     |    Code only    |    Code only     |    Code only    |    Code + docs     |    Code only     | Code only  | Code only  |      Code only      |
| **MCP server**                   |       Yes        |       Yes        |       Yes       |        --        |       --        |     MCP client     |        --        |     --     |     --     |         Yes         |
| **Open source**                  |       Yes        |       Yes        |       No        |    Partially     |       No        |        Yes         |       Yes        |     No     |     No     |         Yes         |

______________________________________________________________________

## Detailed Analysis

### 1. Serena (Oraios)

**What it is.** An open-source coding agent toolkit that turns LLMs into full coding agents via LSP-powered semantic code navigation and editing. Exposes an MCP server with symbol-level tools.

**How it works.** Serena built "Solid-LSP", extending Microsoft's multilspy library to provide synchronous LSP calls. It launches actual language servers for each supported language, giving it live, always-current symbol resolution. An alternative backend uses JetBrains IDE analysis via plugin.

**Language support.** 30+ languages via LSP (Python, JS/TS, Rust, Go, Java, C/C++, C#, Haskell, Kotlin, Scala, Zig, etc.).

**Key MCP tools.** `find_symbol`, `find_referencing_symbols`, `insert_after_symbol` -- symbol-level navigation and surgical editing.

**What Serena does that mache doesn't:**

- Live LSP backend means zero stale data -- always reflects disk state
- 30+ language support vs mache's 8
- Symbol-level editing with scope/indentation awareness
- JetBrains plugin alternative leverages full IDE analysis

**What mache does that Serena doesn't:**

- Schema-driven projection -- topology is configurable, not fixed
- Real filesystem mount (NFS/FUSE) -- works with any Unix tool
- Data-format agnostic -- handles JSON, SQLite alongside source code
- Community detection (Louvain clustering)
- FCA-based schema inference
- `callees/` virtual directories (forward call graph)

**Gaps worth closing:**

- Language breadth: mache's 8 tree-sitter languages vs Serena's 30+ LSP languages. Adding more tree-sitter grammars is straightforward.
- Live refresh: Serena's LSP queries always reflect current disk state. Mache requires re-mount or watcher-based re-ingest for source changes.

**Sources:** [GitHub](https://github.com/oraios/serena), [SmartScope review](https://smartscope.blog/en/generative-ai/claude/serena-mcp-coding-agent/)

______________________________________________________________________

### 2. Augment Code

**What it is.** A commercial AI coding assistant whose core differentiator is the "Context Engine" -- a semantic indexing and retrieval system purpose-built for large codebases (400K+ files).

**How it works.** Custom embedding models (not generic OpenAI embeddings) trained specifically for code. Real-time indexing updates within seconds of file changes. Architecture uses Google Cloud (PubSub, BigTable, AI Hypercomputer) with a custom inference stack. As of February 2026, the Context Engine is available as a standalone MCP server usable by any MCP-compatible agent.

**Context Engine MCP tools.** `index_workspace`, `codebase_retrieval` (JSON output), `semantic_search` (markdown output), `get_context_for_prompt`, `review_git_diff`, `review_diff`.

**Performance claims.** 70%+ improvement in agentic coding performance across Claude Code, Cursor, and Codex when Context Engine is added. 200K-token context window, initial indexing of 450K-file monorepo.

**What Augment does that mache doesn't:**

- Semantic search via custom code-trained embeddings
- Real-time incremental re-indexing (seconds, not re-mount)
- Multi-repo awareness
- Git diff review tooling
- Enterprise certifications (ISO 42001, SOC 2 Type II)

**What mache does that Augment doesn't:**

- Real filesystem mount -- no API needed, any tool works
- Schema-driven projection -- user defines topology
- AST decomposition into navigable directory trees
- Identity-preserving write-back
- Cross-reference virtual directories (callers/callees)
- Data-format agnostic (JSON, SQLite, not just code)
- Open source
- Community detection

**Gaps worth closing:**

- Semantic search: mache has no embedding-based retrieval. Adding vector search (e.g., via LanceDB or SQLite-vec) for the `search` MCP tool would close this gap.
- Incremental re-indexing: mache currently requires re-mount for source changes.

**Sources:** [Augment Code](https://www.augmentcode.com), [Context Engine](https://www.augmentcode.com/context-engine), [Context Engine MCP](https://www.augmentcode.com/product/context-engine-mcp), [SiliconANGLE](https://siliconangle.com/2026/02/06/augment-code-makes-semantic-coding-capability-available-ai-agent/)

______________________________________________________________________

### 3. Sourcegraph Cody

**What it is.** An AI coding assistant built on top of Sourcegraph's decade of code search infrastructure. Uses RAG with context windows up to 1M tokens (Claude Sonnet 4).

**How it works.** Hybrid dense-sparse vector retrieval system tailored for code and documentation. Graph context from Sourcegraph's code graph captures semantic understanding (types, function signatures). "Deep Search" uses a dedicated subagent that explores the codebase broadly and returns a compressed summary, saving tokens in the main agent's context window. Multi-repository awareness via @-mentions.

**What Cody does that mache doesn't:**

- Hybrid vector retrieval (dense + sparse embeddings)
- Multi-repository @-mention context aggregation
- Deep Search subagent for broad codebase exploration
- Decade of code graph infrastructure (SCIP indexers for precise code navigation)
- 1M-token context windows

**What mache does that Cody doesn't:**

- Real filesystem mount
- Schema-driven projection with user-defined topology
- Identity-preserving write-back
- Data-format agnostic (JSON, SQLite)
- Community detection
- Open source (Cody client is open; server infrastructure is proprietary)
- MCP server exposure (Cody is an MCP client, not server)

**Gaps worth closing:**

- Precise code navigation: Sourcegraph's SCIP indexers provide compiler-grade precision for go-to-definition and find-references. Mache's tree-sitter approach is faster but less precise for complex type resolution.
- Multi-repo support: mache currently operates on single-mount targets.

**Sources:** [Sourcegraph Cody docs](https://sourcegraph.com/docs/cody), [Cody GA blog](https://sourcegraph.com/blog/cody-is-generally-available), [Cody in 2026 review](https://devapps.uk/reviews/sourcegraph-cody-in-2026-the-ai-assistant-for-big-code-problems/)

______________________________________________________________________

### 4. Cursor

**What it is.** An AI-native code editor (VS Code fork) valued at $9.9B. Codebase indexing is a core feature that provides semantic understanding for completions, chat, and agent mode.

**How it works.** Files are chunked locally using tree-sitter AST parsing (falls back to regex-based splitting for unsupported languages). Chunks are sent to Cursor's server for embedding (OpenAI or custom model). Embeddings stored in Turbopuffer (remote vector database). Query-time nearest-neighbor search retrieves semantically similar chunks. A Merkle tree of file hashes enables efficient incremental sync. Cross-org deduplication exploits 92% similarity between team members' repos.

**What Cursor does that mache doesn't:**

- Embedding-based semantic search (Turbopuffer vector DB)
- Real-time incremental indexing via Merkle tree diffing
- IDE-integrated completions, inline editing, agent mode
- Multi-step reasoning with dependency tracking (v3.2 context engine, 2026)
- Cross-user deduplication for teams

**What mache does that Cursor doesn't:**

- Real filesystem mount -- usable outside any IDE
- Schema-driven projection
- Identity-preserving write-back through the filesystem
- Data-format agnostic
- Cross-reference virtual directories
- Community detection
- MCP server (Cursor is a client, not server)
- Open source
- Self-hosted / no cloud dependency

**Gaps worth closing:**

- Semantic search via embeddings is the most consistent gap across competitors. Cursor's approach (tree-sitter chunking + vector DB) is well-proven.
- Incremental sync: Cursor's Merkle tree approach for detecting changed files is elegant and could inform mache's watcher design.

**Sources:** [Cursor](https://cursor.com/), [How Cursor Indexes Codebases (Engineer's Codex)](https://read.engineerscodex.com/p/how-cursor-indexes-codebases-fast), [Cursor Deep Dive 2026](https://dasroot.net/posts/2026/02/cursor-ai-deep-dive-technical-architecture-advanced-features-best-practices/)

______________________________________________________________________

### 5. Continue.dev

**What it is.** An open-source AI coding assistant for VS Code and JetBrains that lets you choose your own model and customize everything. Recently pivoted to emphasize "source-controlled AI checks, enforceable in CI" via the Continue CLI.

**How it works.** Codebase indexing combines three approaches: tree-sitter AST parsing for structurally aware chunking, embeddings via transformers.js (default: all-MiniLM-L6-v2, runs locally), and keyword search via ripgrep. Embeddings stored locally in LanceDB (embedded TypeScript vector DB). Re-ranking refines initial retrieval (25 candidates down to 5). Supports MCP as a client for external tool integration. `.continue/rules/` directory enables team-shared AI configuration.

**What Continue does that mache doesn't:**

- Local embedding-based semantic search (LanceDB + transformers.js)
- Re-ranking pipeline for retrieval refinement
- IDE integration (VS Code, JetBrains)
- CI-enforceable AI checks via CLI
- Configurable model selection (local or cloud)
- MCP client integration with external services (GitHub, Sentry, Linear)

**What mache does that Continue doesn't:**

- Real filesystem mount
- Schema-driven projection
- Identity-preserving write-back
- Data-format agnostic
- Cross-reference virtual directories
- Community detection
- MCP server (Continue is a client)
- Deeper AST decomposition (full function/type directory trees vs chunks)

**Gaps worth closing:**

- Local vector search: Continue's LanceDB approach is attractive -- embedded, TypeScript-native, disk-backed, SQL-like filtering. Could be adapted for mache's Go stack.
- Re-ranking: a two-stage retrieve-then-rerank pipeline would improve search quality.

**Sources:** [Continue.dev](https://www.continue.dev/), [GitHub](https://github.com/continuedev/continue), [Codebase indexing docs](https://docs.continue.dev/walkthroughs/codebase-embeddings), [LanceDB integration blog](https://lancedb.com/blog/the-future-of-ai-native-development-is-local-inside-continues-lancedb-powered-evolution/)

______________________________________________________________________

### 6. Aider

**What it is.** An open-source, terminal-based AI pair programming tool that works directly with Git. Known for its "repo map" -- a compact, ranked representation of the codebase that fits in the LLM's context window.

**How it works.** The repo map uses tree-sitter to extract definitions and references from all source files, builds a NetworkX MultiDiGraph where nodes are files and edges are cross-file dependencies, then runs PageRank with personalization to rank symbols by importance. A binary search algorithm packs the highest-value symbols into a configurable token budget (default 1K tokens, user-adjustable via `--map-tokens`). The map shows function signatures, class definitions, and file structure -- not full source code. Aider proposes changes as Git diffs.

**What Aider does that mache doesn't:**

- PageRank-based importance ranking for context selection
- Token-budget-aware context packing (binary search for optimal fill)
- Git-native workflow (changes as tracked diffs/commits)
- 100+ language support (tree-sitter)
- Efficient context utilization: 4.3-6.5% of context window vs 54-70% for iterative search agents

**What mache does that Aider doesn't:**

- Real filesystem mount
- Schema-driven projection
- Identity-preserving write-back (surgical byte-range replacement, not whole-file diffs)
- Data-format agnostic
- Cross-reference virtual directories with navigable content
- Community detection
- MCP server exposure
- On-demand content resolution (lazy loading)

**Gaps worth closing:**

- Importance ranking: Aider's PageRank over the reference graph is a simple, effective heuristic for "what matters most." Mache's refs graph already has the data; adding a PageRank-based ranking to `search` or `get_overview` would improve context selection.
- Token-budget-aware output: mache's MCP tools don't currently optimize for token budgets.

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

- Real filesystem mount
- Schema-driven projection
- Identity-preserving write-back
- Data-format agnostic
- Navigable directory-tree decomposition
- Community detection
- MCP server exposure
- Open source

**Gaps worth closing:**

- Change impact analysis: CodeRabbit's "this change breaks N callers" is a natural extension of mache's callers/callees graph. Exposing a `get_impact` MCP tool that traces from a changed symbol through the refs graph would be valuable.
- ast-grep integration: ast-grep's pattern-matching could complement tree-sitter queries for more expressive code search.

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

- Real filesystem mount
- Schema-driven projection
- Identity-preserving write-back
- Data-format agnostic
- Community detection
- MCP server exposure
- Open source
- Self-hostable without enterprise plan

**Gaps worth closing:**

- Git history integration: understanding how code evolved over time would enrich mache's context. A `git_context` virtual directory or MCP tool could surface recent changes per function.
- Multi-hop investigation: mache's callers/callees graph supports one-hop traversal; multi-hop tracing (A calls B calls C) would match Greptile's depth.

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

- 64 languages vs mache's 8
- `get_architecture` tool (10 architectural aspects in one call)
- Risk-labeled call traces
- Git diff change impact analysis (`detect_changes`)
- Custom Cypher-like query language
- 25 edge types (CALLS, HTTP_CALLS, IMPORTS, READS, WRITES, THROWS, etc.)
- ADR management (`manage_adr`)
- 4 auto-triggering skills based on conversation context
- Background watcher for auto-reindex

**What mache does that codebase-memory-mcp doesn't:**

- Real filesystem mount (NFS/FUSE)
- Schema-driven projection -- user-defined topology, not hardcoded graph schema
- Identity-preserving write-back
- Data-format agnostic (JSON, SQLite, not just code)
- On-demand content resolution (lazy loading, not bulk indexing)
- FCA-based schema inference
- `callees/` forward call graph (not just callers)
- Configurable AST queries per language
- Doc comment extraction
- Context files (imports/globals per scope)

**Gaps worth closing:**

- `get_architecture` equivalent: a first-contact orientation tool that summarizes key architectural aspects
- Change impact analysis: leveraging the refs graph to show what a diff affects
- Background watcher: automatic re-ingest on file changes
- Language breadth: adding more tree-sitter grammars

**Sources:** [GitHub](https://github.com/DeusData/codebase-memory-mcp), [Documentation site](https://deusdata.github.io/codebase-memory-mcp/)

______________________________________________________________________

## Cross-Cutting Themes

### 1. Semantic Search is Table Stakes

Seven of nine competitors offer embedding-based semantic search (Augment, Cody, Cursor, Continue, CodeRabbit, Greptile via code graph, Aider via PageRank). Mache has none. This is the most consistent gap.

**Recommendation:** Add vector search to mache. Options: SQLite-vec (pure Go, stays in the SQLite ecosystem), LanceDB (proven in Continue and CodeRabbit), or a custom embedding pipeline. The `search` MCP tool and `.query/` magic directory are natural integration points.

### 2. Incremental Re-Indexing / Live Updates

Most competitors update their index automatically as files change (Augment: seconds, Cursor: Merkle tree diffing, codebase-memory-mcp: git-based watcher, Greptile: continuous). Mache requires re-mount.

**Recommendation:** Add a file watcher (fsnotify) that triggers incremental re-ingest for changed files. The MemoryStore already supports surgical node updates via write-back; extending this to file-change events is natural.

### 3. Multi-Repo Awareness

Augment, Cody, and Greptile support cross-repository context. Mache operates on single mount targets.

**Recommendation:** Lower priority. Mache can mount multiple sources simultaneously by composing schemas, but explicit multi-repo tooling is a future concern.

### 4. Change Impact Analysis

codebase-memory-mcp (`detect_changes`), CodeRabbit (GraphRAG impact), and Greptile (multi-hop investigation) all analyze how changes propagate through the codebase. Mache's callers/callees graph has the data but doesn't expose an impact analysis tool.

**Recommendation:** Add a `get_impact` MCP tool that takes a file path or symbol and traces through the refs graph to show affected callers/callees, similar to codebase-memory-mcp's `detect_changes`.

### 5. Language Breadth

codebase-memory-mcp supports 64 languages, Serena supports 30+, Aider supports 100+. Mache supports 8.

**Recommendation:** Adding tree-sitter grammars is mechanical work. Prioritize languages by user demand. The sitter_walker architecture makes this straightforward.

### 6. First-Contact Orientation

codebase-memory-mcp's `get_architecture` (10 aspects) and Greptile's full code graph provide immediate orientation for agents encountering a new codebase. Mache's `get_overview` serves this role but is less structured.

**Recommendation:** Enhance `get_overview` or add a dedicated `get_architecture` tool that returns structured architectural aspects (entry points, key abstractions, dependency layers, test coverage, configuration patterns).

______________________________________________________________________

## Positioning Summary

Mache occupies a unique position: it is the **only tool** that combines schema-driven projection, real filesystem mount, AST decomposition, identity-preserving write-back, and data-format agnosticism. No competitor offers even three of these five properties together.

The primary gaps are:

1. **Semantic search** (embeddings/vector DB) -- the most universal capability mache lacks
1. **Incremental re-indexing** -- automatic updates on file changes
1. **Change impact analysis** -- leveraging the existing refs graph
1. **Language breadth** -- 8 vs 30-64 languages
1. **First-contact orientation** -- structured architecture summary tool

The primary strengths competitors cannot easily replicate:

1. **Real filesystem mount** -- requires deep OS integration (NFS/FUSE), not an API wrapper
1. **Schema-driven projection** -- topology is configurable, not hardcoded
1. **Data-format agnosticism** -- no competitor handles JSON + SQLite + source code through one engine
1. **Identity-preserving write-back** -- validate, format, splice, update -- through the filesystem
1. **FCA schema inference** -- automatic topology derivation from data structure
