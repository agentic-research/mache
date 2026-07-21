# Architectural Decision Records

Every ADR carries YAML front-matter with four keys — `title`, `status`, `date`, `tags` —
and its body starts at `## Context`. The front-matter `title` is the document title; there
is no duplicate `#` heading.

## Status vocabulary

| Status          | Meaning                                                                       |
| --------------- | ----------------------------------------------------------------------------- |
| **Proposed**    | Designed and written down, not committed to. May never ship.                  |
| **Accepted**    | Decided and in force; the codebase follows it.                                |
| **Implemented** | Accepted *and* the migration it describes is fully shipped.                   |
| **Superseded**  | No longer in force. The front-matter or a status note names what replaced it. |

## Index

| ADR                                                             | Status      | Title                                                                             |
| --------------------------------------------------------------- | ----------- | --------------------------------------------------------------------------------- |
| [0001](0001-user-space-fuse-bridge.md)                          | Superseded  | User-Space FUSE Bridge (fuse-t)                                                   |
| [0002](0002-declarative-topology-schema.md)                     | Accepted    | Declarative Topology Schema                                                       |
| [0003](0003-cas-layered-overlays.md)                            | Proposed    | Content-Addressed Storage & Layered Overlays                                      |
| [0004](0004-mvcc-memory-ledger.md)                              | Proposed    | Wait-Free MVCC Memory Ledger for POSIX/SQL Graph Projection                       |
| [0005](0005-fca-schema-inference.md)                            | Proposed    | FCA-Based Schema Inference                                                        |
| [0006](0006-pure-go-mcp-first.md)                               | Implemented | Pure Go, MCP-First — Remove CGO and FUSE                                          |
| [0007](0007-git-object-graph-as-fs-projection.md)               | Proposed    | Git Object Graph as Filesystem Projection                                         |
| [0008](0008-greedy-entropy-schema-inference.md)                 | Implemented | Greedy Entropy-Based Schema Inference                                             |
| [0009](0009-ast-aware-write-pipeline.md)                        | Accepted    | AST-Aware Write Pipeline                                                          |
| [0010](0010-hosted-mache-architecture.md)                       | Proposed    | Hosted Mache Architecture                                                         |
| [0011](0011-pointer-abstraction.md)                             | Proposed    | Pointer Abstraction — Filesystems, Graphs, CAS, Git, Diffs Are All the Same Thing |
| [0012](0012-cgo-removal-migration.md)                           | Implemented | CGO Removal — Migration Plan                                                      |
| [0013](0013-refs-defs-canonical-schema.md)                      | Proposed    | Refs/Defs Canonical Schema (Fidelity Poset Over Producers)                        |
| [0014](0014-mache-in-constellation.md)                          | Proposed    | Mache as observation producer in the constellation                                |
| [0015](0015-syntax-aware-write-protection.md)                   | Accepted    | Syntax-Aware Write Protection                                                     |
| [0016](0016-cross-language-reference-resolver.md)               | Proposed    | Cross-Language Reference Resolver                                                 |
| [0017](0017-test-harness.md)                                    | Proposed    | Matrix-Coverage Test Harness — Invariants for "What 100% Means"                   |
| [0018](0018-doc-drift-as-executable-specs.md)                   | Accepted    | Doc-Drift Detection as Executable Specs (find_smells First-Class Workflow)        |
| [0019](0019-real-corpus-fixture-registry.md)                    | Proposed    | Real-Corpus Fixture Registry as First-Class Test Infrastructure                   |
| [0020](0020-portable-cache-lockfile-schema.md)                  | Proposed    | Mache adopts LLO's CacheLockfile schema for portable-cache (consumer-side)        |
| [0021](0021-file-suggester.md)                                  | Proposed    | Semantic file-suggester for Claude Code's `fileSuggestion`                        |
| [0022](0022-mcp-transport-canonical.md)                         | Accepted    | Streamable HTTP is the canonical MCP transport; stdio is an escape hatch          |
| [0023](0023-unified-code-fact-ir.md)                            | Superseded  | Unified Code-Fact IR — Property Graph over a Content-Addressed Symbol Set         |
| [0024](0024-incremental-dataflow-taint-as-substrate-queries.md) | Proposed    | Incremental Dataflow & Taint as Substrate Queries                                 |

## Notes on the non-obvious statuses

- **0001 (Superseded)** — FUSE was removed in v0.7.0. NFS is the only mount backend; the
  pure-Go / MCP-first direction is set by [0006](0006-pure-go-mcp-first.md). For FUSE today,
  use ley-line-open's `leyline serve`.
- **0006 / 0012 (Implemented)** — the CGO+FUSE removal is fully shipped as of v0.18.0
  (`mache-37ae8b`). Release builds are `CGO_ENABLED=0` and every source path routes through
  `leyline parse` → `_ast` → pure-Go `ASTWalker`.
- **0023 (Superseded)** — the `symbol_id` addressing was replaced by the merkle-AST content
  address (`node_hash`) in ley-line-open's producer rework; the property-graph thesis holds.
- **0015** was renumbered from ADR-0006 to resolve a numbering collision with
  `0006-pure-go-mcp-first.md`.

This index is hand-maintained; generating it from the front-matter is tracked separately.
