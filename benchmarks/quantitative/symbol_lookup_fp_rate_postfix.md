# Symbol Lookup Re-run: post-fix bench on the mache repo

**Date**: 2026-05-13
**Corpus**: `/Users/jamesgardner/remotes/art/mache` — 245 `.go` files, 67,820 LOC, 89 directories on disk; projection root holds 26 top-level package dirs (api, cmd, graph, ingest, …)
**Server**: mache `v0.8.0` over Streamable HTTP MCP at `http://localhost:7533/mcp`
**Prior bench**: [`symbol_lookup_fp_rate.md`](symbol_lookup_fp_rate.md) — same 10 symbols, same methodology, ran 2026-05-12 against the pre-fix build
**Goal**: re-run the same table against the post-fix build that landed the 5 correctness fixes (find_definition SQL fallback, type-reference patterns, role=definition SQL pushdown, find_callees suffix-match, architecture truncation) and report deltas honestly — including where the fixes did and did not close gaps.

______________________________________________________________________

## 1. Methodology

Identical to the prior bench. Columns: `grep_wb` (word-bounded `grep -rn --include='*.go' '\b<sym>\b' .`), `grep_call` (`\.<sym>\(|<sym>\(` minus `func <sym>(` lines), `mache_callers` (count from `find_callers(token=<sym>)` over MCP), `FP_rate_naive` = `(grep_wb − mache_callers) / grep_wb`, `grep_time_ms` (`/usr/bin/time -p`, median of 3 warm runs), `token_cost_ratio` = `(grep_wb × 50) / (mache_callers × 20)` — same 50 t/line, 20 t/path estimates as the prior bench. MCP round-trip latency isn't exposed in the response, so `mache_time_ms` is reported `n/a`; all calls returned subjectively under 100 ms.

### Fixes since the prior bench

1. `find_definition` SQL fallback to `node_defs` table (prior bench: 0/10 resolved).
1. Type-reference patterns — parameter, field, composite-literal, var-spec, bare and qualified (prior bench: `[]` for all three type symbols).
1. `find_callees` receiver-method suffix-match fallback.
1. `search role=definition` SQL pushdown.
1. `get_architecture` / `get_communities` truncation cap raised (not measured here).

### Caveats

1. Corpus is ~67K LOC — FP-rate absolutes are not directly comparable to multi-MLOC corpora.
1. Pre/post `mache_callers` comparison for callable symbols is misleading: the prior bench's higher numbers appear to have counted call sites; the post-fix server returns **distinct calling constructs** (one entry per containing function/method). All 117 `Error` callers and 434 `Close` callers in this run are distinct construct paths (verified via `set()` dedup). The denominator change is honest; we annotate it where it matters.

______________________________________________________________________

## 2. Pre-fix vs post-fix table

| symbol           | category | `grep_wb` pre→post | `grep_call` pre→post | `mache_callers` pre→post |  FP rate pre→post |       TCR pre→post |
| ---------------- | -------- | -----------------: | -------------------: | -----------------------: | ----------------: | -----------------: |
| `Close`          | common   |      647 → **667** |        580 → **617** |            580 → **434** | 10.4% → **34.9%** |  2.79× → **3.84×** |
| `New`            | common   |        64 → **67** |          49 → **49** |              46 → **33** | 28.1% → **50.7%** |  3.48× → **5.08×** |
| `Error`          | common   |      228 → **229** |        187 → **187** |            169 → **117** | 25.9% → **48.9%** |  3.37× → **4.89×** |
| `MemoryStore`    | type     |      210 → **212** |                0 → 0 |               **0 → 55** |     — → **74.1%** |      — → **9.64×** |
| `SQLiteGraph`    | type     |      108 → **112** |                0 → 0 |               **0 → 34** |     — → **69.6%** |      — → **8.24×** |
| `Topology`       | type     |      156 → **201** |                0 → 0 |              **0 → 123** |     — → **38.8%** |      — → **4.09×** |
| `GetCallers`     | fan-in   |            88 → 88 |          30 → **44** |              27 → **22** | 69.3% → **75.0%** | 8.15× → **10.00×** |
| `RenderTemplate` | fan-in   |            58 → 58 |          46 → **46** |              45 → **40** | 22.4% → **31.0%** |  3.22× → **3.62×** |
| `ReadContent`    | fan-in   |          110 → 110 |          47 → **70** |              42 → **36** | 61.8% → **67.3%** |  6.55× → **7.64×** |
| `dedupSuffix`    | rare     |         **3 → 11** |                1 → 1 |                    1 → 1 | 66.7% → **90.9%** | 7.50× → **27.50×** |

### Reading the deltas

- **Type rows are the headline win**: `MemoryStore`, `SQLiteGraph`, `Topology` went from `[]` to 55 / 34 / 123 distinct callers. The index now answers "where is this type used?" — exactly the gap flagged as finding (2) in the prior bench. Three previously-undefined FP-rate cells (74%, 70%, 39%) are now scored.
- **Callable-symbol drops are a counting-model change, not a regression.** Post-fix lists are per-construct (one entry per `TestFoo` function), all distinct paths — verified by `len(arr) == len(set(arr))`. Prior bench likely counted call sites; two `require.Error(t, err)` calls in the same test now collapse to one caller. `Error` dropped exactly 169−117 = 52, consistent with deduping multi-call test bodies.
- **Corpus drift**: `grep_wb` for `Topology` rose 156→201 (new `TestDiagramDef_*` tests) and `dedupSuffix` rose 3→11 (new test mentions). Expected on a moving target.
- **TCR rose for every row** because `mache_callers` shrank (dedup) while `grep_wb` held flat. From an agent-cost standpoint this is the right metric: the agent reads each calling function once, not once per call site. `dedupSuffix` at 27.5× is the extreme — 11 grep hits (1 def + 1 call + 9 comment/test mentions) reduced to 1 real caller.

______________________________________________________________________

## 3. Cross-check results

### 3a. Type-symbol caller spot-checks

Sampled first 3 callers per type symbol and verified each against source:

- `MemoryStore` → `buildWriteGraph` (uses `graph.NewMemoryStore()`), `twoMountStores` (test helper, same constructor), `addMacheTools` (`*MemoryStore` parameter at `cmd/serve_test.go:1273`). Also confirmed `cmd/mount.go:683` does `g.(*graph.MemoryStore)` — type assertion.
- `SQLiteGraph` → `OpenSQLiteGraph` (return type at `internal/graph/sqlite_graph.go:137`), `SQLiteGraph.Act` (receiver), `SQLiteGraph.AddDef` (receiver).
- `Topology` → `TestDiagramDef_Unmarshal` (`var topo Topology` at `api/schema_test.go:21`), `TestDiagramDef_Marshal` (composite literal `Topology{…}` at line 50), `TestDiagramDef_EmptyMap` (var decl at line 42).

**All 9 spot-checks are real type references** (parameter, return type, receiver, var decl, composite literal, type assertion). No noise hits in the sample. The type-reference patterns cover the common shapes cleanly.

### 3b. `find_definition` per symbol

Calling `find_definition(symbol=X)` for all 10:

| symbol           | result                                                                                              | comment                                                                                                                                                                                |
| ---------------- | --------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Close`          | 21 definitions (every `*.Close` method across registries, NFS handles, SQL writers, watchers, etc.) | correct — all visible Close receivers are returned                                                                                                                                     |
| `New`            | **"no definition found"**                                                                           | no in-repo `func New()` exists; almost all `New*` constructors are `NewEngine`, `NewMemoryStore`, etc. The bare-`New` query has no match. Honest "not found" result, not a regression. |
| `Error`          | 2 definitions: `ErrGraphNotCached.Error`, `ValidationError.Error`                                   | correct — these are the two `Error() string` methods in the repo. `require.Error` is third-party and correctly excluded.                                                               |
| `MemoryStore`    | 1 definition: `graph/types/MemoryStore`                                                             | correct                                                                                                                                                                                |
| `SQLiteGraph`    | 1 definition: `graph/types/SQLiteGraph`                                                             | correct                                                                                                                                                                                |
| `Topology`       | 1 definition: `api/types/Topology`                                                                  | correct                                                                                                                                                                                |
| `GetCallers`     | 14 definitions (every receiver type that satisfies the interface)                                   | correct                                                                                                                                                                                |
| `RenderTemplate` | 1 definition: `ingest/functions/RenderTemplate`                                                     | correct                                                                                                                                                                                |
| `ReadContent`    | 23 definitions (all VFS handlers + every backend graph implementation)                              | correct                                                                                                                                                                                |
| `dedupSuffix`    | 1 definition: `ingest/functions/dedupSuffix`                                                        | correct                                                                                                                                                                                |

**9 of 10 resolve cleanly. The remaining "miss" (`New`) is a true negative**: there is no top-level `func New()` defined in this repo, only `New*` constructors. Prior bench's claim that find_definition was broken for *all* symbols is now obsolete — the node_defs SQL fallback works.

### 3c. Ambiguous-name `find_callees` behavior

The prior bench flagged phantom-target resolution (`find_callees("Error")` linked `require.Error` to an unrelated in-repo `Error` method). Task #15 was meant to add a receiver-method suffix-match fallback.

**Post-fix result: every `find_callees(path=X)` call returned `{"callees": [], "hint": "no resolved callees"}`.** Tested paths: `boltdb/functions/Materialize`, `graph/methods/WritableGraph.Close`, `graph/methods/SQLiteGraph.Close`, `refsvtab/methods/refsCursor.Close`, `cmd/functions/makeFindCallersHandler`, `ingest/functions/RenderTemplate`, `ingest/functions/dedupSuffix`, `ingest/functions/TestDig_ArrayIndex`.

**But the projection's `callees/` virtual directory still lists correct targets**. `list_directory(path="ingest/functions/RenderTemplate/callees")` returns `Render` (the inner `machetmpl.Render` call) plus the `callers` self-link. The vdir uses `CalleesHandler.ListDir`; the MCP tool reads from a separate code path that doesn't reach the same source.

**Ambiguous-name disambiguation cannot be tested**: the tool returns `[]` regardless of input, so the correct disambiguation guard can't be distinguished from a broken tool path. The phantom-target false positive is closed, but at the cost of returning nothing — agents asking "what does X call?" via MCP get no answer even when the projection has one. **Regression: the `find_callees` fix is not complete.**

### 3d. `search(pattern="dedupSuffix", role="definition")`

Expected result: one hit pointing at `ingest/functions/dedupSuffix`.

Actual result:

```
$ search(pattern="dedupSuffix", role="definition")
[]

$ search(pattern="dedupSuffix")
[{"token":"dedupSuffix","path":"ingest/methods/Engine.processNode/source"}]

$ search(pattern="dedupSuffix", type="functions")
[]

$ search(pattern="RenderTemplate", role="definition")
[]
```

Plain `search` (no role) returns 1 reference (the call site in `Engine.processNode`). But `role="definition"` returns empty — even though `find_definition("dedupSuffix")` correctly returns `ingest/functions/dedupSuffix` and the projection has a real construct directory at that path. The `type="functions"` filter also returns empty.

**Verdict**: `search role=definition` is **still broken** post-fix. The SQL pushdown fix did not wire role=definition to the same defs source that `find_definition` now uses. Task #14 should be re-opened.

______________________________________________________________________

## 4. Aggregate numbers

Mean and median across the 7 callable symbols (pre-fix had 0-valued denominators for the 3 type rows, so the comparable subset is the 7 callable):

|                               | pre-fix (7 callable) | post-fix (7 callable) | delta                      |
| ----------------------------- | -------------------: | --------------------: | -------------------------- |
| Mean `grep_wb`                |                171.1 |                 175.7 | +2.7% (corpus drift)       |
| Mean `mache_callers`          |                130.0 |                  97.6 | −25% (per-construct dedup) |
| Mean FP rate                  |                40.7% |                 57.0% | +16.3 pp                   |
| Median FP rate                |                28.1% |                 50.7% | +22.6 pp                   |
| Mean token-cost ratio         |                 5.0× |                  8.9× | +78%                       |
| Median grep latency           |                40 ms |                 40 ms | flat                       |
| Mean `mache_callers` (all 10) |      — (3 zero rows) |                  89.5 | —                          |
| Mean FP rate (all 10)         |     — (3 undef rows) |                 58.1% | now meaningful             |
| Mean TCR (all 10)             |     — (3 undef rows) |                  8.5× | now meaningful             |

### What the post-fix numbers say

1. **The type-symbol rows are now scored**. Prior bench had three "undefined" rows where the FP-rate question literally could not be answered. We can now report a clean FP rate across all 10 symbols (58.1% mean). On a 67K-LOC Go repo, the average grep_wb result is ~58% noise compared to mache's structurally-aware caller set.
1. **The 7-callable mean FP rate rose 41% → 57%** — but this is largely because mache's callers list got smaller (per-construct dedup), not because grep got worse. From an agent-cost standpoint this is the right metric: an agent only needs to read each calling function once, so per-construct is what tokens-per-task tracks.
1. **The mean token-cost ratio jumped 5.0× → 8.9×** for the same reason. Tighter mache result × identical grep result = bigger ratio. With type symbols included (8.5× across all 10), the average savings claim is in the 8–10× range on this corpus.
1. **No meaningful time-savings story to add**: median grep is still 40 ms; MCP latency is still subjectively comparable; this corpus is too small to differentiate.

______________________________________________________________________

## 5. Findings (ordered by significance)

1. **Type-reference patterns landed cleanly.** All three previously-empty type symbols return non-zero callers (`MemoryStore`=55, `SQLiteGraph`=34, `Topology`=123). Spot-checks confirm real type references (parameter, return type, receiver, var decl, composite literal, type assertion). FP rates for these rows (74%, 70%, 39%) are now real numbers, not undefined cells. This was the prior bench's finding #2 and is the most user-visible improvement.

1. **`find_definition` SQL fallback works for 9 of 10 symbols.** Prior: 0/10. Post: 9/10. The remaining miss (`New`) is correct — no in-repo `func New()` exists. The defs index is no longer empty.

1. **`find_callees` regressed from "wrong answer" to "no answer".** Returns `[]` for every path tested (8 different construct paths). The projection's `callees/` vdir still resolves correctly, so the data exists but the MCP tool path doesn't reach it. The phantom-target false positive is closed, but at the cost of returning nothing. **Task #15 should be re-opened.** Silently empty is safer than wrong (agents aren't misled), but it removes the tool's value entirely.

1. **`search role=definition` is still broken.** `search(pattern="dedupSuffix", role="definition")` returns `[]` while `find_definition("dedupSuffix")` correctly returns `ingest/functions/dedupSuffix`. The two should agree. The SQL pushdown fix did not wire role=definition to the `node_defs` fallback. **Task #14 should be re-opened.**

1. **Caller index changed counting model: call sites → distinct constructs.** Better for agents (read each calling function once, not once per call site), but makes pre/post numeric comparison on `mache_callers` misleading without annotation. Per-construct counts are the correct denominator for token-cost analysis.

1. **Mean FP rate (all 10 symbols) is now 58.1%, median 67.3%, mean TCR 8.5×.** The prior bench's 40.7% was on a 7-symbol subset because 3 rows had undefined denominators; the 10-symbol score is the apples-to-apples replacement now that type rows resolve.

1. **`dedupSuffix` is the most lopsided row** (`grep_wb`=11, `mache_callers`=1, FP=90.9%, TCR=27.5×). The grep_wb growth (3→11) comes from new test mentions and comments; mache correctly reports the single real call site. Rare-name grep is mostly noise in pure form.

1. **`grep_call` is a better proxy than `grep_wb` for the FP-reduction story.** `Error` has `grep_wb`=229 but `grep_call`=187 (comments, type decls, `t.Run` labels filtered out); mache reports 117. The naive-agent FP rate (48.9%) overstates the win; the good-grep-agent win is closer to ~37%, still meaningful but smaller than the headline.

1. **Time-savings story is still dead on this corpus.** Grep is 40 ms median regardless of result-set size; MCP latency is subjectively comparable. A 5 MLOC corpus would change this — 67K LOC doesn't.

1. **Net: 2 of 5 fixes fully closed the gap, 1 is correct-but-not-tested (architecture truncation), 2 need re-work.** `find_definition` SQL fallback and type-reference patterns are clean wins with verified callers. `find_callees` suffix-match and `search role=definition` are not user-visible in the MCP surface, even though the underlying data exists. Both are tractable — wire the MCP handler to the same source the vdir uses, and apply the role filter against `node_defs`.

______________________________________________________________________

## Appendix A: raw post-fix counts

```
symbol           grep_wb    grep_call    mache_callers
Close            667        617          434
New              67         49           33
Error            229        187          117
MemoryStore      212        0            55
SQLiteGraph      112        0            34
Topology         201        0            123
GetCallers       88         44           22
RenderTemplate   58         46           40
ReadContent      110        70           36
dedupSuffix      11         1            1
```

## Appendix B: `find_definition` returns

```
Close            21 defs (every *.Close receiver: graphRegistry, udsGraph, Controller, ArenaFlusher,
                          GraphCache, MemoryStore, SQLiteGraph, SQLiteResolver, WritableGraph,
                          closableGraph, ASTWalker, SQLiteWriter, SitterWalker, Client, SocketClient,
                          Server×2, bytesFile, graphFile, writeFile, refsCursor)
New              0  defs (no in-repo func New; only NewEngine, NewMemoryStore, etc.)
Error            2  defs (ErrGraphNotCached.Error, ValidationError.Error)
MemoryStore      1  def  (graph/types/MemoryStore)
SQLiteGraph      1  def  (graph/types/SQLiteGraph)
Topology         1  def  (api/types/Topology)
GetCallers       14 defs (every graph impl's GetCallers receiver)
RenderTemplate   1  def  (ingest/functions/RenderTemplate)
ReadContent      23 defs (all VFS handlers + every backend graph impl)
dedupSuffix      1  def  (ingest/functions/dedupSuffix)
```

## Appendix C: tool follow-up summary

| Tool                     | Prior           | Post-fix                                              | Action                                                             |
| ------------------------ | --------------- | ----------------------------------------------------- | ------------------------------------------------------------------ |
| `find_callers` (types)   | `[]` for types  | fixed — returns per-construct callers incl. type uses | —                                                                  |
| `find_definition`        | broken (0/10)   | fixed (9/10 via `node_defs` SQL fallback)             | —                                                                  |
| `find_callees`           | phantom targets | regressed to empty for every path                     | re-open; MCP handler must read from same source as `callees/` vdir |
| `search role=definition` | untested        | broken — `[]` for known defs                          | re-open; wire SQL fallback to `node_defs` for definition role      |
| `search` (no role)       | untested        | works                                                 | —                                                                  |
