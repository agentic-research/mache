# Adversarial Review: v2-workspace-refactor

Reviewed: 2026-03-30 | Branch: `v2-workspace-refactor` | 16 commits, +1983/-303, 21 files

## How to use this document

Each finding has inline tests and source comments. Run the failing tests:

```bash
task test -- -run 'TestASTWalker_NotEqPredicateSilentlyIgnored|TestASTWalker_CaptureOriginNamedCapture' -v
```

Both tests fail intentionally to surface bugs. Fix the code, tests go green.

______________________________________________________________________

## Findings

### F1: `#not-eq?` and other predicates silently ignored — FIXED

**Source:** `internal/ingest/ast_walker.go:160-167` — rejection loop for unsupported predicates
**Test:** `internal/ingest/ast_walker_test.go` — `TestASTWalker_NotEqPredicateSilentlyIgnored`
**Status:** PASSING (fixed)

`parseSelector` now rejects `#not-eq?`, `#any-eq?`, `#is?`, `#is-not?` with
an error message directing to SitterWalker.

### F2: `CaptureOrigin` only handles `@scope` — FIXED

**Source:** `internal/ingest/ast_walker.go` — `astMatch.captureRanges` field + `CaptureOrigin` method
**Test:** `internal/ingest/ast_walker_test.go` — `TestASTWalker_CaptureOriginNamedCapture`
**Status:** PASSING (fixed)

`astMatch` now stores per-capture byte ranges in `captureRanges map[string][2]int`,
populated during `Query()` from the `_ast` table. `CaptureOrigin` checks
`captureRanges[name]` for any named capture before falling through to `(0,0,false)`.

### F3: `findChildByKindAST` LIMIT 1 nondeterminism (CONCERN)

**Source:** `internal/ingest/ast_walker.go:370-380` — BUG comment on `findChildByKindAST`
**Test:** `internal/ingest/ast_walker_test.go` — `TestASTWalker_MultipleChildrenSameKind`
**Status:** PASSING with warning log (documents the limitation)

When multiple descendants share the same `node_kind`, SQLite returns one
without ORDER BY. Tree-sitter distinguishes by field name (e.g., `name:` vs
unnamed). ASTWalker only matches by kind.

**Fix (if needed):** Add `ORDER BY a.start_byte ASC` to get deterministic
first-occurrence behavior. For full parity, would need field-name awareness
in the `_ast` table.

### F4: Address refs silently dropped for ASTWalker path (CONCERN)

**Source:** `internal/ingest/engine.go:778-789` — BUG comment on address ref extraction
**Test:** No dedicated test (would require ASTWalker integration fixture)

When `w` is `*ASTWalker`, `ExtractAddressRefs` is skipped — `fileAddrRefs`
stays nil. For Go files, `env:*` refs from `os.Getenv` won't be captured.
Schema behavior changes silently based on walker selection.

**Fix:** Either implement `ASTWalker.ExtractAddressRefs` (query `_ast` tables
for the same patterns), or add a `log.Printf("WARN: ...")` so the gap is visible.

### F5: `parseSelector` string mutation (OBSERVATION)

**Source:** `internal/ingest/ast_walker.go:175-190` — NOTE comment
**Test:** `internal/ingest/ast_walker_test.go` — `TestParseSelector_MultiplePredicates`
**Status:** PASSING (works with 2-3 predicates, fragile pattern)

The `#eq?` extraction loop mutates `s` via `s = s[:idx] + s[...]`. Tested
and passing, but if more predicate types are added, a scan-based approach
would be safer.

### F6: Integration test doesn't exercise ASTWalker (OBSERVATION)

**Source:** `cmd/serve_test.go:2314-2412` — `TestSQLiteGraphGoldenPath`

The golden-path test covers `.db → SQLiteGraph → MCP tools` (template
extraction + SQLiteResolver relocation). It does NOT exercise ASTWalker —
that requires a `.db` with `_ast` tables produced by ley-line, which doesn't
exist in test fixtures.

### F7: Architecture docs honest about scope (OBSERVATION)

**Source:** `_project_files/docs/v2-planning/decoupling-architecture.md:229`

Docs correctly state only Step 1 is done. `cmd/serve.go` still imports
`internal/ingest` — the tree-sitter dependency chain is not broken for
the serve command. Only `SQLiteGraph` is decoupled.

______________________________________________________________________

## Test summary

| Test                                          | Status        | Finding                           |
| --------------------------------------------- | ------------- | --------------------------------- |
| `TestASTWalker_NotEqPredicateSilentlyIgnored` | FAIL          | F1: silent predicate drops        |
| `TestASTWalker_CaptureOriginNamedCapture`     | FAIL          | F2: scope-only CaptureOrigin      |
| `TestASTWalker_MultipleChildrenSameKind`      | PASS (logged) | F3: LIMIT 1 nondeterminism        |
| `TestParseSelector_MultiplePredicates`        | PASS          | F5: string mutation (works today) |

### F8: Fuzz-found — bare `@` in `#eq?` produces empty capture predicate (CONCERN)

**Source:** `internal/ingest/ast_walker.go:271` — `parts[0][1:]` with `parts[0] == "@"`
**Corpus:** `internal/ingest/testdata/fuzz/FuzzParseSelector/f14711f50dc7f5df`
**Status:** Fuzz-found in \<1 second

Input `(#eq? @ 0)` passes the `strings.HasPrefix(parts[0], "@")` check, then
`parts[0][1:]` yields `""` — an empty capture name. The predicate is stored and
evaluated against `values[""]`, which will always be nil, so the match is
(incorrectly) filtered out.

**Fix:** Add `len(capName) > 0` check after extracting the capture name.

### F9: Concurrent queries PASS — no data races detected

**Tests:** `TestASTWalker_ConcurrentQueries`, `TestASTWalker_RaceSelectWalker`
**Status:** PASSING (no race detected with `-race` flag)

ASTWalker is safe for concurrent use. Note: `:memory:` SQLite cannot be used
for concurrent tests (each pool connection gets isolated DB). Tests use temp
files.

### F10: Template rendering — no crashes in 1.2M fuzz iterations

**Test:** `FuzzRender` in `internal/template/render_test.go`
**Status:** PASSING (no panics found)

______________________________________________________________________

## Running the review

All review tests (all should pass — F1, F2, F8 are fixed):

```bash
task test -- -run 'TestParseSelector_Multiple|TestASTWalker_NotEq|TestASTWalker_CaptureOriginNamed|TestASTWalker_MultipleChildren|TestASTWalker_Concurrent|TestASTWalker_RaceSelect|TestRender_Race'
```

Fuzz `parseSelector` (finds bugs fast — try 30s+):

```bash
CGO_CFLAGS="-I/usr/local/include/fuse" CGO_LDFLAGS="-F/Library/Frameworks -framework fuse_t -Wl,-rpath,/Library/Frameworks" \
  go test -tags boltdb -fuzz FuzzParseSelector -fuzztime 30s -run '^$' ./internal/ingest/
```

Fuzz template rendering:

```bash
CGO_CFLAGS="-I/usr/local/include/fuse" CGO_LDFLAGS="-F/Library/Frameworks -framework fuse_t -Wl,-rpath,/Library/Frameworks" \
  go test -tags boltdb -fuzz FuzzRender -fuzztime 30s -run '^$' ./internal/template/
```
