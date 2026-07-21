---
status: current
covers-version: v0.17.0
last-verified: 2026-07-14
sources-of-truth:
  - CHANGELOG.md
  - .beads/
audience: [contributors, maintainers, users]
supersedes: []
---

# Roadmap

## Current state (as of 2026-07, through v0.17.0)

v0.8.0 — the "constellation wave" — ships paired with **ley-line-open v0.2.0**. The wire format between them is now content-addressable: substrate identity is the BLAKE3 `current_root` of the arena payload, not a monotonic generation counter. Old mache reading new arenas (or vice versa) fails loudly with a clear version-mismatch error rather than corrupting reads. See [CHANGELOG.md § v0.8.0](../CHANGELOG.md) for the full break, [ADR-0014](adr/0014-mache-in-constellation.md) for the architectural framing.

**Stable today:**

- 27-of-28-language parsing via `leyline parse` (pure Go — in-process CGO tree-sitter removed in ADR-0012 step 4; only cue lacks a grammar). See [ARCHITECTURE.md § Interplay with ley-line-open](ARCHITECTURE.md#interplay-with-ley-line-open)
- Two graph backends: `MemoryStore` (in-memory map for source ingestion) and `SQLiteGraph` (zero-copy SQL over ley-line-open `.db`)
- NFS-only mount via `go-nfs` + `billy` (FUSE was removed in v0.7.0 per ADR-0006; for FUSE today, use `leyline serve` from ley-line-open)
- MCP server with **18 tools**: 14 work standalone, 3 require ley-line-open enrichment (`semantic_search`, `get_type_info`, `get_diagnostics`), and `get_sheaf_status` reports the ley-line daemon's cache state (returns `{available: false}` when the daemon is unreachable, so it is safe to call anywhere); `find_smells` partially degrades on tree-sitter-only mounts (rules requiring `_ast` need a `.db` built by ley-line-open). The full list: `get_overview`, `find_callers`, `find_callees`, `find_definition`, `search`, `list_directory`, `read_file`, `semantic_search`, `write_file`, `get_type_info`, `get_diagnostics`, `get_sheaf_status`, `get_impact`, `get_communities`, `get_diagram`, `get_architecture`, `find_smells`, `resolve_ref`.
- Write-back pipeline: validate (leyline validate op, pure Go) → format (gofumpt for Go, hclwrite for HCL/Terraform) → splice → surgical node update + `ShiftOrigins` (no re-ingest)
- Draft mode: invalid writes save as drafts, node path stays stable, errors surface via `_diagnostics/`
- Context awareness: virtual `context` files expose imports/globals to agents
- Cross-reference indexing: `node_refs`/`node_defs` SQLite tables produced by `leyline parse`
- **Canonical views** (ADR-0013): `v_defs` / `v_refs` with `(mention ⊑ binding ⊑ reachability)` fidelity ordering — consumer rules query producer-agnostically
- **Capnp event-log readthrough**: `${db}.bindings.capnp` is the cross-runtime contract for binding refs; `find_smells` reads the canonical event log directly
- `callers/` and `callees/` virtual directories — self-gating, NFS-served as `graphFile`s
- `find_smells` MCP tool: 14 rules — 10 code-structure (`dead_code`, `god_file`, `long_function`, `cyclomatic_complexity`, `duplicate_code`, `duplicate_definitions`, `fan_out_skew`, `long_file`, `untested_function`, `magic_int_in_comparison`), 3 doc-drift (`drift_doc_broken_internal_link`, `drift_doc_dead_symbol_reference`, `drift_doc_outdated_count`), 1 test (`sleep_in_test`) — with optional `min_metric` and `source_id` filters; `fan_out_skew` is qualifier-aware via T8.7. Enforced as a PR gate via `.github/workflows/find-smells.yml` (runs `task smells`, fails on any new finding vs the committed `docs/smell-baseline.json` — since v0.16.0, replacing the earlier advisory comment-only workflow)
- FCA + greedy entropy schema inference: `--infer` auto-generates topology from data
- Virtual `_schema.json` at mount root exposing the active topology
- **Hot-swap polling on `current_root`** — the writer (ley-line-open or mache itself) publishes a BLAKE3 root with each new arena; readers detect swaps via root inequality. Control-block + arena VERSION 2.
- **e2e MCP-tool harness** with per-tool latency + alloc profile, CPU + heap pprof capture, and `task profile-tools-pprof` / `task flamegraphs`
- **Snapshot memoization**: `MemoryStore.{Defs,Refs}Map` cached, invalidated on `AddDef` / `AddRef` / `DeleteFileNodes`
- **Single leyline smell gate** (v0.17.0): `task smells` builds the tracked tree via the exact-pinned leyline and runs all `gate`-tagged rules against one `docs/smell-baseline.json`; the pure-Go tree-sitter gate is retired. The leyline binary pin is exact `major.minor.patch` — LLO patch releases can change the emitted `_ast` schema
- **Pure Go — in-process CGO tree-sitter fully removed** (v0.18.0, ADR-0012 step 4, mache-37ae8b): every source path (build / serve / mount / infer / testfixtures) routes through `leyline parse` → `_ast` → pure-Go `ASTWalker`; `SitterWalker`, `sitter_flatten`, the 28 grammar bindings, and the `go-tree-sitter` dep are gone, and the release builds `CGO_ENABLED=0`. leyline v0.8.0 parses 27 of 28 registry languages (all but cue, which has no tree-sitter grammar anywhere); a schema coverage guard reports cue loudly. Write-back validation + the nil-slice linter run over the leyline validate op's `emit_ast` payload (v0.7.8). The `leyline_fs` FFI stays as a separate `//go:build leyline` dev-only surface.

**Known limitations:**

- Memory: ~2GB peak for 323K NVD records (1.6M graph nodes with string IDs) — addressable via [GenerationalGraph](https://github.com/agentic-research/mache) (mache-2f1287)
- Write-back formatting is Go and HCL/Terraform only; other languages validate but don't auto-format
- cue is the one registry language `leyline parse` cannot handle (no tree-sitter-0.26 cue grammar exists anywhere); a schema targeting cue projects nothing and the coverage guard errors loudly
- bbolt-backed `ext/boltdb` projection is opt-in (not in default `go.work`); used by venturi/trivy-db workflows

## Near-term

- **Bundle leyline binary in mache release** (mache-33dc5f) — gates the CGO-removal cutover; ships a known-compatible `leyline` in the mache tarball so the ley-line-open-paired path works without a separate install. With this, "what version of ley-line-open does mache v0.x.0 want" has a literal answer (whatever's in the tarball).
- **Startup `leyline --version` check** (mache-8kif) — refuse to start if the on-PATH leyline is older than the minimum baked into this mache release; complements the wire-format VERSION rejection by failing earlier and clearer.
- **Consent-gated auto-download** (mache-9051f0) — for `go install` / source builds where there's no bundle, fall back to fetching the version-pinned ley-line-open release. Default off; opt in via `--auto-install` flag or interactive prompt. Never silently fetch+exec a remote binary.
- ~~**Eliminate CGO tree-sitter**~~ — SHIPPED (mache-37ae8b, v0.18.0; see "Stable today"). leyline is the universal parser; the release binary is `CGO_ENABLED=0`.
- **Schema-driven read modes** (mache-qzsk) — `read_file` accepts a `mode` arg (`signatures`, `map`, `diff`) for compressed projections instead of full content. The v0.8.0 `current_root` snapshot identity gives `mode=diff` a usable anchor.
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
