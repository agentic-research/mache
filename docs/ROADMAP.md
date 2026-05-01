# Roadmap

## Current state (as of April 2026, post-v0.7.0)

**Stable today:**

- 28-language tree-sitter parsing via the standalone CGO path; pure-Go path via [ley-line-open](https://github.com/agentic-research/ley-line-open) `.db` files (see [ARCHITECTURE.md § Interplay with ley-line-open](ARCHITECTURE.md#interplay-with-ley-line-open))
- Two graph backends: `MemoryStore` (in-memory map for source ingestion) and `SQLiteGraph` (zero-copy SQL over LLO `.db`)
- NFS-only mount via `go-nfs` + `billy` (FUSE was removed in v0.7.0 per ADR-0006; for FUSE today, use `leyline serve` from LLO)
- MCP server with 16 tools: 13 work standalone, 3 require LLO enrichment (`semantic_search`, `get_type_info`, `get_diagnostics`); `find_smells` partially degrades on tree-sitter-only mounts (rules requiring `_ast` need an LLO-built `.db`)
- Write-back pipeline: validate (tree-sitter) → format (gofumpt for Go, hclwrite for HCL/Terraform) → splice → surgical node update + `ShiftOrigins` (no re-ingest)
- Draft mode: invalid writes save as drafts, node path stays stable, errors surface via `_diagnostics/`
- Context awareness: virtual `context` files expose imports/globals to agents
- Cross-reference indexing: `node_refs`/`node_defs` SQLite tables backed by tree-sitter (standalone) or LLO (`.db` path)
- `callers/` and `callees/` virtual directories — self-gating, NFS-served as `graphFile`s
- `find_smells` MCP tool: 9 structural rules (`magic_int_in_comparison`, `dead_code`, `cyclomatic_complexity`, `long_function`, `untested_function`, `duplicate_definitions`, `god_file`, `fan_out_skew`, `long_file`) with optional `min_metric` and `source_id` filters; advisory PR comments via `.github/workflows/find-smells.yml`
- FCA + greedy entropy schema inference: `--infer` auto-generates topology from data
- Virtual `_schema.json` at mount root exposing the active topology
- HotSwap graph with control block for live schema reload

**Known limitations:**

- Memory: ~2GB peak for 323K NVD records (1.6M graph nodes with string IDs) — addressable via [GenerationalGraph](https://github.com/agentic-research/mache) (mache-2f1287)
- Write-back formatting is Go and HCL/Terraform only; other languages validate but don't auto-format
- Standalone (CGO) path produces `node_defs`/`node_refs` but not `_ast` — 4 of 9 `find_smells` rules (`magic_int_in_comparison`, `cyclomatic_complexity`, `long_function`, `long_file`) require an LLO-built `.db`
- bbolt-backed `ext/boltdb` projection is opt-in (not in default `go.work`); used by venturi/trivy-db workflows

## Near-term

- **Eliminate CGO tree-sitter** (mache-37ae8b, epic mache-36d961) — once LLO covers all standalone-path use cases, delete `SitterWalker` + tree-sitter build tags. Ship a pure-Go `mache` binary by default.
- **Bundle leyline binary in mache release** (mache-33dc5f) — auto-detect tiers so the LLO-paired path works without a separate install
- **`detect_changes` MCP tool** (mache-bsq) — git diff → affected AST nodes → blast radius via callers/callees BFS
- **`dep_cycles` smell rule** (mache-unm5) — Tarjan SCC over node_refs; motivates extending `SmellRule` beyond pure SQL (Go-callback rules)
- **Construct creation via NFS** — agents can write directly to source files today (`echo >> file.go`) and mache re-ingests on next writable file close. A first-class `Mkdir`/`Create` handler that generates stubs from templates is the next ergonomic step.

## Medium-term

- **Additional formatters** — Python (black/ruff), TypeScript (prettier). Validation works for all tree-sitter languages; formatting needs per-language wiring in `internal/writeback/format.go`
- **`_diagnostics/doc-drift`** (mache-8zq) — cross-reference docstrings vs signatures, READMEs vs symbols, ADRs vs constants
- **Public `pkg/` API** (mache-734971) — expose ingest engine + materializers as a Go library, not just a CLI
- **Self-referential schema pipeline** (mache-1cee2f) — read mache's own `nodes` table as a data source, enabling meta-projections
- **`trace/` virtual dir** (mache-ok2) — transitive call-path traversal as a navigable directory

## Long-term

- **Content-addressed storage** (ADR-0003) — store data by hash, hard links for dedup
- **Layered overlays** (ADR-0003) — Docker-style composable layers for versioned views
- **MVCC memory ledger** (ADR-0004) — wait-free RCU + ECS for 10M+ entities, gated on profiling
- **R2 storage backend** (mache-2f0075, mache-d45da8, mache-ae2432) — persist mache graph as SQLite on Cloudflare R2 for BYO-storage tier
- **D1 backend** (mache-d44509) — project mache graph to Cloudflare D1 for edge-cached queries
- **Cross-language reference graph** (mache-q43l, epic) — typed locators that span languages (e.g., a Python call referencing a Rust FFI symbol)

## Tracking

All work items live as beads in `.beads/` (Dolt-backed). Use `bd list --json` or the `rsry` MCP server to browse open work. Earlier `INVESTIGATION_LOG.md` and `_agent_log/` files are historical journals retained for context but no longer the active source of truth.
