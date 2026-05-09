# Changelog

All notable changes to mache are documented here. This project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); pre-1.0 minor
bumps may include breaking changes.

## [v0.8.0] — 2026-05-09

The "constellation wave" release. T2.4 wire-format alignment with
ley-line-open v0.2.0, the full ADR-0013 fidelity-poset canonical-view
landing, ADR-0014 architectural framing, and a substantial e2e harness +
perf hygiene pass.

### ⚠ Breaking changes

- **Substrate identity is `current_root`, not `generation`.** Both
  `.ctrl` (control block) and `.arena` headers bump VERSION 1 → 2.
  Pre-v2 files are rejected with a clear error rather than silently
  misinterpreted. Pairs with **ley-line-open v0.2.0** — these must
  ship together; mixing v0.7.x mache with v0.2.0 LLO (or vice versa)
  fails loudly on first read. (#365, mache-1afff0)

  Public Go API changes:

  - `Controller.GetGeneration() uint64` → `GetCurrentRoot() [32]byte`
  - `Controller.SetArena(path, size, generation)` →
    `SetArena(path, size)` + new `SetArenaWithRoot(path, size, root)`
  - `ArenaHeader` adds `DataSize uint64`; readers can hash
    `buf[..DataSize]` against `current_root` without parsing
    payload-format internals.

- **Hot-swap polling predicate is now `currentRoot != lastRoot`**, not
  monotonic-generation comparison. Mache writers now compute
  `blake3.Sum256(payload)` and publish via `SetArenaWithRoot`.

### Added

- **ADR-0013 fidelity poset for refs/defs.** Canonical views
  `v_defs` / `v_refs` with `(mention ⊑ binding ⊑ reachability)` ordering;
  consumer rules query producer-agnostically. Higher-fidelity producers
  (LSP, future SSA) project DOWN at write time.
  (#339, #340, #341, #346, #347, #348)
- **Capnp event-log readthrough.** `${db}.bindings.capnp` is now the
  cross-runtime contract for binding refs; `find_smells` consumes the
  capnp pass directly. SQL `_lsp_refs` consumers retired in favor of
  the canonical event log. (#353, #354, #356, #357, #358)
- **`find_smells` qualifier-aware `fan_out_skew`** via T8.7
  `BindingRecord.qualifier`. (#358, mache-6c0d07)
- **e2e MCP-tool harness** with per-tool latency + alloc profile,
  CPU + heap pprof capture per tool, and `task profile-tools-pprof` /
  `task flamegraphs`. (#360, #361, mache-6b6da6 phases 1–2)
- **`MemoryStore.{Defs,Refs}Map` snapshot memoization.** Heap
  e2e profiling identified these two as dominant allocators in
  `get_impact` (52% heap delta), `get_overview` (32%), and others.
  Memoized snapshots invalidated on `AddDef` / `AddRef` /
  `DeleteFileNodes`. (#362, mache-6b6da6 phase 3)
- **ADR-0014** documenting mache's role in the wider ART constellation
  as a structural-observation **consumer/projector** of LLO's capnp
  event-log canonical encoding (Σ-anchored). The `.db` projection is
  derivative; the event log is the contract. (#364)
- **Falsifiability harnesses.** Skip-list ablation experiment +
  π\_{1→0} projection round-trip. (#349, #350, #351, #352,
  mache-3509aa, mache-354464)

### Changed

- **`cmd build` defaults to `--backend=auto`** which now prefers
  ley-line when present on PATH (per ADR-0012 step 3b). The in-process
  CGO tree-sitter path is still available via `--backend=tree-sitter`
  but is on the deprecation runway. (#317, #318)
- **Drop suffix-then-LIKE fallback in `serve_lsp`** now that
  `_lsp_refs.referrer_node_id` and `_lsp_refs.ref_token` are populated
  by ley-line-open. (#348, mache-346d2b)
- **`find_smells` `dead_code` skip-list precise retreat** after
  Falsifiability A confirmed gopls-indexed methods aren't newly
  flagged. (#351)
- New deps: `github.com/zeebo/blake3` (pure Go, no CGO) for substrate
  root publishing on the writer side.

### Documentation

- **ADR-0013** — refs/defs canonical schema (fidelity poset). (#339)
- **ADR-0014** — mache in the constellation. (#364)
- **ADR-0012 status update** — steps 1–3 shipped, step 4 (CGO removal)
  gated on `mache-33dc5f` (LLO release-bundling). (#316)
- **README + ARCHITECTURE** capture T8 canonical views + capnp event
  log + e2e harness + cache memoization. (#359, #363)

### Internal

- Dead-code removal pass: `ingest.GetLanguage`, `Inferrer.InferFromSQLiteJSON`,
  `ArenaFlusher.LastError`, `SocketClient.Enrich`, `DetectLanguageFromExt`,
  `formatRegistry`, `.query/` virtual dir. (#319, #320, #321, #322,
  #323, #324)
- Test contract pins: `ArenaHeader` serialization, `SchemaUsesTreeSitter`
  dispatch, validate-package public API, `LoadFileIndex` cache,
  `NewDefaultResolver` chain shape, `GetGitHints`, `BuildContext`.
  (#330, #331, #332, #333, #334, #335, #336, #337)
- CI scheduler-tolerance fixes for arena flusher coalesce + watcher
  subdirectory tests. (#325, #329)
- Dependabot bumps: `mark3labs/mcp-go` 0.49→0.52, `modernc.org/sqlite`
  1.49.1→1.50, `fsnotify` 1.9→1.10.1, `mvdan.cc/gofumpt` 0.9.2→0.10,
  `go-git/go-billy/v5` 5.8→5.9. (#326, #327, #328, #342, #343, #344, #345)

### Not in this release (queued for next)

- **CGO tree-sitter removal** (mache-36d961, ADR-0012 step 4). Gated on
  `mache-33dc5f` (release-bundling the LLO `leyline` binary so mache
  can hard-fail when leyline isn't available, instead of falling back
  to in-process CGO). The migration inventory is filed on
  `mache-37ae8b` and is shovel-ready: 16 production sources to delete,
  10 test files to delete-or-migrate, single `//go:build leyline` tag
  to invert, BLAKE3 dep already landed.
- **Schema-driven read modes** for `read_file` (`mode=signatures`,
  `mode=map`, `mode=diff`) — bead `mache-qzsk`. The current_root
  primitive shipped here gives `mode=diff` a usable snapshot identity
  to anchor on.

### Coordination

This release is paired with **ley-line-open v0.2.0**. Old mache
reading new arenas — or vice versa — fails with a clear version-mismatch
error rather than corrupting reads. Tag together; release-note the
break in both repos.

[v0.8.0]: https://github.com/agentic-research/mache/compare/v0.7.0...v0.8.0
