# Changelog

All notable changes to mache are documented here. This project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); pre-1.0 minor
bumps may include breaking changes.

## [v0.13.0] — 2026-07-04

`mache build` now provisions leyline (LLO) itself, so the full 10-rule
`find_smells` set and accurate cross-references are available anywhere mache
runs — no separate leyline install. This makes the W6.1 `find-smells` composite
action (and any consumer) get LLO for free on a plain CI runner.

### Added

- **`mache build` auto-downloads leyline.** `mache serve` already fetched the
  pinned ley-line-open binary when absent; the build path now does too, via a
  shared `leyline.ResolveBinary` (PATH → `~/.mache/bin/leyline` → download).
  Default `--backend auto` prefers leyline, downloading if needed, and falls
  back to in-process tree-sitter only when leyline is genuinely unavailable
  (offline, or `MACHE_NO_LEYLINE=1`). The binary caches to `~/.mache/bin`.

### Changed

- **`docs:lint` is now a CI gate.** The docs-freshness check (`covers-version`
  vs the latest CHANGELOG, mache language-count claims) runs in CI, not just
  local `task check` — closing the local==CI gap that let v0.12.0 ship with
  stale doc markers. Only version/lang-count mismatches fail; `last-verified`
  age is warn-only.

## [v0.12.0] — 2026-07-03

The "distribution" release (analysis-substrate → W6). v0.11.0 made the
structural-smell **ratchet gate** work on mache's own tree; v0.12.0 makes it
consumable by *any* repo. Two pieces: `find-smells` can now emit **SARIF**, and
a **reusable composite GitHub Action** wraps the whole gate so downstream repos
adopt it with one `uses:` line — no Go toolchain, no build-from-source.

### Added

- **`mache find-smells --format sarif`.** Emits a single SARIF 2.1.0 document
  over all rules (`driver.rules[]` + flat `results[]`, 1-based regions,
  repo-relative URIs) for upload to GitHub code-scanning. Built by rendering a
  values map through mache's existing `internal/template` engine via an
  embedded template — no SARIF library, no hand-built JSON.
- **`.github/actions/find-smells` reusable composite action.** Any repo runs the
  ratchet gate via
  `uses: agentic-research/mache/.github/actions/find-smells@<sha>`: it downloads
  the pinned prebuilt `mache-linux-amd64` release binary (CGO=1, ships
  tree-sitter), fails loud if no baseline is committed, builds the index,
  uploads SARIF **before** gating (so findings reach code-scanning even on a
  failing run), writes a markdown job summary, then enforces the ratchet last.
  Inputs: `mache-version`, `schema`, `baseline`, `fail-on-new`, `upload-sarif`.

### Changed

- **`scripts/actions-pin-lint.sh`** no-argument default now also scans
  `.github/actions/`, so composite-action `uses:` refs are SHA-pin-gated by
  `TestActionsPinLint_RepoIsClean` the same as workflow refs.

### Fixed

- **`server.json`** regenerated to match `version.txt` (was left stale at 0.10.0
  after the v0.11.0 tag), so `task check` / `gen:server-json:check` pass on a
  clean tree.

## [v0.11.0] — 2026-07-03

The "analysis-substrate ratchet" release. The headline is a working local-first
structural-smell **gate**: `mache find-smells --baseline` plus a `task smells`
target ratchet mache's own codebase so *new* structural debt fails CI while
existing debt is grandfathered — local, git hooks, and CI run the identical
command. Also lands the consumer half of the ley-line wire-compat handshake,
executable version-parity enforcement, and a fix that restores `cat context` on
mounted `.db` files. (v0.9.0 and v0.10.0 shipped as tags but were never
back-filled in this changelog; this entry resumes it.)

### Added

- **Count-based smell ratchet — `mache find-smells --baseline` / `--write-baseline`.**
  A ratcheting gate over `find_smells` output: the committed baseline records
  findings-per-`(rule_id, source_id)`, and the gate fails only on findings
  *above* baseline (new debt) while the baseline may shrink freely. Counts, not
  line numbers, are recorded, so the gate is stable across incidental line
  shifts. `--baseline` gates and overrides `--fail-on` (exit 1 iff any
  `(rule, file)` exceeds baseline, printing the new debt); `--write-baseline`
  regenerates the committed file; `--baseline-root` relativizes `source_id` so
  the baseline is machine/CI-portable. A glob/`*` run now skips rules whose
  tables are absent instead of aborting. (#476, #477 — mache-491b9f, mache-4d155c)
- **`task smells` — local-first repo gate on real source.** Builds a tree-sitter
  `.db` of mache's own codebase and runs `find-smells --rule '*' --baseline`, so
  the same command gates locally, in git hooks, and in CI (taskfile-CI-parity).
  `docs/smell-baseline.json` grandfathers current debt. The prior dogfood target
  is now `task smells:dogfood`; `task smells:baseline` regenerates. Wired into
  `check` + `ci` and the CI lint job. (#478 — mache-4da90e)
- **`task actions:lint` — SHA-pin gate for GitHub Actions.** Flags any workflow
  `uses:` ref whose `@`-suffix isn't a 40-hex SHA (and any `docker://` ref not
  pinned to `@sha256:<64-hex>`); local `./` refs are exempt. Makes the "all
  actions SHA-pinned" supply-chain invariant executable. Wired into `check` +
  `ci` and the CI lint job, with Go tests guarding the repo's own workflows.
  (#470 — mache-b8900d)
- **Startup ley-line wire-compat handshake.** If a ley-line daemon is already
  reachable, mache queries its `leyline_version` op and refuses to serve on a
  structural `wire_format_major` mismatch, or when this build's schema-client is
  older than the daemon's `compat_min`. It never auto-starts a daemon to probe,
  and no-ops when none is reachable or the daemon predates the op. Closes the
  consumer half of the handshake guarding the silent parse-returns-0 drift that
  bit v0.4.2→v0.4.3. (#474 — mache-8kif)

### Changed

- **Configurable ley-line daemon start timeout + crash-vs-timeout diagnostics.**
  Auto-started daemons were failing with a hardcoded "socket did not appear
  within 5s" on cold starts. The timeout is now configurable via
  `MACHE_LEYLINE_START_TIMEOUT` (Go duration or bare seconds), default raised
  5s → 15s, and a daemon that *crashes* on startup returns immediately with
  "exited during startup: <status>" instead of waiting out the timeout; the
  timeout error names the contended arena. (#468 — mache-0a1ded)

### Fixed

- **`cat context` now works on a mounted/built `.db`.** The `context` virtual
  file (imports + types visible to a construct's scope) was silently empty for
  every construct on a `.db`-backed mount: `node.Context` is populated at ingest
  and worked in `MemoryStore`, but the SQLite path had no `context` column,
  never persisted it, and never selected it on read. The nodes table now carries
  a `context BLOB`, both `GetNode` read paths select it (guarded by a new
  `graph.ColumnExists` so older / ley-line-produced tables degrade to empty
  rather than erroring; incremental builds `ALTER` it in), and the writer
  round-trips it through the two-pass write. (#467 — mache-b8fe72)

### Internal

- **Executable ley-line schema/binary version-parity gate.** A test fails CI on
  a `major.minor` mismatch between the `go.mod` `leyline-schema` client pin and
  the `leylineBinaryVersion` daemon-binary const (the wire format; patch may
  float) — making executable the invariant a comment + a human previously
  enforced. Complements the runtime handshake above. (#469 — mache-b8af69)
- **Watcher FD-leak test hardened.** `TestWatcher_TargetIgnored` now asserts the
  FD-level invariant directly — `WatchList()` excludes `target/`, `dist/`,
  `build/` — instead of only counting callbacks, so a refactor that keeps the
  callback filter but drops the `SkipDir` skip can no longer pass while
  reintroducing the 129K-FD leak. (#473 — mache-336016)
- **`docs:lint` fixed on main** — required frontmatter added to the dated
  smell-debt-baseline snapshot that had reddened the gate. (#466 — mache-89e322)
- Dependabot / toolchain bumps: `modernc.org/sqlite` 1.52.0→1.53.0,
  `mark3labs/mcp-go` 0.55.0→0.55.1, `ley-line-open/clients/go/leyline-schema`,
  `actions/checkout` 6→7, `actions/setup-go` 6.4.0→6.5.0,
  `golangci/golangci-lint-action` 9.2.1→9.3.0, `softprops/action-gh-release`
  3.0.0→3.0.1. (#457–#462, #472)

### Documentation

- **Daemon-lifecycle docs point at the shipped supervisor.** The
  connection-refused gotcha in GETTING-STARTED now points at the
  `mache init --global` keepalive supervisor (launchd/systemd, shipped v0.10.0)
  instead of describing it as unbuilt. (#466 — mache-823d91)
- **ART analysis-substrate scoping spec + done-state.** Design docs for the
  analysis-substrate decade: the "everything is a projection of one
  capnp-schema'd substrate" thesis, the W0–W6 decomposition, the local-first
  gate done-state, and the successor dependency-reduction decade. (#471, #475)

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
  ship together; mixing v0.7.x mache with v0.2.0 ley-line-open (or vice versa)
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
  as a structural-observation **consumer/projector** of ley-line-open's capnp
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
  gated on `mache-33dc5f` (ley-line-open release-bundling). (#316)
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
  `mache-33dc5f` (release-bundling the ley-line-open `leyline` binary so mache
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

[v0.12.0]: https://github.com/agentic-research/mache/compare/v0.11.0...v0.12.0
[v0.13.0]: https://github.com/agentic-research/mache/compare/v0.12.0...v0.13.0
[v0.8.0]: https://github.com/agentic-research/mache/compare/v0.7.0...v0.8.0
