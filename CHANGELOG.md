# Changelog

All notable changes to mache are documented here. This project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); pre-1.0 minor
bumps may include breaking changes.

## [Unreleased]

### Added

- **`mache install` / `mache uninstall` — provisioning that ships inside the
  binary.** Until now provisioning lived only in the Taskfile, so it required a
  repo checkout: a user who installed from a release asset or a package manager
  had a binary that could not provision its own REQUIRED backend. leyline has
  been mache's sole parser since ADR-0012 step 4, so that gap is the difference
  between an installed mache and a working one. `mache install` copies the
  running binary into a bin directory (default `~/.local/bin`, the same place
  `task install` uses) and provisions the exact pinned ley-line-open release
  through `leyline.EnsureCachedBinary`, which SHA-verifies the download and
  writes only the version-namespaced cache path — the unversioned
  `~/.mache/bin/leyline` stays never-written (`mache-0acdf6`), and leyline is
  never placed on PATH by copy, symlink or shim, since that recreates the
  "which version does bare `leyline` mean" question the pin exists to answer.
  Your shell rc is not touched: the `export PATH=…` line is printed, and
  `--update-rc` is an explicit opt-in that writes one idempotent marked block
  `uninstall --update-rc` can remove again. Both commands refuse a
  Homebrew-managed mache rather than fighting brew's manifest — the exact
  shadowing that cost an afternoon in `mache-6ec106`. `--dry-run` reports every
  change without making one. (`mache-19326d`)

- **`task install:verify` — an install gate that verifies a real installation
  instead of assuming one.** Every other gate in this repo exercises the source
  tree; nothing checked an artifact a user could actually have (`mache-4d7f2c`).
  The new target drives an *installed binary* by fork/exec and MCP-over-the-wire
  and asserts on content, not exit codes: the leyline the binary resolves is the
  pin — proven against a deliberately stale leyline planted first on PATH, the
  measured `mache-19326d` skew; the version it reports is one this tree could
  have produced, down to the built commit being an ancestor of HEAD; and
  `find_definition` / `get_overview` / `list_directory` / `find_smells` return
  the *expected values* for a fixture the binary projects itself. Runnable
  locally, wired into `task check`, `task ci` and a CI job that invokes the same
  target. `task install:verify:docker` adds the clean-HOME leg — no `~/.mache`,
  no checkout, no Taskfile — against the published image, which is
  `mache-19326d`'s acceptance criterion and the one condition a dev box cannot
  honestly simulate; it also pins the image's `serve`-baked `ENTRYPOINT`, so
  `docker run IMAGE version` silently booting a server (`mache-504adc`) is a
  stated expectation rather than a surprise. The docker leg skips when no daemon
  or image is reachable, so it never reddens CI on a box without docker; set
  `MACHE_VERIFY_REQUIRE_DOCKER=1` to make those skips failures.
  (`mache-19326d`, `mache-4d7f2c`, `mache-504adc`)

- **`mache leyline path` and `mache leyline exec` — invoke the pinned leyline
  without depending on PATH.** mache resolves leyline through one pin-checked
  chokepoint and caches it version-namespaced under `~/.mache/bin`, which is not
  on PATH. A human or agent typing bare `leyline` got none of that. Measured on
  a machine that had run `task install`: `~/.local/bin/leyline` reported 0.10.3
  while the pin was v0.13.0, so `leyline cdc enable --db x.db` ran a 0.10.3
  parser against a `.db` mache built with 0.13.0 — invisible, since both
  binaries are named `leyline` and neither announces the mismatch. `mache leyline path` prints the resolved absolute path and nothing else
  (`$(mache leyline path) cdc enable --db x.db`); `mache leyline exec -- <args>`
  proxies straight through, forwarding args, stdio and exit status. Neither
  mutates PATH, shell rc, or the unversioned `~/.mache/bin/leyline` path, which
  stays never-written (`mache-0acdf6`). (`mache-19326d`)

- **Doc-vs-CLI drift gate.** Every `mache …` invocation in a shell block of
  `README.md`, `GETTING-STARTED.md`, `examples/README.md` and `docs/*.md` is now
  resolved against the live cobra command tree: the subcommand must exist, its
  flags must exist on the command they are attached to, and the positional count
  must satisfy that command's `cobra.Args`. Documented `docker run` lines are
  checked against the real `ENTRYPOINT`/`CMD` of the image they name, since args
  append to ENTRYPOINT and replace CMD. Ground truth is `rootCmd.Commands()`,
  pflag's registry, `apko.yaml` and `Dockerfile.release` — no hardcoded command
  list and no scrape of `--help`. Same shape as the existing README-vs-registry
  and image-tag-vs-buildinfo pins. It proves a documented command parses, not
  that it succeeds. (`mache-d55457`, `mache-504adc`)

### Fixed

- **Four documented invocations that do not exist.** `examples/README.md`
  documented `mache mount <source> --schema <s> <mnt>`; there is no `mount`
  subcommand — mounting is the root command, so the source belongs in `--data`
  and the mountpoint is the single positional (`mache-d55457`).
  `GETTING-STARTED.md` documented `mache serve --schema … -d ./src`, but `serve`
  has no `-d`; the source is its positional. `docs/README.md` referred to
  `mache push` / `mache pull` rather than `mache cache push` / `mache cache pull`. `README.md` referred to a `mache mount` subcommand in prose twice.
  All four were found by the new gate.

- **The published image had no documented invocation.** `ghcr.io/agentic-research/mache`
  bakes `serve` into its `ENTRYPOINT`, so `docker run IMAGE serve --stdio …`
  expands to `mache serve serve …` and fails with `accepts at most 1 arg(s), received 2`. The README now shows the correct form for it, verified by running
  it against the real published image. The nearby distroless `mache:<version>`
  line was already correct — that image (built by `task image` from `apko.yaml`)
  has a bare `mache` entrypoint, and the two must not be conflated.
  (`mache-504adc`)

## [v0.20.0] — 2026-07-30

### ⚠️ Action required before upgrading

- **Rebuild any persisted `.db` built before ley-line-open v0.11.0.** Do not
  reuse it. mache now pins leyline v0.13.0, and v0.11.0 raised
  `IR_SCHEMA_VERSION` from `merkle-ast-v1` to `merkle-ast-v2` — giving
  `function_signature_item` the `canonical_kind` it lacked rewrote every Rust
  trait signature's `node_hash`, because the preimage hashes
  `canonical_kind(raw).unwrap_or(raw)`.

  **Sources are byte-identical across that change**, so nothing mache has can
  detect it: cache lockfiles hash raw source bytes and the parse skip is
  mtime+size. A `.db` built by a pre-0.11 leyline and served afterwards carries
  v1 addresses while the binary computes v2, silently. Most paths write a fresh
  temp `.db` per parse, so the exposed case is specifically
  `mache build -o x.db` followed later by `mache serve x.db`
  (`mache-438104`).

  `.db` files now record which leyline produced them — `_mache_meta` carries
  `leyline_pin`, `leyline_version`, and `leyline_source` — so this is
  answerable from the artifact rather than by inference.

  The later bumps in this release do **not** add a second rebuild trigger:
  v0.11.3 → v0.12.1 → v0.13.0 were each verified by measurement to hold
  `ir_schema_version` at `merkle-ast-v2` and `extraction_epoch` at `4`. Only
  `injection_epoch` moves (v0.13.0's markdown inline grammar), which is
  correctly scoped and forces no re-parse. The v0.11.0 boundary above is the
  only one in this release that invalidates a persisted `.db`.

### Fixed

- **Constructs were silently dropped on name collision** (`mache-c725e9`, P0).
  Two constructs whose schema name rendered the same string took the same node
  ID, and the last write won; the earlier ones vanished with no error and no
  diagnostic. On ley-line-open's Rust workspace **3,597 of 4,137 functions
  reached the projection — 540 lost**. On mache's own source, 1 of 10
  `func init()` in `cmd/` survived, and `cmd/mount.go`'s init — the one
  registering `--schema`/`--data`/`--control` — was absent from mache's own
  graph entirely.

  The dedup guard existed but was unreachable: it gated on
  `store.GetNode(id).Children`, and `SQLiteWriter.GetNode` never populates
  `Children`. It was live on `MemoryStore` and dead on the build path. Claimed
  IDs are now tracked in the engine rather than read back from a store, so
  collision detection no longer depends on which backend is written to.
  Post-fix the projection returns **4,137 — exactly the `_ast` ground truth**.

- **`nodes.record` was declared `JSON`, which gives it NUMERIC affinity**
  (`mache-4b8a42`). SQLite picks affinity by substring-matching the declared
  type; `JSON` matches none, so it fell through to NUMERIC and rewrote any
  text that parses as a number (`'007'` → `7`, `'1.10'` → `1.1`). Ingest was
  immune by accident — it binds `[]byte`, and BLOBs bypass the conversion —
  but write-back through the mount binds a string and did not.

- **Rust test code was judged as production API** (`mache-c7d56d`). Go states
  test-ness in the file name; Rust states it in an attribute and colocates it
  in the production file, so no path predicate can see it. On ley-line-open/rs,
  46% of files carry `#[cfg(test)]`. `fan_out_skew` dropped 176 → 86 findings
  (51% were test helpers, which legitimately call many things to build
  fixtures) and `duplicate_definitions` 654 → 414.

- **Rust dead-code findings from Cargo layout** (`mache-8ecfd7`). `tests/`,
  `benches/`, and `examples/` are separate crates whose entry points the
  harness invokes; every function in them read as dead. 2,524 → 1,986.

- **Children were discarded on the build path when re-fetching a node**
  (`mache-e3d9bb`). `processNode` re-read a node to "preserve Children +
  Properties", but those have different contracts: `Properties` round-trips
  through the `props` column on every store, `Children` does not round-trip
  through `GetNode` on any SQLite-backed one. The line dropped children the
  recursion had just added — on the build path only, since `MemoryStore`
  returns the live node and answers correctly.

  Children now have **one accessor**. `ListChildren` is correct on every
  backend: `SQLiteWriter` implements it against `parent_id` (its previous
  `return nil, nil // Not used during ingest` read like a constraint and was
  not one — rows are readable inside their own open transaction), and
  `bufferingTarget` unions the file nodes it defers for the later swap. No
  production code reads `GetNode().Children` any more.

  This also normalised the **error contract**, which the field disagreement was
  masking: `MemoryStore.ListChildren` returns `ErrNotFound` for a missing node
  while `SQLiteWriter` returns an empty set, so the backends disagreed about
  whether *asking* is an error, not just about the answer.

- **`mache serve`'s auto-leyline log described a path that no longer exists**
  (`mache-6dda39`). It claimed the fallback used `SitterWalker`, removed in
  v0.18.0, and pointed at `MACHE_NO_LEYLINE=1` as "the in-process watcher path"
  without saying that path now needs `leyline` to pre-parse — while the same
  flag disables downloading it. The message states the precondition and names
  the pinned version.

- **`fan_out_skew`'s corpus-wide mean was diluted by markdown refs**
  (`mache-50e939`). ley-line-open v0.13.0's markdown backtick fix
  (`ley-line-open-ea1e42`) gives code spans real `node_refs` rows, but a
  doc citation has no enclosing function — `container_node_id` is NULL,
  so `referrer_node_id` falls back to the span's own unique node_id and
  each becomes a singleton fan-out-1 referrer group. Measured on mache's
  own repo: the mean dropped 9.79 → 5.08 once markdown refs existed,
  lowering the 3× threshold for every real function and producing ~300
  false positives that reflected a diluted mean, not a complexity change.
  Fixed by scoping the mean to referrer_node_ids that resolve to a real
  `nodes` row (markdown code spans never get one) rather than every raw
  `v_refs` referrer.

### Changed

- **`nodes.record` is now TEXT in every writer, and holds TEXT.** Both the
  declared type and the bound value changed. Ingest previously bound `[]byte`
  (stored BLOB) while write-back bound a string (stored TEXT), and SQLite never
  considers a BLOB equal to a TEXT value — so `WHERE record = ?` matched only
  nodes that had been written back and silently missed every ingested one
  (`mache-6bed54`). **`typeof(record)` flips `blob` → `text` for existing
  ingested rows**; content bytes are unchanged, and anything diffing `.db`
  files bytewise or asserting on `typeof` will see it.

- **The leyline binary cache is namespaced by pinned version**
  (`mache-0acdf6`). It was one unversioned path, so every mache build on a
  machine treated that file as its own and they overwrote each other — observed
  going v0.10.4 → v0.10.2 → v0.10.4 within a single session while different
  builds ran. Since LLO ships `_ast` schema changes in patch releases, the
  graph shape silently depended on which mache last touched the cache.
  `~/.mache/bin/leyline-v0.11.3` and `-v0.10.3` now coexist.

- **Node properties moved to a dedicated `props` column** (`mache-90b89b`).
  `Node.Properties` was `map[string][]byte`, and `encoding/json` renders
  `[]byte` as base64 — so a projection stored
  `{"imports":"eyJmbXQiOiJmbXQifQ==","lang":"Z28="}` where it meant
  `{"imports":{"fmt":"fmt"},"lang":"go"}`, double-encoding `imports` (already
  JSON) and inflating the record 2.3x. The costly part was not the size but
  that `json_extract(record,'$.lang')` returned `Z28=`, putting `lang`/`pkg`/
  `imports` out of reach of every SQL consumer and smell rule. They are now
  real nested JSON in their own column: `json_extract(props,'$.lang')` returns
  `go`, and nested fields like `$.imports.fmt` resolve.

- **`record` means two things instead of three.** It held a source data record
  *or* inline rendered content *or* serialized properties; the third is gone.
  The `CASE WHEN kind = NodeKindDir` guard in `NodesTableReader` is deleted —
  it existed only to stop a `GetNode` shipping a file's whole body, and `props`
  never holds content, so the separation is structural now.

- **`Node.Properties` is `map[string]json.RawMessage`.** Access it through
  `PropString`/`PropRaw` and their setters, re-exported from the public `graph`
  package for external consumers.

- **Node properties moved to a dedicated `props` column** (`mache-90b89b`).
  `Node.Properties` was `map[string][]byte`, and `encoding/json` renders
  `[]byte` as base64 — so a projection stored
  `{"imports":"eyJmbXQiOiJmbXQifQ==","lang":"Z28="}` where it meant
  `{"imports":{"fmt":"fmt"},"lang":"go"}`, double-encoding `imports` (already
  JSON) and inflating the record 2.3x. The costly part was not the size but
  that `json_extract(record,'$.lang')` returned `Z28=`, putting `lang`/`pkg`/
  `imports` out of reach of every SQL consumer and smell rule. They are now
  real nested JSON in their own column: `json_extract(props,'$.lang')` returns
  `go`, and nested fields like `$.imports.fmt` resolve.

- **`record` means two things instead of three.** It held a source data record
  *or* inline rendered content *or* serialized properties; the third is gone.
  The `CASE WHEN kind = NodeKindDir` guard in `NodesTableReader` is deleted —
  it existed only to stop a `GetNode` shipping a file's whole body, and `props`
  never holds content, so the separation is structural now.

- **`Node.Properties` is `map[string]json.RawMessage`.** Access it through
  `PropString`/`PropRaw` and their setters, re-exported from the public `graph`
  package for external consumers.

- **leyline pin: v0.12.1 → v0.13.0** (`mache-438104`). Verified by
  measurement, not release notes: `_meta.ir_schema_version` and
  `extraction_epoch` unchanged (`merkle-ast-v2`, `4`); only
  `injection_epoch` shifted, correctly scoped to markdown's inline grammar
  now running where it didn't before. **No `.db` rebuild required for this
  bump.**

### Added

- **The LLO data-plane boundary is enforced** (`mache-e64f36`). mache is the
  control plane — smell rules, queries, code intelligence, the MCP surface —
  and ley-line-open is the data plane. A ratchet test fails when a file outside
  a written allowlist writes an LLO-owned table, so "mache does not do
  data-plane work" is checkable rather than folklore. Three files are
  allowlisted with reasons; the set may only shrink.

- **`TAG=vX.Y.Z task leyline:pin-bump`** rewrites the four platform SHA-256
  digests, the pinned version, the pin test, and `server.json`'s
  `minimum-version` in one step, and fails closed if a release does not carry
  all four platform assets. It deliberately refuses to write the doc block or
  decide the lineage question.

- **`drift_doc_dead_symbol_reference` is real** (`mache-eb2bf3`), replacing
  the `WHERE 1=0` placeholder now that ley-line-open v0.13.0 gives
  markdown backtick spans `node_refs` rows. Scoped to Rust paths
  (`token LIKE '%::%'`) for v1 — Go has no `::` syntax, sidestepping the
  open `ley-line-open-651909` (Go package-level consts emit no defs)
  entirely rather than false-positiving on it. Two known false-positive
  classes are documented on the rule rather than filtered with a fragile
  allowlist: external-library citations, and Rust enum-variant/struct-field
  access (extraction doesn't index those as defs). Not tagged `gate`:
  advisory only.

### Removed

- **Reading node properties out of `record`.** A `.db` written by mache before
  this change is refused on open with an error naming `mache build` as the fix.
  Files produced by `leyline parse` are unaffected: they never carried
  properties, and refusing them would break mache's primary input.

## [v0.19.0] — 2026-07-22

### Changed

- **`server.json` now describes tiers, not an optional dependency.** The
  `_meta…/tools` block split tools into `standalone` (15) vs
  `requires-ley-line-open` (3) — but nothing has been standalone since v0.18.0
  made `leyline parse` the sole source parser. Tools are now grouped by the
  ley-line-open artifact they need: `base` (14), `lsp` (2), `embeddings` (1),
  `any` (1). `optional-deps` becomes `required-deps`, and its version floor is
  now **derived** from the pin mache actually verifies (`leyline.BinaryVersion`)
  instead of a separately-maintained string that had drifted to `0.4.5` against
  a `v0.8.0` pin. Consumers reading the `_meta` block should expect the new keys.
- **`task image` tags from `internal/buildinfo/version.txt`.** It hardcoded
  `mache:0.8.0` — ten releases stale — so `task image` produced a
  wrongly-tagged artifact and the documented `docker run` line referenced an
  image that was never published.

### Fixed

- **Node `Properties` survive to serve time.** `SQLiteWriter` persists a dir
  node's `Properties` (`lang` / `pkg` / `imports`) into the nodes-table
  `record` column, and the build-time reader restored them — but
  `NodesTableReader.GetNode` (the **serve** path) never selected `record`, so
  every construct read from a `.db` silently lost them. Qualified-callee
  resolution had been papering over this with a regex that scraped Go `import`
  statements out of context text. The `record` fetch is guarded SQL-side
  (`CASE WHEN kind = dir`) so file-node content is never shipped just to answer
  a `GetNode`.
- **Phantom `json_extract` columns in the scan query.** Field paths were pulled
  from name templates with a regex over the raw string, so a literal dot in
  surrounding text (`report.txt {{.id}}`) produced a bogus `$.txt` column that
  always resolved NULL. Templates are now parsed and only genuine field
  references are collected.
- **Dot imports are no longer mis-resolved.** The deleted import regex could not
  capture `.` in its alias group, so `. "os"` silently degraded into a normal
  `os` import.

### Internal

- Regex removed from every place it was doing heuristical matching: the two
  leyline test helpers now call the production `leyline.ResolveBinary`, and two
  production regexes were replaced with structural matching (a digit check for
  ISO date prefixes; `text/template` parsing for field references). The two that
  remain are justified in place — one compiles a regex supplied by the schema
  author in a tree-sitter `#match?` predicate (the regex *is* the contract).
  A `go/ast`-based ratchet fails any new `regexp` import that lacks a reason.
- New repo invariants, all auto-running under `task test` → `task ci` →
  pre-push: dependency usage-surface analysis (fails on a new minimal-surface
  dependency without a justification; `task deps:surface` prints the report),
  README-vs-registry tool-matrix pinning, and image-tag-vs-buildinfo pinning.
- **`.golangci.yml` is now committed.** `task lint` previously ran on whatever
  the installed golangci-lint defaulted to. The set is pinned explicitly and
  gains `bodyclose`, `misspell`, `unconvert`, `wastedassign`, `copyloopvar`.
- `task fmt` now also formats changed markdown, so the pre-commit `mdformat`
  hook stops rewriting files out from under a commit.

### Removed

- **Dead `leyline_fs` CGO FFI surface** (`internal/leyline/client.go`,
  `internal/leyline/doc.go`). It was gated behind the `//go:build leyline` tag,
  compiled by no build/CI/task, referenced by nothing, and hardcoded a cgo path
  into the **private** `ley-line` repo (`../../../ley-line/rs/crates/fs/…`) — a
  path that no longer exists there (the `fs` crate now lives in public
  ley-line-open at `rs/ll-open/fs/`, which publishes `libleyline_fs`). This was
  the last reference to the private ley-line repo in mache and completes
  ADR-0006 Thread 4 ("delete `internal/leyline/client.go`"). mache consumes
  leyline purely as a subprocess/daemon over its UDS socket; the release binary
  was already pure-Go and pulled zero symbols from `libleyline_fs`, so there is
  no behavior change. Docs (README / ARCHITECTURE / ROADMAP), `release.yml`, and
  `melange.yaml` comments that described the FFI as a "kept dev-only surface"
  are updated accordingly.

## [v0.18.0] — 2026-07-21

The CGO-removal wave, part two — and the end of it: the in-process tree-sitter
backend is gone, ley-line is the sole source parser, and the released binaries
are built `CGO_ENABLED=0`. Removing the in-process projector exposed an
O(nodes²) blowup in the pure-Go `ASTWalker` and a silently-dead `find_callees`
on the serve path; both are fixed and verified here.

### Changed

- **Pure Go — in-process CGO tree-sitter removed** (ADR-0012 step 4). Every
  source path (`build` / `serve` / `mount` / `infer` / testfixtures) routes
  through `leyline parse` → `_ast` → the pure-Go `ASTWalker`. `SitterWalker`,
  `sitter_flatten`, the 28 vendored grammar bindings and the `go-tree-sitter`
  dependency are deleted (−428K lines of generated `parser.c` alone), and the
  release builds `CGO_ENABLED=0`. The pinned leyline parses 27 of 28 registry
  languages (all but cue, which has no compatible grammar crate); a schema
  coverage guard reports the gap loudly. The `leyline_fs` FFI stays behind its
  separate `//go:build leyline` tag. (`mache-37ae8b`, #533)

### Fixed

- **O(nodes²) projection blowup → O(nodes)** — porting SitterWalker's in-memory
  tree walk to per-node SQL made every child lookup re-scan all same-kind nodes
  in the file, so a whole-repo projection took >3 min (and hung ~20 min in
  testfixtures). Fixed with a per-file in-memory node index, once-per-file
  call/ref extraction, and build-only read tuning (mmap). **cmd/ 4m22s → 3.1s
  (~78×); whole repo >3 min → ~10s**, with output proven byte-identical to the
  pre-fix baseline (0 differing rows across `nodes`/`node_refs`/`node_defs`).
  (`mache-4f3840`)
- **`find_callees` silently returned nothing on serve/mount** — the call
  extractor fed the *graph node id* into `_ast` as `source_id`, matching zero
  rows every time; test coverage hid it behind a regex stand-in. Restored via a
  scoped+qualified `_ast` query, with the construct's `_ast` scope persisted at
  projection time, plus a `node_refs` fallback so it also works on a
  schema-built `.db` served without an `_ast` table. (`mache-fd9982`,
  `mache-6fbaf1`)
- **Read-connection tuning no longer mutates served databases** — the
  single-connection + `locking_mode=EXCLUSIVE` tuning is scoped to the one-shot
  build that owns its temp db; it previously clobbered the served graph's pool
  and held a process-lifetime file lock. (`mache-010123`)
- **Whole-file extraction surfaces DB errors instead of caching partials** — a
  transient failure could permanently empty a file's callees/refs with no
  signal. (`mache-015f5c`, `mache-6ff371`)
- **ASTWalker caches invalidated on re-ingest**, and the unsynchronized
  `childSeen` map is now mutex-guarded against concurrent `ReIngestFile`.
  (`mache-018eee`, `mache-024e9c`, `mache-706757`)

### Internal

- CI provisions the pinned leyline before tests, so the sole-parser projection
  tests run instead of silently skipping. (`mache-01c467`)
- Whole-repo E2E fixtures retiered out of the `-race` unit run (they blew the
  20-minute budget under the race detector) and run non-race in the integration
  workflow. (`mache-4f3840`)
- `modernc.org/sqlite` 1.53.0 → 1.54.0. (#532)

## [v0.17.0] — 2026-07-14

The CGO-removal wave, part one: the structural smell gate runs entirely on the
leyline `_ast` projection, schema builds and FCA inference no longer need the
in-process tree-sitter backend, and the leyline binary pin is exact.

### Changed

- **Exact leyline pin (correctness fix)** — `leylineVersionMatchesPin` now
  requires exact `major.minor.patch`. Previously the patch floated, so a stale
  PATH leyline (e.g. 0.7.3) satisfied the v0.7.5 pin and silently produced dbs
  missing `container_node_id`/`canonical_kind`, zeroing `fan_out_skew`/
  `untested_function` locally while CI saw them. LLO patch releases can change
  the emitted `_ast` schema, so the patch does not float. (`mache-608a3c`, #523)
- **Single leyline smell gate** — `task smells` builds the tracked tree via the
  pinned leyline and runs all `gate`-tagged rules against one unified
  `docs/smell-baseline.json`; the pure-Go tree-sitter gate and
  `smell-baseline-ast.json` are retired. `untested_function` ported to the
  leyline projection (caller resolution via `container_node_id`, Go-convention
  scope, fixture-corpus exclusion; 757→8 on mache) and `dead_code` excludes
  `testdata/`/`test_data/` corpus (321→16). (`mache-608a3c`/`mache-ba24f3`, #523)
- **`--schema` works on the leyline backend** — `mache build --schema X`
  projects via the pure-Go Engine+ASTWalker over the leyline parse (parity-gate
  proven byte-identical to the in-process projection). A coverage guard makes
  unparseable schema languages LOUD: hard error on `--backend=leyline` (naming
  the language and the tree-sitter escape hatch), warning on auto — including
  preset refs like `--schema sql`. (`mache-73b885`, #524)
- **FCA inference from `_ast`** — `mache infer` and mount `--infer` leyline-parse
  once and infer per language from the `_ast` tables (`FlattenASTDB`/
  `InferFromASTDB`); `sitter.NewParser` is gone from both paths. Record and
  topology parity with the tree-sitter path is test-pinned. (`mache-73b885`, #524)
- **README repositioned** — mache is a schema-driven engine that projects
  structured data into a navigable graph; code intelligence is the flagship
  application. Tool count corrected to eighteen. (`mache-fe558b`, #521)

### Added

- **`examples/smell-rules/` starter kit** — copyable rules + a newcomer README
  covering the rule JSON shape, builtin/overlay composition, baseline bootstrap,
  a complete workflow snippet, and the ratchet model. (`mache-fe3a4a`, #522)
- `TestFindSmellsAction_TaskfileParity` pins the composite-action ↔ Taskfile
  gate contract, including the deliberate divergences that reconverge when the
  action's default `mache-version` bumps to this release. (`mache-fe3a4a`/#522,
  `mache-608a3c`/#523)

### Fixed

- **Composite action `schema` input was silently ignored** on ≥ v0.13.0 — the
  auto backend prefers leyline, which (pre-#524) did not honor build-time
  schemas. The action forces `--backend tree-sitter` when `schema` is set;
  with this release's schema-on-leyline that forcing becomes unnecessary and
  is removed in the action reconvergence follow-up. (`mache-fe3a4a`, #522)

## [v0.16.2] — 2026-07-08

`server.json`'s OCI package now follows the ADR-0041 ecosystem contract; this
release also ships deterministic community detection and watcher-driven sheaf
cache invalidation.

### Changed

- **`server.json` `packages[].oci` → ADR-0041 canonical form** — `identifier` is
  the tagless image name (`ghcr.io/agentic-research/mache`) with `version` = the
  git tag (`v0.16.2`). cloister's resolver resolves tag→digest and pins
  `identifier@digest`. Supersedes v0.16.1's tag-in-identifier shape, which
  ADR-0041 rejected. (`mache-39e443`)

### Added

- **Deterministic community detection** — connected-components post-split, O(E)
  modularity, and sorted tie-breaks, so `get_communities` returns identical
  partitions run-to-run (the sheaf cache key is now stable). (`mache-e1fa1f`,
  `mache-ff7e31`)
- **Watcher-driven sheaf invalidation** — mache consumes LLO v0.6+'s
  `daemon.sheaf.invalidate` events (region-scoped `current_root`/`generation`
  payload) to flush MCP-tool caches incrementally instead of polling; the dead
  legacy `sheaf.invalidate` subscription was dropped. (`mache-e4c4ea`,
  `mache-30c634`)

## [v0.16.1] — 2026-07-08

### Fixed

- **`server.json` OCI tag drift** — `packages[].oci` declared
  `identifier: ghcr.io/agentic-research/mache` + `version: 0.16.0` while the
  image is tagged `:v0.16.0`, so a resolver reading `identifier:version` hit a
  nonexistent `:0.16.0`. (Reshaped to the ADR-0041 form in v0.16.2.)
  (`mache-03745c`)

## [v0.16.0] — 2026-07-08

Smell rules become data, and mache's gates start enforcing.

### Added

- **Gate-preflight lint** (`internal/lint`, `tools/gate-preflight`) — flags
  Taskfile gate tasks that invoke an external tool without a preflight guard
  (`command -v` / `preconditions:` / `go install` / artifact check), so a
  missing tool fails loud instead of silently no-opping a gate. Wired as
  `gates:preflight` into `check` + `ci`. (`mache-f0e96a`)

### Changed

- **Smell rules externalized to embedded JSON** — the 14 builtin rules moved
  from Go struct literals to `cmd/rules/*.json`, loaded via `//go:embed`. Rules
  are now data (same SQL, byte-identical findings vs baseline); external
  `MACHE_SMELL_RULES_DIR` rules still append/override. (`mache-b0b979`)
- **`find-smells.yml` now enforces** — runs `task smells` on PRs and fails on new
  findings vs `docs/smell-baseline.json` (was advisory / never-fail).

## [v0.15.0] — 2026-07-08

The merkle IR release: mache reads ley-line-open's content-addressed AST.

### Added

- **`node_hash` reader passthrough** — `v_defs`/`v_refs` expose the merkle
  `node_hash` additively (probe-guarded; `NULL` on standalone dbs, so existing
  output is byte-identical), plus a one-to-many fan-out guard (a deduped subtree
  appears at many occurrences; resolution stays per-occurrence, never keyed on
  `node_hash`). (`mache-ff9a9d`, `mache-ffabd1`)

### Changed

- **Pinned leyline v0.5.7 → v0.6.0** — the merkle-AST IR producer:
  content-addressed `node_hash` + deduped `node_content` replace the old
  location-keyed `symbol_id`/`symbols`/`fact_edges` (identical subtrees store
  once). `go.mod` `leyline-schema` + `leylineBinaryVersion` both move to 0.6,
  kept in lockstep by the version-parity gate. (`mache-3f0d59`)

### Fixed

- **De-flaked `TestProvenance_RecordAndReport`** under `-race` — the version
  probe's 2s exec timeout was starved by full-suite contention; extracted to an
  overridable `probeTimeout` (prod behavior unchanged). (`mache-3a0da5`)

## [v0.14.0] — 2026-07-04

The "OCI distribution" release. mache now self-publishes a container image and
declares where it's sourced from, so downstream (cloister et al.) resolve a
version-pinned artifact instead of a hand-maintained, drift-prone tag.

### Added

- **Published OCI image** `ghcr.io/agentic-research/mache` — a leyline-bundled,
  multi-arch (linux/amd64 + linux/arm64) image built + pushed + cosign-signed
  (keyless) on every release tag. Containers get the full 10-rule LLO analysis
  with zero runtime download. Base is `ubuntu:24.04` (glibc 2.39 — leyline
  requires it; the old `debian:bookworm-slim` shipped 2.36 and leyline aborted
  at runtime). Tooling-native entrypoint (`mache serve`; config via mounted
  `.mache.json` + `MACHE_*`). `task image:verify` + a release-job runtime smoke
  guard the base/glibc contract.
- **`server.json` `packages[].oci`** — mache declares its own image source
  (`registryType: oci`, `identifier`, `version`), version single-sourced from
  `version.txt` so it can't drift from the pushed tag. Aligns with cloister
  ADR-0038.

### Changed

- **Docs:** corrected the `find_smells` rule count (9 → 14: 10 code-structure +
  3 doc-drift + 1 test) and added an OCI-distribution note.

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
[v0.14.0]: https://github.com/agentic-research/mache/compare/v0.13.0...v0.14.0
[v0.8.0]: https://github.com/agentic-research/mache/compare/v0.7.0...v0.8.0
