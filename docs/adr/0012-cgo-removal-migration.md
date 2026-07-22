---
title: 'ADR-0012: CGO Removal — Migration Plan'
status: Implemented
date: 2026-04-30
tags: [architecture, cgo, pure-go, tree-sitter, leyline, implemented]
---

> **Status note (Implemented — FULLY SHIPPED):** Steps 1–3 + the step-4 commitment point
> completed in `mache-37ae8b` (v0.18.0): in-process CGO tree-sitter removed (SitterWalker,
> sitter_flatten, the 28 grammar bindings, the go-tree-sitter dep), leyline is now the
> universal parser, and the release builds `CGO_ENABLED=0`. Every source path —
> build / serve / mount / infer / testfixtures — routes through `leyline parse` → `_ast` →
> `ASTWalker` (pure Go). The only registry language without a grammar is cue (none exists at
> tree-sitter 0.26), which the coverage guard reports loudly. The `leyline_fs` FFI
> (`internal/leyline/client.go`, `//go:build leyline`) was a separate dev-only surface,
> unaffected then and **since removed** (completed ADR-0006 Thread 4 — see the Unreleased
> CHANGELOG entry; it hardcoded a stale path into the private ley-line repo and was
> compiled by nothing). Supersedes inline mitigation in `mache-2y9w` (PR #257, #299).

## Context

Mache today links tree-sitter as a CGO library through
`github.com/smacker/go-tree-sitter`. The CGO surface has been a
recurring source of pain:

- **`mache-2y9w`**: Sporadic `SIGSEGV: segmentation violation` in
  `internal/ingest` tests on `ubuntu-latest`. Two `runtime.LockOSThread`
  fixes (PR #257 parallel path, PR #299 single-file path) have brought
  flake rates down significantly, but with `-race` enabled the residual
  rate stays at ~20% locally on macOS arm64. The fault consistently
  lands inside `smacker/go-tree-sitter/bindings.go:1026` during cgo
  execution. PR #300 documents that LockOSThread mitigations are
  exhausted as a class — further reductions require either upstream
  binding changes or removing the CGO entirely.
- **`task test` runs with workarounds**: `-race -gcflags=all=-d=checkptr=0`.
  The `checkptr=0` exists specifically to silence false positives from
  the CGO bridge. With CGO gone, both can drop.
- **Smell-rule asymmetry**: 5 of 9 `find_smells` rules work on
  standalone-mache builds (use `node_defs`/`node_refs`/`nodes` only).
  The other 4 (`magic_int_in_comparison`, `cyclomatic_complexity`,
  `long_function`, `long_file`) require an `_ast` table only ley-line
  produces. Standalone consumers see "9 rules" in discovery but only
  5 actually run. This is a documented capability split (`docs/ARCHITECTURE.md`
  "Interplay with ley-line-open"), not a bug — but the inconsistency
  goes away if mache always uses an LLO-built `.db`.
- **Ley-line ownership of parsing is already the architectural intent**.
  `docs/ARCHITECTURE.md` says: "When ley-line pre-parses source into
  `.db` files with `_ast` tables, ASTWalker replaces SitterWalker and
  the entire pipeline is pure Go." This ADR commits to that path.

## Decision

**Remove tree-sitter CGO from mache. Delegate parsing to ley-line entirely.**

After this migration:

- `mache build <source-dir>` invokes `leyline parse` as a subprocess.
  Output `.db` has `_ast`, `_source`, `node_defs`, `node_refs`, `nodes`.
- `mache serve <db>` and `mache <mountpoint>` use `ASTWalker` (pure-Go,
  reads `_ast` via SQL) for everything that today uses `SitterWalker`.
- `SitterWalker`, the smacker/go-tree-sitter dependency, all per-language
  grammar bindings, and `internal/treesitter/elixir/` get deleted.
- `task test` drops `-race -gcflags=all=-d=checkptr=0` workarounds.
- All 9 `find_smells` rules work on every backend (every backend has
  `_ast` now, since ley-line is the only parser).

Operational answer to "where does leyline come from" is `mache-33dc5f`
(bundle leyline binary in mache release; auto-detect tiers).

## Audit (current CGO surface)

### Production code that imports `smacker/go-tree-sitter`

| File                                    | LOC  | Role                                                                                                                                                                                                                  |
| --------------------------------------- | ---- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/ingest/engine.go`             | 1890 | Ingestion dispatch loop (parallel + sequential paths). CGO use: `parser := sitter.NewParser()`, `tree := parser.ParseCtx(...)`, then walks via `e.sitterWalker`. Both paths now have `LockOSThread` (PR #257 + #299). |
| `cmd/mount.go`                          | 1086 | NFS mount + write-back. Uses `e.sitterWalker` for content extraction during writes.                                                                                                                                   |
| `internal/ingest/sitter_walker.go`      | 689  | The walker itself. Compiled tree-sitter queries cached per-language. Delete entirely after migration.                                                                                                                 |
| `internal/lattice/infer.go`             | 286  | FCA schema inference walks tree-sitter AST. Replace with ASTWalker queries against `_ast` table.                                                                                                                      |
| `cmd/infer.go`                          | 238  | CLI for FCA inference. Same change as infer.go.                                                                                                                                                                       |
| `internal/lang/lang.go`                 | 187  | Language registry — ext → grammar mapping. Replaces with name-only lookup once grammars are gone.                                                                                                                     |
| `cmd/build.go`                          | 161  | `mache build` command. The pivot point: rewrites to invoke `leyline parse`.                                                                                                                                           |
| `internal/ingest/address_refs.go`       | 151  | Reference extraction via tree-sitter queries. Delete with sitter_walker.                                                                                                                                              |
| `internal/writeback/validate.go`        | 139  | Write-back syntax validation via tree-sitter parse. Replace with `leyline validate` subprocess OR a different validator.                                                                                              |
| `internal/linter/linter.go`             | 94   | Go linter using tree-sitter. Could move to using `go vet` or stay if it only fires for Go.                                                                                                                            |
| `internal/ingest/sitter_flatten.go`     | 67   | Flatten helper — delete with sitter_walker.                                                                                                                                                                           |
| `internal/ingest/parent_match.go`       | 59   | Parent-walking helper — delete with sitter_walker.                                                                                                                                                                    |
| `internal/ingest/language.go`           | 18   | Language type alias — delete.                                                                                                                                                                                         |
| `internal/treesitter/elixir/binding.go` | 16   | Elixir grammar binding — delete.                                                                                                                                                                                      |

**Total CGO-touching LOC: ~5081 in production paths.**

The two largest files (`engine.go`, `mount.go`) only use a small slice
of CGO; most of their LOC is non-CGO logic. Net deletion is closer to
`sitter_walker.go` + `address_refs.go` + `sitter_flatten.go` +
`parent_match.go` + `language.go` + `treesitter/elixir/` + parts of
`engine.go`/`mount.go`/`infer.go`/`lattice/infer.go`/`writeback/validate.go`/`linter/linter.go`
— call it ~1200–1500 LOC of actual CGO+parser code.

### Tests with CGO imports

`cmd/config_test.go`, `internal/ingest/address_refs_test.go`,
`internal/ingest/sitter_flatten_test.go`,
`internal/ingest/sitter_walker_test.go`,
`internal/lattice/grammar_introspect_test.go`,
`tests/integration_test.go` — most delete with their production
counterparts; `tests/integration_test.go` rewrites to use `leyline parse` subprocess.

### Already-pure-Go alternative

`internal/ingest/ast_walker.go` exists and is the SQL-backed
replacement. It currently lives alongside SitterWalker, selected via
`SelectWalker` based on `_ast` table presence. The migration is mostly
"make ASTWalker the only walker."

## Migration steps (loop-paced PRs)

The plan is reversible until the commitment point (step 4). Each step
ships independently and the test suite stays green throughout.

### Step 1 — This ADR (you are here) ✅ shipped (#310)

Documents the plan. Doc-only PR. Sets the contract.

### Step 2 — Switch query handlers to ASTWalker primary ✅ shipped (#311–#313)

For `find_callers`, `find_callees`, `find_definition`: use ASTWalker
when `_ast` is present in the active backend. SitterWalker stays as
fallback for source-code mounts that haven't been re-built.

Landed as three PRs:

- **#311** — `newASTCallExtractor`: pure-Go alternative to the CGO
  call extractor. Reads `_ast` and `_imports` directly via SQL.
- **#312** — wire AST call extractor for `SQLiteGraph` backends with
  `_ast`. Adds `SQLiteGraph.DB()` accessor. CGO path still used for
  `MemoryStore`-backed mounts.
- **#313** — `CompositeGraph.SetCallExtractorPicker`: per-mount
  dispatch so cross-mount queries pick the right extractor (AST vs
  CGO) based on the local mount's backend.

### Step 3 — `mache build` invokes `leyline parse` ✅ shipped (#314, #315)

Two PRs:

- **#314** (step 3a) — `--backend=leyline` opt-in. Shells out to
  `leyline parse` and copies the result; `--backend=leyline` errors
  if leyline is missing (no silent fallback). Default unchanged.
- **#315** (step 3b) — `--backend=auto` prefers leyline. New
  `leylineAvailable()` probe (PATH + `~/.mache/bin/leyline`). When
  leyline is detected, `mache build` uses it; users without leyline
  see today's behavior unchanged. `--backend=tree-sitter` is the
  explicit escape hatch for in-process parsing.

### Step 4 — Make leyline the required path (commitment point) ⏳ gated on activation + `mache-33dc5f`

**Engine-side progress (2026-06-25, `mache-33cd7e` + `mache-33de70`):** the
work that makes the commitment point *possible* has landed and is
byte-parity-proven, decomposed as S-slices off epic `mache-36d961`:

- **Interface widening (S3, #431/#432/#433):** the engine no longer
  type-switches on the concrete walker. The extractors it needed — lang/pkg
  (`FileMeta`), location/doc-comments (`DocScope`), per-scope calls/refs
  (`CallExtractor`), and file-level refs (`ExtractFileLevelRefs`, #443) — are
  all on the `Match`/`Walker` interfaces, implemented by both backends.
- **Phase-1 parse gating (S4, #445):** the tree-sitter parse + sitter
  context/imports/file-level-refs now run only when `e.astWalker == nil`.
  With an ASTWalker backend, **no CGO runs in ingest** (this also removes the
  `mache-2y9w` SIGSEGV surface on that path). `TestASTQueryParity` ingests
  the same source through both backends — with the CGO parse SKIPPED on the
  AST side — and asserts byte-for-byte identical projection.

**What's left for the commitment point is no longer the engine — it's
*activation* and *deletion*:**

1. **Activation — where `SetASTWalker` gets wired.** Today `SetASTWalker` has
   no production caller; the default `mache <dir>` ingest still parses with
   tree-sitter because **mache is its own parser at ingest time — there is no
   `_ast` db to read from** (the `_ast` index is ingestion's *output*, not an
   input). Activation means inserting a *parse-first* step in the read-only
   source-ingest fast path (`cmd/mount.go`, the `SchemaUsesTreeSitter` branch
   ~L366): run `leyline parse <src> -o <index>.db` to produce
   `_ast`/`_source`/`_imports`, then `engine.SetASTWalker(NewASTWalker(db))`
   before `engine.Ingest(<src>)`. `SelectWalker(db)` is the helper that picks
   ASTWalker when `_ast` is present. Gate on leyline availability: present →
   pure-Go AST ingest; absent → today's tree-sitter path. This is the pivot
   the ADR already names — "ley-line produces, mache projects" — made real
   for source ingestion, and it makes leyline a runtime dependency of
   `mache <dir>` (the same Negative already noted for `mache build`).
1. **Deletion blockers** (unchanged): `validate.go`, `lattice/infer.go`,
   `linter.go` each still need a non-CGO answer before SitterWalker and the
   grammars can be deleted.

Drop SitterWalker fallback. Delete:

- `internal/ingest/sitter_walker.go`, `sitter_flatten.go`,
  `parent_match.go`, `language.go`
- `internal/ingest/address_refs.go`
- `internal/treesitter/elixir/`
- `internal/writeback/validate.go` CGO parts
- `internal/lattice/infer.go` and `cmd/infer.go` CGO parts
- `internal/linter/linter.go` (or move to `go vet` proxy)
- The `smacker/go-tree-sitter` dependency from `go.mod`

One large PR (mostly deletions) plus the corresponding test deletions.

### Step 5 — CI cleanup ⏳ pending step 4

Drop `-race -gcflags=all=-d=checkptr=0` workarounds from `Taskfile.yml`
and `.github/workflows/ci.yml`. Drop the inline `mache-2y9w` notes
from `internal/ingest/reingest_test.go` (added in PR #300). Tiny PR.

## Consequences

### Positive

- **`mache-2y9w` retired** — no CGO, no race-detector interaction, no
  rerun tax. CI flake budget drops. PR #284, #292, #294, #297 all
  needed reruns this session because of `mache-2y9w`; that's gone.
- **Smell-rule asymmetry collapses** — every backend has `_ast`, so all
  9 rules work everywhere. The "five vs nine" split in
  `docs/ARCHITECTURE.md` and `find-smells.yml` simplifies.
- **Pure-Go binaries are deployable everywhere** — no per-platform CGO
  cross-compilation pain. Release matrix simplifies.
- **Smell-rule extraction (option 2 in our roadmap)** becomes
  truly mechanical — no CGO entanglement to inherit.
- **Clean architectural separation** matches the framing already in
  `docs/ARCHITECTURE.md`: ley-line produces, mache projects.

### Negative

- **`mache build` requires `leyline` on PATH** (or bundled per
  `mache-33dc5f`). Existing standalone-mache users without leyline
  regress until they install ley-line-open or a leyline-bundled
  release. Mitigation: ship leyline in mache releases; warn-with-link
  when leyline is missing.
- **The `--writable` write-back pipeline currently uses tree-sitter
  for validation** (`internal/writeback/validate.go`). Need either:
  (a) a `leyline validate` subprocess, or (b) accept lower
  validation rigor (e.g., re-parse on next build). Decide in step 4.
- **`internal/lattice` FCA inference uses tree-sitter** to walk source
  during inference. Replace with SQL queries against the post-parse
  `_ast` table. Adds an order constraint: must parse before infer.
- **Linter (`internal/linter/linter.go`)** uses tree-sitter for syntax
  checks during write-back. Either move to `gofmt -e` proxy (Go-only),
  drop the linter (the splice-validate step already catches syntax
  errors), or keep it as a shell-out to `gopls`/`golangci-lint`.

### Reversibility

Steps 2–3 are reversible: SitterWalker stays in-tree as fallback.
Step 4 is the commitment point — once SitterWalker is deleted and
`smacker/go-tree-sitter` leaves `go.mod`, reverting requires re-adding
the dep, which is annoying but not fatal.

## Decision criteria for the commitment point (step 4)

Before committing to step 4, the following must be true:

- [x] Steps 2–3 have shipped (#311, #312, #313, #314, #315);
  `mache-iegm` epic close-out tracked separately
- [ ] `mache-33dc5f` (leyline bundling in mache release) has shipped
  OR is explicitly deferred with a documented "install leyline
  separately" path
- [ ] All 9 `find_smells` rules work end-to-end against an
  ll-open-built `.db` with no SitterWalker code path involved
- [ ] CI passes 10 consecutive runs without a single `mache-2y9w`-class
  retry (leading indicator that the migration is right)
- [ ] `internal/writeback/validate.go` has a non-CGO answer (subprocess
  or alternative validator)

Not all need to land in this ADR's lifetime — the criteria can be
tracked as a checklist beads on `mache-37ae8b`.

## References

- `mache-2y9w` (CGO SIGSEGV bead) — partial mitigation in PR #257, #299; documentation in PR #300
- `mache-37ae8b` (CGO removal bead) — the work this ADR codifies
- `mache-33dc5f` (leyline binary bundling) — operational pre-req
- PR #284, #292, #294, #297 — recent reruns caused by `mache-2y9w`
- `docs/ARCHITECTURE.md` "Interplay with ley-line-open" — current
  capability matrix; collapses after this migration
- `internal/ingest/ast_walker.go` — the pure-Go replacement, already
  present and selected via `SelectWalker` when `_ast` is available
